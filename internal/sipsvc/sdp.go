package sipsvc

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/pion/sdp/v3"

	"github.com/pedango/PayVoice/internal/audio"
)

// Errors returned during media negotiation.
var (
	// ErrNoAudio means the offer contained no audio media description.
	ErrNoAudio = errors.New("sipsvc: offer has no audio media")
	// ErrNoCommonCodec means the peer offered no G.711 variant. PayVoice
	// answers 488 in this case rather than picking something it cannot mix.
	ErrNoCommonCodec = errors.New("sipsvc: no common codec, G.711 required")
)

// Offer is the media description extracted from a peer's SDP.
type Offer struct {
	// Address and Port are where media should be sent.
	Address string
	Port    int
	// Codec is the negotiated G.711 variant.
	Codec audio.Codec
	// DTMFPayloadType is the peer's telephone-event type, or zero if it did
	// not offer one.
	DTMFPayloadType uint8
	// Direction is the peer's a= direction attribute.
	Direction string
	// PTime is the peer's packetization interval in milliseconds.
	PTime int
}

// HoldRequested reports whether the peer put the call on hold. RFC 3264 hold
// is signalled by direction, and older devices use a zero connection address.
func (o *Offer) HoldRequested() bool {
	return o.Direction == "sendonly" || o.Direction == "inactive" || o.Address == "0.0.0.0"
}

// RemoteAddr resolves the offer to a UDP address.
func (o *Offer) RemoteAddr() (*net.UDPAddr, error) {
	if o.Address == "" || o.Port == 0 {
		return nil, errors.New("sipsvc: offer has no media address")
	}
	return net.ResolveUDPAddr("udp", net.JoinHostPort(o.Address, strconv.Itoa(o.Port)))
}

// ParseOffer reads a peer's SDP and negotiates a codec.
//
// Codec selection prefers the peer's own ordering rather than PayVoice's: the
// first G.711 variant in the m= line wins. Gateways list their preference
// first, and overriding it is a common cause of one-way audio with carriers
// that transcode reluctantly.
func ParseOffer(body []byte) (*Offer, error) {
	var sd sdp.SessionDescription
	if err := sd.Unmarshal(body); err != nil {
		return nil, fmt.Errorf("sipsvc: malformed SDP: %w", err)
	}

	var media *sdp.MediaDescription
	for _, m := range sd.MediaDescriptions {
		if m.MediaName.Media == "audio" {
			media = m
			break
		}
	}
	if media == nil {
		return nil, ErrNoAudio
	}

	o := &Offer{
		Port:      media.MediaName.Port.Value,
		Direction: "sendrecv",
		PTime:     audio.FrameDurationMs,
	}

	// The media-level connection line overrides the session-level one.
	if sd.ConnectionInformation != nil && sd.ConnectionInformation.Address != nil {
		o.Address = sd.ConnectionInformation.Address.Address
	}
	if media.ConnectionInformation != nil && media.ConnectionInformation.Address != nil {
		o.Address = media.ConnectionInformation.Address.Address
	}

	// Map dynamic payload types by their rtpmap encoding name.
	dynamic := map[string]uint8{}
	for _, a := range media.Attributes {
		switch a.Key {
		case "rtpmap":
			pt, name, ok := parseRTPMap(a.Value)
			if !ok {
				continue
			}
			dynamic[strings.ToLower(name)] = pt
		case "ptime":
			if v, err := strconv.Atoi(strings.TrimSpace(a.Value)); err == nil && v > 0 {
				o.PTime = v
			}
		case "sendrecv", "sendonly", "recvonly", "inactive":
			o.Direction = a.Key
		}
	}
	if pt, ok := dynamic["telephone-event"]; ok {
		o.DTMFPayloadType = pt
	}

	found := false
	for _, f := range media.MediaName.Formats {
		switch f {
		case "0":
			o.Codec, found = audio.CodecPCMU, true
		case "8":
			o.Codec, found = audio.CodecPCMA, true
		}
		if found {
			break
		}
	}
	if !found {
		return nil, ErrNoCommonCodec
	}
	return o, nil
}

// parseRTPMap splits "101 telephone-event/8000" into its payload type and name.
func parseRTPMap(v string) (uint8, string, bool) {
	fields := strings.Fields(strings.TrimSpace(v))
	if len(fields) < 2 {
		return 0, "", false
	}
	pt, err := strconv.Atoi(fields[0])
	if err != nil || pt < 0 || pt > 127 {
		return 0, "", false
	}
	name := fields[1]
	if i := strings.Index(name, "/"); i > 0 {
		name = name[:i]
	}
	return uint8(pt), name, true
}

// AnswerParams describes the SDP PayVoice sends back.
type AnswerParams struct {
	// Host is the address to advertise, which for a NAT deployment must be the
	// public address rather than the socket's bind address.
	Host string
	Port int
	// Codec is the negotiated variant, which must match the offer.
	Codec audio.Codec
	// DTMFPayloadType echoes the peer's telephone-event type so both
	// directions use one number.
	DTMFPayloadType uint8
	// Hold answers a hold offer with the matching direction.
	Hold bool
}

// BuildSDP renders an offer or answer.
//
// The SDP is written by hand rather than through a builder because the exact
// attribute set matters to carrier interop, and because an answer must mirror
// the offer's payload type numbering precisely.
func BuildSDP(p AnswerParams) []byte {
	if p.DTMFPayloadType == 0 {
		p.DTMFPayloadType = 101
	}
	// RFC 4566 recommends an NTP-style timestamp for the session id.
	sessionID := time.Now().Unix() + 2208988800

	direction := "sendrecv"
	if p.Hold {
		direction = "sendonly"
	}

	formats := fmt.Sprintf("%d %d", p.Codec.PayloadType(), p.DTMFPayloadType)

	var b strings.Builder
	fmt.Fprintf(&b, "v=0\r\n")
	fmt.Fprintf(&b, "o=payvoice %d %d IN IP4 %s\r\n", sessionID, sessionID, p.Host)
	fmt.Fprintf(&b, "s=payvoice\r\n")
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", p.Host)
	fmt.Fprintf(&b, "t=0 0\r\n")
	fmt.Fprintf(&b, "m=audio %d RTP/AVP %s\r\n", p.Port, formats)
	fmt.Fprintf(&b, "a=rtpmap:%d %s/8000\r\n", p.Codec.PayloadType(), p.Codec.Name())
	fmt.Fprintf(&b, "a=rtpmap:%d telephone-event/8000\r\n", p.DTMFPayloadType)
	// 0-16 covers the digits, star, pound, A-D and flash.
	fmt.Fprintf(&b, "a=fmtp:%d 0-16\r\n", p.DTMFPayloadType)
	fmt.Fprintf(&b, "a=ptime:%d\r\n", audio.FrameDurationMs)
	fmt.Fprintf(&b, "a=maxptime:%d\r\n", audio.FrameDurationMs)
	fmt.Fprintf(&b, "a=%s\r\n", direction)
	return []byte(b.String())
}
