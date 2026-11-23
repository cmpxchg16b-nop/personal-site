package sipregistration

import (
	"fmt"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

func inboundWireRequest(method sip.RequestMethod, recipient, via, callID string, sequence int, body string) string {
	extraHeaders := ""
	if method == sip.INVITE {
		extraHeaders = "Contact: <sip:caller@pbx.example>\r\nContent-Type: application/sdp\r\n"
	}
	return fmt.Sprintf("%s %s SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP %s\r\n"+
		"Max-Forwards: 70\r\n"+
		"From: <sip:caller@pbx.example>;tag=caller-tag\r\n"+
		"To: <sip:1001@127.0.0.1>\r\n"+
		"Call-ID: %s\r\n"+
		"CSeq: %d %s\r\n"+
		"%s"+
		"Content-Length: %d\r\n\r\n%s",
		method, recipient, via, callID, sequence, method, extraHeaders, len(body), body)
}

func readInboundWireResponse(t *testing.T, connection *net.UDPConn) *sip.Response {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 65535)
	count, err := connection.Read(buffer)
	if err != nil {
		t.Fatalf("read SIP response: %v", err)
	}
	message, err := sip.ParseMessage(buffer[:count])
	if err != nil {
		t.Fatalf("parse SIP response: %v\n%s", err, buffer[:count])
	}
	response, ok := message.(*sip.Response)
	if !ok {
		t.Fatalf("received non-response SIP message:\n%s", buffer[:count])
	}
	if response.CSeq() == nil || response.CallID() == nil {
		t.Fatalf("response lacks CSeq or Call-ID:\n%s", buffer[:count])
	}
	return response
}

func TestDiagoRuntimeInboundCancelOverUDP(t *testing.T) {
	registrar := newLoopbackRegistrar(t)
	manager := NewManager(slog.New(slog.DiscardHandler))
	manager.registrationExpiry = time.Minute
	manager.attemptTimeout = time.Second
	manager.unregisterTimeout = time.Second
	t.Cleanup(func() { _ = manager.Close() })
	created, err := manager.Create("user-alice", Input{
		Server: registrar.address, Username: "1001", Password: "secret", Transport: TransportUDP, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	registration := registrar.waitObservation(t, "register", 0)
	serviceWait(t, "inbound CANCEL test registration", func() bool {
		credential, _ := manager.Get("user-alice", created.ID)
		return credential.Registration.State == RegistrationRegistered
	})
	bridge := newServiceFakeBridge("incoming-delivery")
	service := NewService(manager, slog.New(slog.DiscardHandler))
	service.SetBridge(bridge)
	t.Cleanup(func() { _ = service.Close() })

	contact, err := sipUriForLookup(registration.contact)
	if err != nil {
		t.Fatalf("registered Contact URI: %v", err)
	}
	destination, err := net.ResolveUDPAddr("udp", registration.source)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := net.DialUDP("udp", nil, destination)
	if err != nil {
		t.Skipf("UDP loopback unavailable: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	recipient := (&sip.Uri{Scheme: "sip", User: "1001", Host: contact.Host, Port: contact.Port}).String()
	localAddress := connection.LocalAddr().String()
	const (
		sequence      = 1
		matchedCallID = "matched-cancel@pbx.example"
		unknownCallID = "unknown-cancel@pbx.example"
		matchedBranch = "z9hG4bK-matched-cancel"
		unknownBranch = "z9hG4bK-unknown-cancel"
	)
	invite := inboundWireRequest(sip.INVITE, recipient, localAddress+";branch="+matchedBranch, matchedCallID, sequence, serviceTestSIPAudio)
	if _, err := connection.Write([]byte(invite)); err != nil {
		t.Fatal(err)
	}
	for {
		response := readInboundWireResponse(t, connection)
		if response.CallID().Value() != matchedCallID || response.CSeq().MethodName != sip.INVITE {
			t.Fatalf("unexpected response while waiting for 180: %s", response.StartLine())
		}
		if response.StatusCode == sip.StatusRinging {
			break
		}
		if response.StatusCode >= 200 {
			t.Fatalf("INVITE reached final response before CANCEL: %s", response.StartLine())
		}
	}

	cancel := inboundWireRequest(sip.CANCEL, recipient, localAddress+";branch="+matchedBranch, matchedCallID, sequence, "")
	if _, err := connection.Write([]byte(cancel)); err != nil {
		t.Fatal(err)
	}
	var cancelOK, inviteTerminated bool
	for !cancelOK || !inviteTerminated {
		response := readInboundWireResponse(t, connection)
		if response.CallID().Value() != matchedCallID {
			t.Fatalf("unexpected Call-ID after matched CANCEL: %s", response.CallID().Value())
		}
		switch response.CSeq().MethodName {
		case sip.CANCEL:
			if response.StatusCode != sip.StatusOK {
				t.Fatalf("matched CANCEL status = %d, want 200", response.StatusCode)
			}
			cancelOK = true
		case sip.INVITE:
			if response.StatusCode >= 200 && response.StatusCode != sip.StatusRequestTerminated {
				t.Fatalf("cancelled INVITE status = %d, want 487", response.StatusCode)
			}
			if response.StatusCode == sip.StatusRequestTerminated {
				inviteTerminated = true
			}
		default:
			t.Fatalf("unexpected CSeq method after CANCEL: %s", response.CSeq().MethodName)
		}
	}
	bridge.waitEvent(t, "real UDP caller CANCEL", func(event serviceRecordedEvent) bool {
		return !event.fanout && event.deliveryID == "incoming-delivery" && event.call != nil &&
			event.call.Direction == "incoming" && event.call.State == "cancelled"
	})

	unknownCancel := inboundWireRequest(sip.CANCEL, recipient, localAddress+";branch="+unknownBranch, unknownCallID, sequence, "")
	if _, err := connection.Write([]byte(unknownCancel)); err != nil {
		t.Fatal(err)
	}
	for {
		response := readInboundWireResponse(t, connection)
		if response.CallID().Value() == matchedCallID {
			if response.CSeq().MethodName == sip.CANCEL {
				t.Fatalf("duplicate response to matched CANCEL: %s", response.StartLine())
			}
			continue // INVITE final responses may be retransmitted by UDP.
		}
		if response.CallID().Value() != unknownCallID || response.CSeq().MethodName != sip.CANCEL {
			t.Fatalf("unexpected response to unknown CANCEL: %s", response.StartLine())
		}
		if response.StatusCode != sip.StatusCallTransactionDoesNotExists {
			t.Fatalf("unknown CANCEL status = %d, want 481", response.StatusCode)
		}
		break
	}
}
