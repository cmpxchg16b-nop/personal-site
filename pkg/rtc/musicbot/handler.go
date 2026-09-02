package musicbot

// This file is the music bot's message policy — a
// msg_handler.BotMessageHandler. Everything protocol-shaped (the echo
// discipline, the transfer reassembly and acknowledgements, the call
// log's status amends) lives a layer down, in msg_handler's Server; what
// lives here is only what the messages MEAN to a music bot:
//
//   - chat is the bot's CLI (/help, /list-songs, /play),
//   - attachments are refused with a chat reply,
//   - a video call is declined, a voice call is accepted and answered
//     with the song,
//   - /play on a quiet line phones the user (the bot's own INVITE);
//     /play mid-call switches the song — a plain stream switch within
//     a codec family, a track replacement across families (the codec
//     follows the song).
//
// The songbook is injected (the Configuration's AudioSources — the
// configuration document's audioSource entries); it is static after
// construction, and the CLI addresses songs by their Name.
//
// State discipline: per-peer call state lives in a sync.Map keyed by the
// peer; a state's fields are touched only from the peer's serialized dcmsg
// invocations (the framework's documented guarantee) and from the session
// watcher, which only ever CompareAndDeletes — the hashReporter's
// discipline. The player is an actor synchronizing its own pump.

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/pion/webrtc/v4"

	"personal-site/pkg/models/audiosource"
	"personal-site/pkg/models/ss"
	"personal-site/pkg/rtc/msg_handler"
)

// The CLI's answer texts.
const (
	helpText = "Music bot commands:\n" +
		"/help — show this help\n" +
		"/list-songs — list the available songs\n" +
		"/play <song> — call you and play the song, or switch to it mid-call"
	attachmentRefusal = "Attachments are not supported — I'm a music bot. Try /help."
	unknownCommand    = "Unrecognized command — try /help."
	playUsage         = "Usage: /play <song> — see /list-songs."
	callDeclined      = "Call declined."
)

// callPhase is where a peer's call stands.
type callPhase int

const (
	// phaseRinging: the bot INVITEd and waits for the answer.
	phaseRinging callPhase = iota
	// phaseActive: the call is established; the music is on the wire.
	phaseActive
)

// peerCall is one peer's call state — touched only on the peer's
// serialized dcmsg goroutine and by the session watcher's
// CompareAndDelete.
type peerCall struct {
	outgoing bool // the bot opened the call (it is the caller)
	callId   string
	phase    callPhase
	player   *player
}

// musicHandler is the music bot's msg_handler.BotMessageHandler.
type musicHandler struct {
	logger *slog.Logger

	// The songbook: audio sources by name, and their listing order.
	// Static after construction.
	songs map[string]*audiosource.AudioSourceData
	order []string

	// calls is the per-peer call state. The map is shared across peers'
	// channel goroutines; the values are not (see the file's doc).
	calls sync.Map // ss.SubscriberId → *peerCall
}

var _ msg_handler.BotMessageHandler = (*musicHandler)(nil)

func newMusicHandler(logger *slog.Logger, sources []*audiosource.AudioSourceData) *musicHandler {
	h := &musicHandler{
		logger: logger,
		songs:  make(map[string]*audiosource.AudioSourceData, len(sources)),
	}
	for _, s := range sources {
		if _, dup := h.songs[s.Name]; dup {
			panic(fmt.Sprintf("musicbot: two songs named %q", s.Name))
		}
		h.songs[s.Name] = s
		h.order = append(h.order, s.Name)
	}
	return h
}

// HandleChatMessage is the CLI: parse the line, answer it.
func (h *musicHandler) HandleChatMessage(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter) {
	fields := strings.Fields(msg.Text)
	if len(fields) == 0 {
		h.say(msg.From, w, unknownCommand)
		return
	}
	switch fields[0] {
	case "/help":
		h.say(msg.From, w, helpText)
	case "/list-songs":
		h.say(msg.From, w, h.songListText())
	case "/play":
		h.play(ctx, msg, w, fields[1:])
	default:
		h.say(msg.From, w, unknownCommand)
	}
}

// HandleFileChunk does nothing: the Server owns the transfer mechanics
// (the stream is accepted and acknowledged either way); the refusal is
// spoken once, when the attachment completes.
func (h *musicHandler) HandleFileChunk(_ context.Context, _ *msg_handler.FileAnnouncement, _ *msg_handler.FileChunk, _ msg_handler.ResponseWriter) {
}

// HandleAttachment refuses the finished attachment: the music bot takes
// no files, of any kind.
func (h *musicHandler) HandleAttachment(_ context.Context, _ *msg_handler.FileAnnouncement, attachment *msg_handler.Attachment, w msg_handler.ResponseWriter) {
	if _, err := w.Reply(attachmentRefusal); err != nil {
		h.logger.Warn("musicbot: attachment refusal not sent", "peer", attachment.Peer, "err", err)
	}
}

// HandleCalling is the call policy.
func (h *musicHandler) HandleCalling(ctx context.Context, sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	switch {
	case sip.Method == msg_handler.SipMethodInvite:
		h.handleInvite(ctx, sip, w)
	case sip.Response != nil:
		h.handleResponse(sip, w)
	case sip.Method == msg_handler.SipMethodBye || sip.Method == msg_handler.SipMethodCancel:
		h.handleHangup(sip, w)
	}
}

// handleInvite answers an incoming call: video is declined, voice is
// accepted and answered with the song.
func (h *musicHandler) handleInvite(ctx context.Context, sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	peer := sip.From
	if sip.Media == msg_handler.MediaVideo {
		h.logger.Info("musicbot: declining video call", "peer", peer, "callId", sip.CallId)
		if err := w.Reject(msg_handler.SipCodeDecline, msg_handler.SipPhraseDecline); err != nil {
			h.logger.Warn("musicbot: call decline not sent", "peer", peer, "err", err)
		}
		return
	}
	if len(h.order) == 0 {
		// A songbook with no song has nothing to answer with; the
		// INVITE goes unanswered (the caller may cancel).
		h.logger.Warn("musicbot: no song to answer a voice call with", "peer", peer, "callId", sip.CallId)
		return
	}
	// A second INVITE from the same peer supersedes the state the bot
	// held: end the old call's media and take the new call.
	if old, ok := h.calls.Load(peer); ok {
		call := old.(*peerCall)
		call.player.stop()
		h.calls.CompareAndDelete(peer, call)
		if err := w.DetachMedia(call.player.track); err != nil {
			h.logger.Warn("musicbot: superseded call's track not detached", "peer", peer, "err", err)
		}
	}
	h.acceptCall(ctx, peer, sip.CallId, w)
}

// handleResponse folds the answer to one of the bot's own calls: on 200
// OK the prepared track goes on the wire; on a decline the call is
// dropped.
func (h *musicHandler) handleResponse(sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	peer := sip.From
	v, ok := h.calls.Load(peer)
	if !ok {
		return // an answer to a call the bot never opened (or already ended)
	}
	call := v.(*peerCall)
	if !call.outgoing || call.callId != sip.CallId {
		return
	}
	if sip.Response.Code == msg_handler.SipCodeOK {
		if call.phase != phaseRinging {
			return // a duplicate answer
		}
		if err := w.AttachMedia(call.player.track); err != nil {
			// The call stands answered but carries no music; the peer can
			// still hang up, which resets the state.
			h.logger.Warn("musicbot: attach on accepted call failed", "peer", peer, "err", err)
			return
		}
		call.phase = phaseActive
		h.logger.Info("musicbot: call accepted, playing", "peer", peer, "callId", call.callId)
		return
	}
	h.calls.CompareAndDelete(peer, call)
	call.player.stop()
	h.logger.Info("musicbot: call declined", "peer", peer, "callId", call.callId)
	h.say(peer, w, callDeclined)
}

// handleHangup ends the call the peer hung up (BYE) or aborted (CANCEL):
// the music stops, the track is withdrawn, the state is reset.
func (h *musicHandler) handleHangup(sip *msg_handler.SipMessage, w msg_handler.ResponseWriter) {
	peer := sip.From
	v, ok := h.calls.Load(peer)
	if !ok {
		return
	}
	call := v.(*peerCall)
	if call.callId != sip.CallId {
		return
	}
	h.calls.CompareAndDelete(peer, call)
	call.player.stop()
	if err := w.DetachMedia(call.player.track); err != nil {
		h.logger.Warn("musicbot: track not detached on hangup", "peer", peer, "err", err)
	}
	h.logger.Info("musicbot: call ended", "peer", peer, "callId", call.callId)
}

// play is the /play command: with no call it prepares the song and phones
// the user; mid-call it switches the song.
func (h *musicHandler) play(ctx context.Context, msg *msg_handler.ChatMessage, w msg_handler.ResponseWriter, args []string) {
	peer := msg.From
	if len(args) != 1 {
		h.say(msg.From, w, playUsage)
		return
	}
	s, ok := h.songs[args[0]]
	if !ok {
		h.say(msg.From, w, fmt.Sprintf("Unknown song %q — see /list-songs.", args[0]))
		return
	}
	if v, ok := h.calls.Load(peer); ok {
		call := v.(*peerCall)
		if err := h.switchSong(ctx, peer, call, s, w); err != nil {
			h.logger.Warn("musicbot: the song switch failed; the current song continues",
				"peer", peer, "song", s.Name, "err", err)
			h.say(msg.From, w, fmt.Sprintf("Could not switch to %q — the current song continues.", s.Name))
			return
		}
		if call.phase == phaseActive {
			h.say(msg.From, w, fmt.Sprintf("Now playing %q.", s.Name))
		} else {
			h.say(msg.From, w, fmt.Sprintf("Will play %q when you answer.", s.Name))
		}
		return
	}
	call, err := h.startCall(ctx, peer, s)
	if err != nil {
		h.say(msg.From, w, "Could not prepare the track — the call is off.")
		return
	}
	call.outgoing = true
	// Register for the peer's inbound media before the INVITE goes out,
	// so no early track is missed.
	w.OnTrack(h.drainTrack)
	callId, err := w.Invite(msg_handler.MediaVoice)
	if err != nil {
		h.calls.CompareAndDelete(peer, call)
		call.player.stop()
		h.say(msg.From, w, fmt.Sprintf("Could not call you: %v.", err))
		return
	}
	call.callId = callId
	h.say(msg.From, w, fmt.Sprintf("Calling you to play %q…", s.Name))
}

// switchSong points an ongoing call at another song. Within a codec
// family it is a stream switch inside the player; across families the
// track's codec changes with the song, so the whole player — track and
// all — is replaced: the new track is attached before the old one is
// detached, so the music never leaves the wire.
func (h *musicHandler) switchSong(ctx context.Context, peer ss.SubscriberId, call *peerCall, s *audiosource.AudioSourceData, w msg_handler.ResponseWriter) error {
	if call.player.accepts(s) {
		return call.player.setSource(s)
	}
	np, err := newPlayer(ctx, h.logger, peer, s)
	if err != nil {
		return err
	}
	if call.phase == phaseActive {
		if err := w.AttachMedia(np.track); err != nil {
			np.stop()
			return err
		}
		if err := w.DetachMedia(call.player.track); err != nil {
			h.logger.Warn("musicbot: the replaced track not detached", "peer", peer, "err", err)
		}
	}
	// While the call still rings, nothing is on the wire yet — the
	// answer will attach whatever track the call holds by then.
	call.player.stop()
	call.player = np
	return nil
}

// acceptCall takes an incoming voice call: a fresh player on the
// default song, the media answer (the track offer plus 200 OK), and the
// inbound-media registration.
func (h *musicHandler) acceptCall(ctx context.Context, peer ss.SubscriberId, callId string, w msg_handler.ResponseWriter) {
	s := h.songs[h.order[0]]
	call, err := h.startCall(ctx, peer, s)
	if err != nil {
		h.logger.Warn("musicbot: cannot prepare the track", "peer", peer, "err", err)
		return
	}
	call.callId = callId
	call.phase = phaseActive
	w.OnTrack(h.drainTrack)
	if err := w.Accept(call.player.track); err != nil {
		h.calls.CompareAndDelete(peer, call)
		call.player.stop()
		h.logger.Warn("musicbot: call accept failed", "peer", peer, "err", err)
		return
	}
	h.logger.Info("musicbot: voice call accepted, playing", "peer", peer, "callId", callId, "song", s.Name)
	h.say(peer, w, fmt.Sprintf("Now playing %q.", s.Name))
}

// startCall prepares one call's state: a fresh player on the song (its
// stream opens here — the source's lazily loaded data), the pump
// running, the state recorded, the session watcher armed.
func (h *musicHandler) startCall(ctx context.Context, peer ss.SubscriberId, s *audiosource.AudioSourceData) (*peerCall, error) {
	p, err := newPlayer(ctx, h.logger, peer, s)
	if err != nil {
		return nil, err
	}
	call := &peerCall{player: p}
	h.calls.Store(peer, call)
	// The session's end ends the call's music and drops the state. The
	// ctx survives a glare rebuild (it IS the session), so the music
	// plays on.
	go func() {
		<-ctx.Done()
		call.player.stop()
		h.calls.CompareAndDelete(peer, call)
	}()
	return call, nil
}

// drainTrack is the inbound-media callback: the bot has no use for the
// peer's audio, but an unread track's packets would pile up in the
// receiver — so they are read and discarded.
func (h *musicHandler) drainTrack(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
	h.logger.Info("musicbot: inbound track; draining",
		"kind", track.Kind(), "codec", track.Codec().MimeType)
	go func() {
		for {
			if _, _, err := track.ReadRTP(); err != nil {
				return // the track or the connection ended
			}
		}
	}()
}

// songListText is the /list-songs answer.
func (h *musicHandler) songListText() string {
	var b strings.Builder
	b.WriteString("Available songs:")
	for _, name := range h.order {
		fmt.Fprintf(&b, "\n- %s — %s", name, h.songs[name].Description)
	}
	return b.String()
}

// say answers the peer with a chat message, logging a failed send.
func (h *musicHandler) say(peer ss.SubscriberId, w msg_handler.ResponseWriter, text string) {
	if _, err := w.Reply(text); err != nil {
		h.logger.Warn("musicbot: reply not sent", "peer", peer, "err", err)
	}
}
