package musicbot

// This file is the player: one call's music source. A player owns the
// call's outbound media track and a pump goroutine feeding it one 20 ms
// frame per tick, so the RTP stream keeps real time.
//
// The music is a stream, not a buffer: the player reads each frame from
// an open read of its audio source (lazily loaded on the player's
// creation — a source's bytes are never held as a whole), and whenever
// the read ends, the stream rewinds to its first sample and the song
// loops — for as long as the call lasts, whatever the source's length
// (a streaming source, of unknown length, restarts from its beginning
// just the same). The track's codec follows the source's format: μ-law
// bytes ride PCMU as they are; a linear PCM source rides opus, its
// samples normalized and encoded frame by frame.
//
// The player is an actor: the pump goroutine alone touches the open
// stream and the encoder; a mid-call song switch of the same codec
// family travels to it as a request on the switches channel and is
// applied between frames (the switch reply carries the error). A switch
// across codec families cannot reuse the track — the handler replaces
// the whole player instead (see its switchSong).
//
// The pump is forgiving about the track's lifecycle: before the track
// is attached (a ringing call) or while a glare rebuild re-binds it,
// the track has no binding and WriteSample silently drops the frames —
// the stream's position simply advances, as if the music played to an
// empty room. The pump stops on its stop channel (the call ended), on
// the session's context (the peer dropped), or on a stream error.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"

	"personal-site/pkg/models/audiosource"
	"personal-site/pkg/models/ss"
)

// The PCMU path's shape: 8 kHz mono μ-law — G.711's native rate, one
// companded byte per sample, the payload byte for byte. The opus
// path's shape: 48 kHz stereo, one 20 ms frame per tick.
const (
	sampleRate      = 8000
	frameDuration   = 20 * time.Millisecond
	samplesPerFrame = sampleRate / 50 // one frameDuration of samples: 160

	opusSampleRate      = 48000
	opusChannels        = 2
	opusSamplesPerFrame = opusSampleRate / 50                // per channel: 960
	opusPCMPerFrame     = opusSamplesPerFrame * opusChannels // interleaved samples: 1920
	opusMaxPacket       = 4000                               // one encoded frame's maximum bytes
)

// switchReq carries one mid-call song switch to the pump: the new
// source, and where to answer with the outcome.
type switchReq struct {
	src   *audiosource.AudioSourceData
	reply chan error
}

// errPlayerStopped is what a switch on a stopped player answers.
var errPlayerStopped = errors.New("musicbot: the player has stopped")

// player is one call's music source: the track its music rides, and
// the pump feeding it. The pump goroutine alone touches in, norm, enc,
// pcm and buf; the handler goroutine reaches it through switches.
type player struct {
	track *webrtc.TrackLocalStaticSample

	// in is the open source stream (the μ-law path reads it directly);
	// norm adapts it for the opus path; enc is nil exactly then.
	src  *audiosource.AudioSourceData
	in   loopingCloser
	norm *stereoNormalizer
	enc  *musicEncoder
	pcm  []int16 // one frame of interleaved s16 stereo samples
	buf  []byte  // PCMU: the sample buffer; opus: the packet buffer

	switches chan switchReq
	stopCh   chan struct{}
	once     sync.Once
}

// loopingCloser is an open source stream: readable, rewindable,
// closable.
type loopingCloser interface {
	loopingSource
	io.Closer
}

// newPlayer prepares one call's music: an open stream of the source
// (lazily loaded here — the source's first touch), the track its codec
// dictates, and the pump running.
func newPlayer(ctx context.Context, logger *slog.Logger, peer ss.SubscriberId, src *audiosource.AudioSourceData) (*player, error) {
	p := &player{
		switches: make(chan switchReq),
		stopCh:   make(chan struct{}),
	}
	if err := p.tune(ctx, src); err != nil {
		return nil, err
	}
	go p.run(ctx, logger, peer)
	return p, nil
}

// tune opens the source's stream and creates the track and the codecs
// its format dictates. It runs before the pump starts.
func (p *player) tune(ctx context.Context, src *audiosource.AudioSourceData) error {
	st, err := src.Open(ctx)
	if err != nil {
		return err
	}
	f := st.Format()
	switch f.SampleFormatType {
	case audiosource.SampleMuLaw:
		track, err := webrtc.NewTrackLocalStaticSample(pcmuTrackCodec, "music-pcmu", "musicbot")
		if err != nil {
			st.Close()
			return err
		}
		p.track = track
		p.in = st
		p.norm = nil
		p.enc = nil
		p.buf = make([]byte, samplesPerFrame)
	case audiosource.SampleLinearPCM:
		enc, err := newMusicEncoder(f.SampleRate, f.NumChannels)
		if err != nil {
			st.Close()
			return err
		}
		track, err := webrtc.NewTrackLocalStaticSample(opusTrackCodec, "music-opus", "musicbot")
		if err != nil {
			st.Close()
			return err
		}
		p.track = track
		p.in = st
		p.norm = newStereoNormalizer(st, f.BitDepth, f.NumericType)
		p.enc = enc
		p.pcm = make([]int16, opusPCMPerFrame)
		p.buf = make([]byte, opusMaxPacket)
	default:
		st.Close()
		return fmt.Errorf("musicbot: the stream's sample format %q is not playable", f.SampleFormatType)
	}
	p.src = src
	return nil
}

// retune points the player at another song of the same codec family
// (the handler checks accepts before asking): the new stream opens
// first, so a failed switch leaves the current song playing. Runs on
// the pump goroutine.
func (p *player) retune(ctx context.Context, src *audiosource.AudioSourceData) error {
	st, err := src.Open(ctx)
	if err != nil {
		return err
	}
	f := st.Format()
	var norm *stereoNormalizer
	if p.enc != nil {
		if f.SampleFormatType != audiosource.SampleLinearPCM {
			st.Close()
			return fmt.Errorf("musicbot: the switch would change the track's codec")
		}
		norm = newStereoNormalizer(st, f.BitDepth, f.NumericType)
	} else if f.SampleFormatType != audiosource.SampleMuLaw {
		st.Close()
		return fmt.Errorf("musicbot: the switch would change the track's codec")
	}
	_ = p.in.Close()
	p.in = st
	p.norm = norm
	p.src = src
	if p.enc != nil {
		p.enc.reset()
	}
	return nil
}

// accepts reports whether the player can play src as a plain stream
// switch — the same codec family. The family follows the declared
// sample format type: a flac source always decodes to linear pcm, so
// its declaration decides its family too.
func (p *player) accepts(src *audiosource.AudioSourceData) bool {
	if p.enc == nil {
		return src.SampleFormatType == audiosource.SampleMuLaw
	}
	return src.SampleFormatType == audiosource.SampleLinearPCM
}

// setSource asks the pump to switch to src (of the same codec family;
// see accepts) and returns its outcome.
func (p *player) setSource(src *audiosource.AudioSourceData) error {
	reply := make(chan error, 1)
	select {
	case p.switches <- switchReq{src: src, reply: reply}:
	case <-p.stopCh:
		return errPlayerStopped
	}
	select {
	case err := <-reply:
		return err
	case <-p.stopCh:
		return errPlayerStopped
	}
}

// run is the pump: one frame per frameDuration tick until the call ends
// (stop), the session ends (ctx), or the stream fails.
func (p *player) run(ctx context.Context, logger *slog.Logger, peer ss.SubscriberId) {
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	defer p.closeIn()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case req := <-p.switches:
			req.reply <- p.retune(ctx, req.src)
		case <-ticker.C:
			if err := p.tick(ctx); err != nil {
				logger.Warn("musicbot: the music stream failed; the player stops",
					"peer", peer, "err", err)
				return
			}
		}
	}
}

// tick produces and writes one frame of music.
func (p *player) tick(ctx context.Context) error {
	if p.enc == nil {
		// The PCMU path: the μ-law bytes are the payload.
		if err := readLooping(ctx, p.in, p.buf); err != nil {
			return err
		}
		return p.track.WriteSample(media.Sample{Data: p.buf, Duration: frameDuration})
	}
	if err := p.norm.readFrame(ctx, p.pcm); err != nil {
		return err
	}
	n, err := p.enc.encode(p.pcm, p.buf)
	if err != nil {
		return err
	}
	return p.track.WriteSample(media.Sample{Data: p.buf[:n], Duration: frameDuration})
}

// closeIn drops the open stream; the pump's deferred cleanup.
func (p *player) closeIn() {
	if p.in != nil {
		_ = p.in.Close()
	}
}

// stop ends the pump; safe to call repeatedly.
func (p *player) stop() {
	p.once.Do(func() { close(p.stopCh) })
}
