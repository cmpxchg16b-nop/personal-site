package musicbot

// This file is the G.711 μ-law encoder: the bot's song is synthesized as
// 8 kHz mono PCM and encoded to μ-law, the WebRTC PCMU payload — every
// browser's audio stack offers PCMU, and unlike Opus it is trivially
// encodable in pure Go (companding is a table-free bit trick), so the bot
// carries no codec dependency.

import "math/bits"

// G.711 μ-law constants of the canonical ITU-T/Sun reference
// implementation: the segment bias and the clip threshold.
const (
	muLawBias = 0x84 // 132
	muLawClip = 32635
)

// linearToMuLaw encodes one 16-bit linear PCM sample as its μ-law byte:
// sign, the segment (exponent) of the biased magnitude, and the mantissa,
// complemented. (0 encodes to 0xFF, the μ-law positive zero.)
func linearToMuLaw(sample int16) byte {
	sign := 0
	s := int32(sample)
	if s < 0 {
		sign = 0x80
		s = -s
	}
	if s > muLawClip {
		s = muLawClip
	}
	s += muLawBias
	// After the bias the magnitude is at most 0x7FFF, so s>>7 fits a byte
	// and its highest set bit is the segment.
	exponent := 7 - bits.LeadingZeros8(uint8(s>>7))
	mantissa := (s >> (exponent + 3)) & 0x0F
	return byte(^(sign | (exponent << 4) | int(mantissa)))
}

// muLawEncode encodes a whole PCM buffer.
func muLawEncode(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, sample := range pcm {
		out[i] = linearToMuLaw(sample)
	}
	return out
}
