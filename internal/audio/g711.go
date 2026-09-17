package audio

// G.711 companding, ITU-T A-law and mu-law.
//
// The implementation follows the reference algorithm from ITU-T G.711 (the
// widely distributed Sun Microsystems g711.c). Encode paths run the algorithm
// directly; decode paths are precomputed into 256-entry tables at init because
// decoding is on the hot path for every received packet.

const (
	// muBias is added to the 14-bit magnitude before segment search.
	muBias = 0x84
	// muClip is the largest 14-bit magnitude mu-law can represent.
	muClip = 8159
	// aClip is the largest 12-bit magnitude A-law can represent.
	aClip = 4095
)

// segEndMu and segEndA are the upper bounds of each of the eight
// piecewise-linear segments, in the encoder's shifted domain.
var (
	segEndMu = [8]int16{0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF, 0x1FFF}
	segEndA  = [8]int16{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}
)

var (
	mulawToPCM [256]int16
	alawToPCM  [256]int16
)

func init() {
	for i := 0; i < 256; i++ {
		mulawToPCM[i] = decodeMulaw(byte(i))
		alawToPCM[i] = decodeAlaw(byte(i))
	}
}

// segmentSearch returns the index of the first segment whose bound is >= val.
func segmentSearch(val int16, bounds *[8]int16) int {
	for i := 0; i < len(bounds); i++ {
		if val <= bounds[i] {
			return i
		}
	}
	return len(bounds)
}

// EncodeMulawSample compands one 16-bit linear sample to a mu-law octet.
func EncodeMulawSample(pcm int16) byte {
	// Work in the 14-bit domain the standard defines.
	v := pcm >> 2
	var mask int16
	if v < 0 {
		v = -v
		mask = 0x7F
	} else {
		mask = 0xFF
	}
	if v > muClip {
		v = muClip
	}
	v += muBias >> 2

	seg := segmentSearch(v, &segEndMu)
	if seg >= 8 {
		return byte(0x7F ^ mask)
	}
	val := byte(seg<<4) | byte((v>>(seg+1))&0xF)
	return val ^ byte(mask)
}

// decodeMulaw expands one mu-law octet to a 16-bit linear sample.
func decodeMulaw(u byte) int16 {
	u = ^u
	t := int16(u&0x0F)<<3 + muBias
	t <<= (u & 0x70) >> 4
	if u&0x80 != 0 {
		return muBias - t
	}
	return t - muBias
}

// EncodeAlawSample compands one 16-bit linear sample to an A-law octet.
func EncodeAlawSample(pcm int16) byte {
	// Work in the 12-bit domain the standard defines.
	v := pcm >> 3
	var mask int16
	if v >= 0 {
		mask = 0xD5
	} else {
		mask = 0x55
		v = -v - 1
	}
	if v > aClip {
		v = aClip
	}

	seg := segmentSearch(v, &segEndA)
	if seg >= 8 {
		return byte(0x7F ^ mask)
	}
	val := byte(seg << 4)
	if seg < 2 {
		val |= byte((v >> 1) & 0xF)
	} else {
		val |= byte((v >> seg) & 0xF)
	}
	return val ^ byte(mask)
}

// decodeAlaw expands one A-law octet to a 16-bit linear sample.
func decodeAlaw(a byte) int16 {
	a ^= 0x55
	t := int16(a&0x0F) << 4
	switch seg := (a & 0x70) >> 4; seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if a&0x80 != 0 {
		return t
	}
	return -t
}

// DecodeMulawSample expands one mu-law octet using the precomputed table.
func DecodeMulawSample(u byte) int16 { return mulawToPCM[u] }

// DecodeAlawSample expands one A-law octet using the precomputed table.
func DecodeAlawSample(a byte) int16 { return alawToPCM[a] }

// Codec identifies the G.711 variant carried on the wire.
type Codec uint8

const (
	// CodecPCMU is G.711 mu-law, RTP payload type 0.
	CodecPCMU Codec = 0
	// CodecPCMA is G.711 A-law, RTP payload type 8.
	CodecPCMA Codec = 8
)

// PayloadType returns the static RTP payload type for the codec.
func (c Codec) PayloadType() uint8 { return uint8(c) }

// Name returns the SDP encoding name.
func (c Codec) Name() string {
	if c == CodecPCMA {
		return "PCMA"
	}
	return "PCMU"
}

// ParseCodec maps a configuration string to a Codec, defaulting to mu-law.
func ParseCodec(s string) Codec {
	switch s {
	case "pcma", "PCMA", "alaw", "8":
		return CodecPCMA
	default:
		return CodecPCMU
	}
}

// Encode compands a linear frame into dst, which must have len(pcm) capacity.
// It returns the filled slice so callers can reuse a scratch buffer.
func (c Codec) Encode(dst []byte, pcm []int16) []byte {
	if cap(dst) < len(pcm) {
		dst = make([]byte, len(pcm))
	}
	dst = dst[:len(pcm)]
	if c == CodecPCMA {
		for i, s := range pcm {
			dst[i] = EncodeAlawSample(s)
		}
		return dst
	}
	for i, s := range pcm {
		dst[i] = EncodeMulawSample(s)
	}
	return dst
}

// Decode expands a companded payload into dst, which must have len(payload)
// capacity. It returns the filled slice.
func (c Codec) Decode(dst []int16, payload []byte) []int16 {
	if cap(dst) < len(payload) {
		dst = make([]int16, len(payload))
	}
	dst = dst[:len(payload)]
	if c == CodecPCMA {
		for i, b := range payload {
			dst[i] = alawToPCM[b]
		}
		return dst
	}
	for i, b := range payload {
		dst[i] = mulawToPCM[b]
	}
	return dst
}

// Silence returns the companded octet that decodes closest to zero. Filling a
// gap with this value is what a real gateway transmits during silence; a zero
// byte would be a loud tone in mu-law.
func (c Codec) Silence() byte {
	if c == CodecPCMA {
		return 0xD5
	}
	return 0xFF
}
