package sipregistration

import (
	"github.com/emiago/diago"
	"github.com/emiago/sipgo/sip"
)

const statusTemporarilyUnavailable = 480

// handleInboundInvite preserves the registration-only behavior used by legacy
// isolated tests. Runtime listeners use handleRuntimeInboundInvite below.
func (m *Manager) handleInboundInvite(dialog *diago.DialogServerSession) {
	m.handleRuntimeInboundInvite(nil, dialog)
}

// handleRuntimeInboundInvite delivers only when the receiving runtime, To AOR,
// and currently leased credential generation all match. In particular, an old
// listener cannot ring browsers through a replacement credential generation.
func (m *Manager) handleRuntimeInboundInvite(runtime *diagoRuntime, dialog *diago.DialogServerSession) {
	request := dialog.InviteRequest
	credential, matched := m.resolveInbound("INVITE", request)
	status := sip.StatusNotImplemented
	phrase := "Not Implemented"
	var service *Service
	var leased Credential
	var operations operationRuntime
	var revision uint64
	var live bool
	if !matched {
		status = sip.StatusNotFound
		phrase = "Not Found"
	} else if !credential.Enabled {
		status = statusTemporarilyUnavailable
		phrase = "Temporarily Unavailable"
	} else {
		m.mu.RLock()
		service = m.service
		m.mu.RUnlock()
		if service == nil {
			// 501 is reserved for a registration-only manager with no Service.
			status = sip.StatusNotImplemented
			phrase = "Not Implemented"
		} else {
			leased, operations, revision, live = m.leaseForRuntime(runtime)
			if !live || leased.ID != credential.ID || leased.OwnerID != credential.OwnerID || leased.SIPURI != credential.SIPURI || leased.SIPURI != inboundAOR(request) {
				status = statusTemporarilyUnavailable
				phrase = "Temporarily Unavailable"
			} else {
				service.serveInboundInvite(leased, operations, revision, &diagoInboundDialog{DialogServerSession: dialog})
				return
			}
		}
	}
	if err := dialog.Respond(status, phrase, nil); err != nil {
		m.logger.Warn("respond to inbound SIP INVITE failed", "aor", inboundAOR(request), "err", err)
	}
}

// handleInboundMessage preserves the registration-only behavior used by legacy
// isolated tests. Runtime listeners use handleRuntimeInboundMessage below.
func (m *Manager) handleInboundMessage(request *sip.Request, transaction sip.ServerTransaction) error {
	return m.handleRuntimeInboundMessage(nil, request, transaction)
}

func (m *Manager) handleRuntimeInboundMessage(runtime *diagoRuntime, request *sip.Request, transaction sip.ServerTransaction) error {
	credential, matched := m.resolveInbound("MESSAGE", request)
	status := sip.StatusNotImplemented
	phrase := "Not Implemented"
	var headers []sip.Header
	if !matched {
		status = sip.StatusNotFound
		phrase = "Not Found"
	} else if !credential.Enabled {
		status = statusTemporarilyUnavailable
		phrase = "Temporarily Unavailable"
	} else {
		m.mu.RLock()
		service := m.service
		m.mu.RUnlock()
		if service == nil {
			status = sip.StatusNotImplemented
			phrase = "Not Implemented"
		} else {
			leased, _, _, live := m.leaseForRuntime(runtime)
			if !live || leased.ID != credential.ID || leased.OwnerID != credential.OwnerID || leased.SIPURI != credential.SIPURI || leased.SIPURI != inboundAOR(request) {
				status = statusTemporarilyUnavailable
				phrase = "Temporarily Unavailable"
			} else {
				status, phrase, headers = service.handleInboundMessage(leased, request)
			}
		}
	}
	response := sip.NewResponseFromRequest(request, status, phrase, nil)
	for _, header := range headers {
		response.AppendHeader(header)
	}
	return transaction.Respond(response)
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
