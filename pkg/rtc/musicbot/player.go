package musicbot

// This file is the player: one call's music source. A player owns the
// call's outbound media track (a webrtc.TrackLocalStaticSample — the
// microphone's counterpart for a bot, written with pre-encoded μ-law
// frames) and a pump goroutine feeding it one 20 ms frame per tick, so
// the RTP stream keeps real time. Switching the song mid-call is a local
// affair: the pump reads the current song's loop under a mutex — same
// track, same codec, no renegotiation.
//
// The pump is forgiving about the track's lifecycle: before the track is
// attached (a ringing call) or while a glare rebuild re-binds it, the
// track has no binding and WriteSample silently drops the frames — the
// song's position simply advances, as if the music played to an empty
// room. The pump stops on its stop channel (the call ended), on the
// session's context (the peer dropped), or on a write error.

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"personal-site/pkg/models/ss"
)

// The track's identity on the wire (the SDP msid); the codec is PCMU —
// every browser's WebRTC stack offers it, and the song is rendered at its
// native rate.
var musicTrackCodec = webrtc.RTPCodecCapability{
	MimeType:  webrtc.MimeTypePCMU,
	ClockRate: sampleRate,
	Channels:  1,
}

// player is one call's music source: the song currently playing and the
// track it plays onto.
type player struct {
	track *webrtc.TrackLocalStaticSample

	// mu guards the two fields the pump reads and /play writes.
	mu     sync.Mutex
	song   *song
	offset int // the next frame's start byte in the song's loop

	stopCh chan struct{}
	once   sync.Once
}

// newPlayer constructs a player starting at the beginning of s.
func newPlayer(s *song) (*player, error) {
	track, err := webrtc.NewTrackLocalStaticSample(musicTrackCodec, "music", "musicbot")
	if err != nil {
		return nil, err
	}
	return &player{track: track, song: s, stopCh: make(chan struct{})}, nil
}

// setSong switches the playing song, restarting it — the mid-call song
// switch.
func (p *player) setSong(s *song) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.song = s
	p.offset = 0
}

// frame returns the next frame of the current song's loop.
func (p *player) frame() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	frame := p.song.ulaw[p.offset : p.offset+samplesPerFrame]
	p.offset = (p.offset + samplesPerFrame) % len(p.song.ulaw)
	return frame
}

// run is the pump: one frame per frameDuration tick until the call ends
// (stop), the session ends (ctx), or the track fails.
func (p *player) run(ctx context.Context, logger *slog.Logger, peer ss.SubscriberId) {
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
		}
		err := p.track.WriteSample(media.Sample{Data: p.frame(), Duration: frameDuration})
		if err != nil {
			logger.Warn("musicbot: music track write failed; the player stops",
				"peer", peer, "err", err)
			return
		}
	}
}

// stop ends the pump; safe to call repeatedly.
func (p *player) stop() {
	p.once.Do(func() { close(p.stopCh) })
}
