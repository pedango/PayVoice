package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// WAV format tags as they appear in the fmt chunk.
const (
	wavFormatPCM   = 0x0001
	wavFormatALaw  = 0x0006
	wavFormatMuLaw = 0x0007
	wavFormatExt   = 0xFFFE
)

// ErrNotWAV is returned when the payload lacks a RIFF/WAVE header.
var ErrNotWAV = errors.New("audio: not a RIFF/WAVE stream")

// DecodeWAV parses a RIFF/WAVE payload into mono linear PCM and reports its
// sample rate. Multi-channel input is downmixed, since the telephone side is
// always mono. It accepts the three encodings speech providers actually emit:
// 16-bit PCM, A-law and mu-law.
//
// Chunks are walked rather than assumed to sit at fixed offsets, because
// providers routinely prepend LIST/INFO metadata before the data chunk.
func DecodeWAV(b []byte) ([]int16, int, error) {
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, 0, ErrNotWAV
	}

	var (
		format     uint16
		channels   uint16
		sampleRate uint32
		bits       uint16
		data       []byte
		haveFmt    bool
	)

	for pos := 12; pos+8 <= len(b); {
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		body := pos + 8
		if size < 0 || body+size > len(b) {
			// Truncated final chunk: take whatever is present. Streaming TTS
			// responses often declare a length they never finish writing.
			size = len(b) - body
		}
		if size < 0 {
			break
		}

		switch id {
		case "fmt ":
			if size < 16 {
				return nil, 0, fmt.Errorf("audio: short fmt chunk (%d bytes)", size)
			}
			format = binary.LittleEndian.Uint16(b[body : body+2])
			channels = binary.LittleEndian.Uint16(b[body+2 : body+4])
			sampleRate = binary.LittleEndian.Uint32(b[body+4 : body+8])
			bits = binary.LittleEndian.Uint16(b[body+14 : body+16])
			if format == wavFormatExt && size >= 40 {
				// WAVE_FORMAT_EXTENSIBLE hides the real tag in the first two
				// bytes of the SubFormat GUID.
				format = binary.LittleEndian.Uint16(b[body+24 : body+26])
			}
			haveFmt = true
		case "data":
			data = b[body : body+size]
		}

		// Chunks are word aligned.
		pos = body + size
		if size%2 == 1 {
			pos++
		}
	}

	if !haveFmt {
		return nil, 0, errors.New("audio: WAVE stream has no fmt chunk")
	}
	if data == nil {
		return nil, 0, errors.New("audio: WAVE stream has no data chunk")
	}
	if channels == 0 {
		channels = 1
	}

	var samples []int16
	switch format {
	case wavFormatPCM:
		if bits != 16 {
			return nil, 0, fmt.Errorf("audio: unsupported PCM depth %d, want 16", bits)
		}
		samples = make([]int16, len(data)/2)
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(data[i*2 : i*2+2]))
		}
	case wavFormatMuLaw:
		samples = make([]int16, len(data))
		for i, v := range data {
			samples[i] = DecodeMulawSample(v)
		}
	case wavFormatALaw:
		samples = make([]int16, len(data))
		for i, v := range data {
			samples[i] = DecodeAlawSample(v)
		}
	default:
		return nil, 0, fmt.Errorf("audio: unsupported WAVE format tag 0x%04X", format)
	}

	if channels > 1 {
		samples = downmix(samples, int(channels))
	}
	return samples, int(sampleRate), nil
}

// downmix averages interleaved channels into mono.
func downmix(in []int16, channels int) []int16 {
	out := make([]int16, len(in)/channels)
	for i := range out {
		var sum int32
		for c := 0; c < channels; c++ {
			sum += int32(in[i*channels+c])
		}
		out[i] = clamp16(sum / int32(channels))
	}
	return out
}

// DecodeRawPCM16LE reads headerless little-endian 16-bit PCM, which is what
// most providers return when asked for "pcm_16000" style output.
func DecodeRawPCM16LE(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[i*2 : i*2+2]))
	}
	return out
}

// EncodeWAV wraps mono linear PCM in a 16-bit RIFF/WAVE container. Used by the
// optional call recorder and by test fixtures.
func EncodeWAV(pcm []int16, sampleRate int) []byte {
	const headerSize = 44
	dataSize := len(pcm) * 2
	out := make([]byte, headerSize+dataSize)

	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+dataSize))
	copy(out[8:12], "WAVE")

	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], wavFormatPCM)
	binary.LittleEndian.PutUint16(out[22:24], 1)
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(out[32:34], 2)
	binary.LittleEndian.PutUint16(out[34:36], 16)

	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(dataSize))
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(out[44+i*2:46+i*2], uint16(s))
	}
	return out
}
