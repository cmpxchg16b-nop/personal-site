package ss

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/pion/webrtc/v4"
)

func TestSIPWireJSONShape(t *testing.T) {
	if WellKnownSvcIdSIP != "6ad2a70f-61e6-4b0b-85a0-e028f8f0a65b" {
		t.Fatalf("WellKnownSvcIdSIP = %q", WellKnownSvcIdSIP)
	}
	fullCall := &SIPCallEvent{
		CallId:          "call-1",
		CredentialId:    "credential-1",
		Target:          "sip:bob@example.com",
		Kind:            "video",
		Action:          "dial",
		Direction:       "outgoing",
		State:           "dialing",
		LocalUri:        "sip:alice@example.com",
		RemoteUri:       "sip:bob@example.com",
		Reason:          "testing",
		SessionDesc:     &webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: "v=0\r\n"},
		RTCICECandidate: &webrtc.ICECandidateInit{Candidate: "candidate:1"},
	}
	fullMessage := &SIPMessageEvent{
		MessageId:    "message-1",
		CredentialId: "credential-1",
		Target:       "sip:bob@example.com",
		Text:         "hello",
		Action:       "send",
		Direction:    "incoming",
		State:        "received",
		LocalUri:     "sip:alice@example.com",
		RemoteUri:    "sip:bob@example.com",
		Reason:       "testing",
		CreatedAt:    1764000000.25,
	}

	for _, tc := range []struct {
		name  string
		event any
		key   string
		want  map[string]any
	}{
		{
			name:  "client call with all fields",
			event: &ClientToSSEv{SIPCall: fullCall},
			key:   "sipCall",
			want: map[string]any{
				"callId": "call-1", "credentialId": "credential-1", "target": "sip:bob@example.com",
				"kind": "video", "action": "dial", "direction": "outgoing", "state": "dialing",
				"localUri": "sip:alice@example.com", "remoteUri": "sip:bob@example.com", "reason": "testing",
				"sessionDesc": map[string]any{"type": "offer", "sdp": "v=0\r\n"},
				"rtcICECandidate": map[string]any{
					"candidate": "candidate:1", "sdpMid": nil, "sdpMLineIndex": nil, "usernameFragment": nil,
				},
			},
		},
		{
			name:  "server call",
			event: &SSToClientEv{SIPCall: &SIPCallEvent{CallId: "call-2", State: "accepted"}},
			key:   "sipCall",
			want:  map[string]any{"callId": "call-2", "state": "accepted"},
		},
		{
			name:  "client message",
			event: &ClientToSSEv{SIPMessage: &SIPMessageEvent{MessageId: "message-2", Action: "send"}},
			key:   "sipMessage",
			want:  map[string]any{"messageId": "message-2", "action": "send"},
		},
		{
			name:  "server message with all fields",
			event: &SSToClientEv{SIPMessage: fullMessage},
			key:   "sipMessage",
			want: map[string]any{
				"messageId": "message-1", "credentialId": "credential-1", "target": "sip:bob@example.com",
				"text": "hello", "action": "send", "direction": "incoming", "state": "received",
				"localUri": "sip:alice@example.com", "remoteUri": "sip:bob@example.com", "reason": "testing",
				"createdAt": 1764000000.25,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.event)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got map[string]map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", data, err)
			}
			if len(got) != 1 || !reflect.DeepEqual(got[tc.key], tc.want) {
				t.Fatalf("wire shape = %s, want %s = %#v", data, tc.key, tc.want)
			}
		})
	}
}

func TestSIPWireOptionalFields(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event any
		want  string
	}{
		{name: "call id remains required", event: &SIPCallEvent{}, want: `{"callId":""}`},
		{name: "message id remains required", event: &SIPMessageEvent{}, want: `{"messageId":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.event)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if string(data) != tc.want {
				t.Fatalf("wire shape = %s, want %s", data, tc.want)
			}
		})
	}
}
