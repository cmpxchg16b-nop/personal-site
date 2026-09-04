package musicbot

// This file is the player: one call's music source. A player owns the
// call's outbound media track and a pump goroutine feeding it, so the
// RTP stream keeps real time — one 20 ms frame per tick for the sample
// families, the packets' own durations pacing the opus family.
//
// The music is a stream, not a buffer: the player reads each frame (or
// packet) from an open read of its audio source (lazily loaded on the
// player's creation — a source's bytes are never held as a whole), and
// whenever the read ends, the stream rewinds to its first sample and
// the song loops — for as long as the call lasts, whatever the source's
// length (a streaming source, of unknown length, restarts from its
// beginning just the same). The track's codec follows the source's
// format: μ-law bytes ride PCMU as they are; a linear PCM source rides
// opus, its samples normalized and encoded frame by frame; an opus
// source rides opus too, its packets passed through untouched — no
// decoding, no re-encoding, the packets the codec made going on the
// wire byte for byte.
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
// pcm, buf and pkts; the handler goroutine reaches it through switches.
type player struct {
	track *webrtc.TrackLocalStaticSample

	// in is the open source stream of the sample families (the μ-law
	// path reads it directly; norm adapts it for the linear pcm one);
	// pkts is the open source stream of the opus family, read as
	// packets. Exactly one of them is set: pkts for an opus source, in
	// for the others; enc is nil exactly then, too, save that the opus
	// family sets no encoder either — its packets need none.
	src  *audiosource.AudioSourceData
	in   loopingCloser
	pkts packetCloser
	norm *stereoNormalizer
	enc  *musicEncoder
	pcm  []int16   // one frame of interleaved s16 stereo samples
	fpcm []float32 // one frame of interleaved float stereo samples
	buf  []byte    // PCMU: the sample buffer; opus: the packet buffer

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
		ss, ok := st.(audiosource.SampleStream)
		if !ok {
			st.Close()
			return fmt.Errorf("musicbot: the mu-law stream serves no samples")
		}
		p.track = track
		p.in = ss
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
		ss, ok := st.(audiosource.SampleStream)
		if !ok {
			st.Close()
			return fmt.Errorf("musicbot: the linear pcm stream serves no samples")
		}
		p.track = track
		p.in = ss
		p.norm = newStereoNormalizer(ss, f.BitDepth, f.NumericType)
		p.enc = enc
		if f.NumericType == audiosource.NumFloat {
			p.fpcm = make([]float32, opusPCMPerFrame)
		} else {
			p.pcm = make([]int16, opusPCMPerFrame)
		}
		p.buf = make([]byte, opusMaxPacket)
	case audiosource.SampleOpus:
		// The packets are the payload: no normalizer, no encoder —
		// the track is the same opus one the linear pcm family encodes
		// toward, fed the codec's own output instead.
		ps, ok := st.(audiosource.PacketStream)
		if !ok {
			st.Close()
			return fmt.Errorf("musicbot: the opus stream serves no packets")
		}
		track, err := webrtc.NewTrackLocalStaticSample(opusTrackCodec, "music-opus", "musicbot")
		if err != nil {
			st.Close()
			return err
		}
		p.track = track
		p.pkts = ps
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
	if p.pkts != nil {
		if f.SampleFormatType != audiosource.SampleOpus {
			st.Close()
			return fmt.Errorf("musicbot: the switch would change the track's codec")
		}
		ps, ok := st.(audiosource.PacketStream)
		if !ok {
			st.Close()
			return fmt.Errorf("musicbot: the opus stream serves no packets")
		}
		_ = p.pkts.Close()
		p.pkts = ps
		p.src = src
		return nil
	}
	// The sample families: the new stream reads as samples, and the
	// linear pcm family's normalizer wraps it.
	if p.enc != nil {
		if f.SampleFormatType != audiosource.SampleLinearPCM {
			st.Close()
			return fmt.Errorf("musicbot: the switch would change the track's codec")
		}
	} else if f.SampleFormatType != audiosource.SampleMuLaw {
		st.Close()
		return fmt.Errorf("musicbot: the switch would change the track's codec")
	}
	ss, ok := st.(audiosource.SampleStream)
	if !ok {
		st.Close()
		return fmt.Errorf("musicbot: the stream serves no samples")
	}
	var norm *stereoNormalizer
	if p.enc != nil {
		norm = newStereoNormalizer(ss, f.BitDepth, f.NumericType)
		// The frame buffers follow the new song's numeric shape.
		if f.NumericType == audiosource.NumFloat {
			p.fpcm = make([]float32, opusPCMPerFrame)
			p.pcm = nil
		} else {
			p.pcm = make([]int16, opusPCMPerFrame)
			p.fpcm = nil
		}
	}
	_ = p.in.Close()
	p.in = ss
	p.norm = norm
	p.src = src
	if p.enc != nil {
		p.enc.reset()
	}
	return nil
}

// accepts reports whether the player can play src as a plain stream
// switch — the same codec family. The family follows the declared
// sample format type: a flac source always decodes to linear pcm, an
// ogg-framed one is opus, so their declarations decide their families
// too.
func (p *player) accepts(src *audiosource.AudioSourceData) bool {
	switch {
	case p.pkts != nil:
		return src.SampleFormatType == audiosource.SampleOpus
	case p.enc != nil:
		return src.SampleFormatType == audiosource.SampleLinearPCM
	default:
		return src.SampleFormatType == audiosource.SampleMuLaw
	}
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

// run is the pump until the call ends (stop), the session ends (ctx),
// or the stream fails: one frame per frameDuration tick for the sample
// families, the packets' own durations pacing the opus family's writes.
func (p *player) run(ctx context.Context, logger *slog.Logger, peer ss.SubscriberId) {
	defer p.closeIn()
	if p.pkts != nil {
		p.runPackets(ctx, logger, peer)
		return
	}
	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
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
	if p.fpcm != nil {
		if err := p.norm.readFrameFloat32(ctx, p.fpcm); err != nil {
			return err
		}
		n, err := p.enc.encodeFloat32(p.fpcm, p.buf)
		if err != nil {
			return err
		}
		return p.track.WriteSample(media.Sample{Data: p.buf[:n], Duration: frameDuration})
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

// runPackets is the opus family's pump: whole packets pass from the
// stream to the track byte for byte — no decoding, no re-encoding —
// each written with the duration its page's granule position measured,
// so the track's timestamps follow the music's own media time. The
// writes are paced to real time: no faster than the durations add up
// to, so a looping source plays at its own speed — while a live
// source, whose reads block on the network's arrival, runs at real
// time already and never waits.
func (p *player) runPackets(ctx context.Context, logger *slog.Logger, peer ss.SubscriberId) {
	var (
		played time.Duration // the media time written since the origin
		origin time.Time     // when the first packet went out
		timer  *time.Timer
	)
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		// Control messages are serviced between every two packets — a
		// self-paced source (a live stream, whose reads block until the
		// network delivers) never enters the pacing wait, so the wait's
		// select must not be the only door.
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case req := <-p.switches:
			// The switch is a fresh piece of music: the pacing restarts
			// with it.
			req.reply <- p.retune(ctx, req.src)
			origin, played = time.Time{}, 0
			continue
		default:
		}
		pkt, dur, err := readPacketLooping(ctx, p.pkts)
		if err != nil {
			logger.Warn("musicbot: the opus stream failed; the player stops",
				"peer", peer, "err", err)
			return
		}
		if origin.IsZero() {
			origin = time.Now()
		}
		if wait := origin.Add(played).Sub(time.Now()); wait > 0 {
			if timer == nil {
				timer = time.NewTimer(wait)
			} else {
				timer.Reset(wait)
			}
			select {
			case <-ctx.Done():
				return
			case <-p.stopCh:
				return
			case req := <-p.switches:
				// The switch is a fresh piece of music: the pacing and the
				// pending packet belong to the old one, so both go.
				req.reply <- p.retune(ctx, req.src)
				origin, played = time.Time{}, 0
				continue
			case <-timer.C:
			}
		}
		if err := p.track.WriteSample(media.Sample{Data: pkt, Duration: dur}); err != nil {
			logger.Warn("musicbot: the opus track write failed; the player stops",
				"peer", peer, "err", err)
			return
		}
		played += dur
	}
}

// closeIn drops the open stream; the pump's deferred cleanup.
func (p *player) closeIn() {
	if p.in != nil {
		_ = p.in.Close()
	}
	if p.pkts != nil {
		_ = p.pkts.Close()
	}
}

// stop ends the pump; safe to call repeatedly.
func (p *player) stop() {
	p.once.Do(func() { close(p.stopCh) })
}
