package sipregistration

import (
	"github.com/emiago/diago"
	"github.com/emiago/sipgo/sip"
)

const statusTemporarilyUnavailable = 480

// handleInboundInvite terminates SIP at the server. A later milestone will
// translate a matched INVITE to the site's own SS events and fan it out through
// the CAM table to every active WebRTC UA of Credential.OwnerID. This milestone
// deliberately rejects the call instead of pretending that delivery exists.
func (m *Manager) handleInboundInvite(dialog *diago.DialogServerSession) {
	request := dialog.InviteRequest
	credential, matched := m.resolveInbound("INVITE", request)
	status := sip.StatusNotImplemented
	phrase := "Not Implemented"
	if !matched {
		status = sip.StatusNotFound
		phrase = "Not Found"
	} else if !credential.Enabled {
		status = statusTemporarilyUnavailable
		phrase = "Temporarily Unavailable"
	}
	if err := dialog.Respond(status, phrase, nil); err != nil {
		m.logger.Warn("respond to inbound SIP INVITE failed", "aor", inboundAOR(request), "err", err)
	}
}

// handleInboundMessage applies the same AOR ownership check as INVITE. MESSAGE
// translation and delivery are intentionally outside the registration-only
// milestone; unknown recipients are always logged and answered 404.
func (m *Manager) handleInboundMessage(request *sip.Request, transaction sip.ServerTransaction) error {
	credential, matched := m.resolveInbound("MESSAGE", request)
	status := sip.StatusNotImplemented
	phrase := "Not Implemented"
	if !matched {
		status = sip.StatusNotFound
		phrase = "Not Found"
	} else if !credential.Enabled {
		status = statusTemporarilyUnavailable
		phrase = "Temporarily Unavailable"
	}
	return transaction.Respond(sip.NewResponseFromRequest(request, status, phrase, nil))
}

func (m *Manager) resolveInbound(method string, request *sip.Request) (Credential, bool) {
	aor := inboundAOR(request)
	if aor == "" {
		m.logger.Warn("inbound SIP signaling has no usable To AOR", "method", method, "request_uri", request.Recipient.String(), "source", request.Source())
		return Credential{}, false
	}
	credential, ok := m.LookupAOR(aor)
	if !ok {
		m.logger.Warn("unmatched inbound SIP signaling: no user for SIP URI", "method", method, "aor", aor, "request_uri", request.Recipient.String(), "source", request.Source())
		return Credential{}, false
	}
	m.logger.Info("inbound SIP signaling resolved to user", "method", method, "aor", aor, "user_id", credential.OwnerID, "credential_id", credential.ID, "enabled", credential.Enabled, "registration_state", credential.Registration.State, "source", request.Source())
	return credential, true
}

func inboundAOR(request *sip.Request) string {
	if request == nil || request.To() == nil || request.To().Address.User == "" || request.To().Address.Host == "" {
		return ""
	}
	return canonicalAOR(request.To().Address.User, request.To().Address.Host)
}
