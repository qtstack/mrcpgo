package server

import (
	"errors"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// allowMethods is advertised in OPTIONS responses.
const allowMethods = "INVITE, ACK, BYE, CANCEL, OPTIONS"

// onInvite handles an inbound INVITE: it allocates media resources, builds the
// SDP answer and answers 200 OK, which blocks until the ACK confirms the
// dialog.
func (s *Server) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	callID := callIDOf(req)
	log := s.log.With("call_id", callID, "sip_method", "INVITE", "source", req.Source())
	log.Info("INVITE received")

	if callID == "" {
		log.Warn("INVITE without Call-ID")
		s.respondStateless(tx, sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	dlg, err := s.dialogUA.ReadInvite(req, tx)
	if err != nil {
		log.Warn("cannot create dialog", "error", err)
		s.respondStateless(tx, sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}
	s.trackDialog(callID, dlg)

	if err := dlg.Respond(sip.StatusTrying, "Trying", nil); err != nil {
		log.Warn("failed to send 100 Trying", "error", err)
		return
	}

	answer, sess, host, code, err := s.negotiate(req, dlg, callID, log)
	if err != nil {
		log.Warn("INVITE rejected", "status", code, "error", err)
		// A non-2xx final response terminates the dialog on its own.
		if rerr := dlg.Respond(code, statusReason(code), nil); rerr != nil {
			log.Debug("failed to send failure response", "error", rerr)
		}
		return
	}

	if dlg.LoadState() == sip.DialogStateEnded {
		// CANCEL arrived while resources were being allocated.
		log.Info("dialog already terminated, releasing resources")
		if err := sess.Close(errors.New("dialog terminated before answer")); err != nil {
			log.Debug("session close failed", "error", err)
		}
		return
	}

	contact := sip.ContactHeader{
		Address: sip.Uri{User: s.cfg.SIP.ContactUser, Host: host, Port: s.contactPort()},
	}
	contentType := sip.ContentTypeHeader("application/sdp")

	// Respond blocks until the ACK is processed, which is what keeps the
	// INVITE transaction alive on UDP. Each request is handled in its own
	// goroutine, so this does not stall other calls.
	if err := dlg.Respond(sip.StatusOK, "OK", answer, &contact, &contentType); err != nil {
		log.Warn("INVITE answer failed", "error", err)
		if cerr := sess.Close(errors.New("dialog not confirmed")); cerr != nil {
			log.Debug("session close failed", "error", cerr)
		}
		return
	}

	if sess.Activate() {
		log.Info("call established",
			"channel_count", sess.ChannelCount(),
			"rtp_port", sess.RTP().LocalAddr().String())
	}
}

// onAck confirms the dialog, which unblocks the pending 200 OK.
func (s *Server) onAck(req *sip.Request, tx sip.ServerTransaction) {
	callID := callIDOf(req)
	dlg, ok := s.dialog(callID)
	if !ok {
		// ACK for a non-2xx response has no dialog to confirm.
		s.log.Debug("ACK for unknown dialog", "call_id", callID)
		return
	}
	if dlg.LoadState() != sip.DialogStateEstablished {
		return
	}
	if err := dlg.ReadAck(req, tx); err != nil {
		s.log.Warn("ACK rejected", "call_id", callID, "error", err)
	}
}

// onBye terminates the dialog and therefore the session.
func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	callID := callIDOf(req)
	log := s.log.With("call_id", callID, "sip_method", "BYE")
	log.Info("BYE received")

	dlg, ok := s.dialog(callID)
	if !ok {
		log.Warn("BYE for unknown dialog")
		s.respondStateless(tx, sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists,
			"Call/Transaction Does Not Exist", nil))
		return
	}

	// ReadBye validates the CSeq, answers 200 OK itself and terminates the
	// dialog. Do not respond again here.
	if err := dlg.ReadBye(req, tx); err != nil {
		log.Warn("BYE rejected", "error", err)
		s.respondStateless(tx, sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists,
			"Call/Transaction Does Not Exist", nil))
	}
}

// onCancel only sees CANCEL requests that could not be matched to an INVITE
// transaction; matched CANCELs are answered by the transaction layer with 200
// OK and 487 Request Terminated.
func (s *Server) onCancel(req *sip.Request, tx sip.ServerTransaction) {
	s.log.Info("unmatched CANCEL received", "call_id", callIDOf(req))
	s.respondStateless(tx, sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists,
		"Call/Transaction Does Not Exist", nil))
}

// respondStateless writes a response outside the dialog layer and logs failures
// at debug level. A failure here usually means the peer already gave up, so it
// is not worth a warning.
func (s *Server) respondStateless(tx sip.ServerTransaction, res *sip.Response) {
	if err := tx.Respond(res); err != nil {
		s.log.Debug("failed to send SIP response", "status", res.StatusCode, "error", err)
	}
}

// onOptions advertises the supported methods and media types.
func (s *Server) onOptions(req *sip.Request, tx sip.ServerTransaction) {
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	res.AppendHeader(sip.NewHeader("Allow", allowMethods))
	res.AppendHeader(sip.NewHeader("Accept", "application/sdp"))
	if err := tx.Respond(res); err != nil {
		s.log.Warn("failed to answer OPTIONS", "error", err)
	}
}

func (s *Server) trackDialog(callID string, dlg *sipgo.DialogServerSession) {
	s.dialogs.Store(callID, dlg)
	dlg.OnState(func(state sip.DialogState) {
		if state == sip.DialogStateEnded {
			s.dialogs.Delete(callID)
		}
	})
}

func (s *Server) dialog(callID string) (*sipgo.DialogServerSession, bool) {
	v, ok := s.dialogs.Load(callID)
	if !ok {
		return nil, false
	}
	dlg, ok := v.(*sipgo.DialogServerSession)
	return dlg, ok
}

func callIDOf(req *sip.Request) string {
	if h := req.CallID(); h != nil {
		return h.Value()
	}
	return ""
}

func statusReason(code int) string {
	switch code {
	case sip.StatusNotAcceptableHere:
		return "Not Acceptable Here"
	case sip.StatusUnsupportedMediaType:
		return "Unsupported Media Type"
	case sip.StatusInternalServerError:
		return "Server Internal Error"
	case sip.StatusBadRequest:
		return "Bad Request"
	default:
		return "Error"
	}
}
