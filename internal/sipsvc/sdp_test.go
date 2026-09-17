package sipsvc

import (
	"errors"
	"strings"
	"testing"

	"github.com/pedango/pedango/internal/audio"
)

// asteriskOffer is a realistic offer from a PBX, including the trailing
// telephone-event and direction attributes that must be honoured.
const asteriskOffer = `v=0
o=root 1926113224 1926113224 IN IP4 197.251.10.4
s=Asterisk PBX 18.9.0
c=IN IP4 197.251.10.4
t=0 0
m=audio 14006 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
a=ptime:20
a=maxptime:150
a=sendrecv
`

func TestParseOfferFromPBX(t *testing.T) {
	offer, err := ParseOffer([]byte(asteriskOffer))
	if err != nil {
		t.Fatalf("ParseOffer: %v", err)
	}

	if offer.Address != "197.251.10.4" {
		t.Errorf("address = %q, want 197.251.10.4", offer.Address)
	}
	if offer.Port != 14006 {
		t.Errorf("port = %d, want 14006", offer.Port)
	}
	if offer.Codec != audio.CodecPCMU {
		t.Errorf("codec = %s, want PCMU (the first the peer listed)", offer.Codec.Name())
	}
	if offer.DTMFPayloadType != 101 {
		t.Errorf("telephone-event PT = %d, want 101", offer.DTMFPayloadType)
	}
	if offer.PTime != 20 {
		t.Errorf("ptime = %d, want 20", offer.PTime)
	}
	if offer.HoldRequested() {
		t.Error("a sendrecv offer was read as hold")
	}
}

// TestParseOfferHonoursPeerCodecOrder covers the interop trap: when a gateway
// lists A-law first it means it, and answering with mu-law is a common cause
// of one-way audio.
func TestParseOfferHonoursPeerCodecOrder(t *testing.T) {
	alawFirst := strings.Replace(asteriskOffer,
		"m=audio 14006 RTP/AVP 0 8 101",
		"m=audio 14006 RTP/AVP 8 0 101", 1)

	offer, err := ParseOffer([]byte(alawFirst))
	if err != nil {
		t.Fatalf("ParseOffer: %v", err)
	}
	if offer.Codec != audio.CodecPCMA {
		t.Errorf("codec = %s, want PCMA because the peer listed it first", offer.Codec.Name())
	}
}

// TestParseOfferMediaLevelConnectionWins covers an SDP that carries both a
// session-level and a media-level c= line, where the media-level one governs.
func TestParseOfferMediaLevelConnectionWins(t *testing.T) {
	body := `v=0
o=- 1 1 IN IP4 10.0.0.1
s=-
c=IN IP4 10.0.0.1
t=0 0
m=audio 5000 RTP/AVP 0
c=IN IP4 203.0.113.9
a=rtpmap:0 PCMU/8000
`
	offer, err := ParseOffer([]byte(body))
	if err != nil {
		t.Fatalf("ParseOffer: %v", err)
	}
	if offer.Address != "203.0.113.9" {
		t.Errorf("address = %q, want the media-level 203.0.113.9", offer.Address)
	}
}

func TestParseOfferRejectsOpusOnly(t *testing.T) {
	body := `v=0
o=- 1 1 IN IP4 10.0.0.1
s=-
c=IN IP4 10.0.0.1
t=0 0
m=audio 5000 RTP/AVP 111
a=rtpmap:111 opus/48000/2
`
	_, err := ParseOffer([]byte(body))
	if !errors.Is(err, ErrNoCommonCodec) {
		t.Errorf("error = %v, want ErrNoCommonCodec", err)
	}
}

func TestParseOfferRejectsVideoOnly(t *testing.T) {
	body := `v=0
o=- 1 1 IN IP4 10.0.0.1
s=-
c=IN IP4 10.0.0.1
t=0 0
m=video 5000 RTP/AVP 96
a=rtpmap:96 VP8/90000
`
	if _, err := ParseOffer([]byte(body)); !errors.Is(err, ErrNoAudio) {
		t.Errorf("error = %v, want ErrNoAudio", err)
	}
}

func TestParseOfferDetectsHold(t *testing.T) {
	held := strings.Replace(asteriskOffer, "a=sendrecv", "a=sendonly", 1)
	offer, err := ParseOffer([]byte(held))
	if err != nil {
		t.Fatalf("ParseOffer: %v", err)
	}
	if !offer.HoldRequested() {
		t.Error("a sendonly offer was not read as hold")
	}
}

// TestParseOfferDetectsLegacyHold covers older devices that signal hold with a
// zero connection address instead of a direction attribute.
func TestParseOfferDetectsLegacyHold(t *testing.T) {
	body := `v=0
o=- 1 1 IN IP4 0.0.0.0
s=-
c=IN IP4 0.0.0.0
t=0 0
m=audio 5000 RTP/AVP 0
a=rtpmap:0 PCMU/8000
`
	offer, err := ParseOffer([]byte(body))
	if err != nil {
		t.Fatalf("ParseOffer: %v", err)
	}
	if !offer.HoldRequested() {
		t.Error("a zero connection address was not read as hold")
	}
}

func TestParseOfferRejectsGarbage(t *testing.T) {
	if _, err := ParseOffer([]byte("this is not SDP at all")); err == nil {
		t.Error("expected an error for a non-SDP body")
	}
}

// TestBuildSDPIsParseable is the round trip that matters: what Pedango answers
// must be readable by the same parser, and must preserve the negotiated
// payload types exactly.
func TestBuildSDPIsParseable(t *testing.T) {
	answer := BuildSDP(AnswerParams{
		Host:            "41.66.10.20",
		Port:            20004,
		Codec:           audio.CodecPCMA,
		DTMFPayloadType: 96,
	})

	parsed, err := ParseOffer(answer)
	if err != nil {
		t.Fatalf("our own answer did not parse: %v\n%s", err, answer)
	}
	if parsed.Address != "41.66.10.20" {
		t.Errorf("address = %q, want 41.66.10.20", parsed.Address)
	}
	if parsed.Port != 20004 {
		t.Errorf("port = %d, want 20004", parsed.Port)
	}
	if parsed.Codec != audio.CodecPCMA {
		t.Errorf("codec = %s, want PCMA", parsed.Codec.Name())
	}
	if parsed.DTMFPayloadType != 96 {
		t.Errorf("telephone-event PT = %d, want the echoed 96", parsed.DTMFPayloadType)
	}
}

// TestBuildSDPUsesCRLF guards a real interop failure: SIP bodies must use CRLF
// line endings, and some gateways drop a call outright on bare LF.
func TestBuildSDPUsesCRLF(t *testing.T) {
	body := string(BuildSDP(AnswerParams{Host: "10.0.0.1", Port: 20000, Codec: audio.CodecPCMU}))

	for _, line := range strings.Split(body, "\r\n") {
		if strings.Contains(line, "\n") {
			t.Fatalf("found a bare LF inside %q", line)
		}
	}
	if !strings.HasSuffix(body, "\r\n") {
		t.Error("SDP body does not end with CRLF")
	}
}

func TestBuildSDPDefaultsDTMFPayloadType(t *testing.T) {
	body := BuildSDP(AnswerParams{Host: "10.0.0.1", Port: 20000, Codec: audio.CodecPCMU})
	if !strings.Contains(string(body), "a=rtpmap:101 telephone-event/8000") {
		t.Errorf("expected a default telephone-event mapping, got:\n%s", body)
	}
}

func TestBuildSDPHold(t *testing.T) {
	body := string(BuildSDP(AnswerParams{
		Host: "10.0.0.1", Port: 20000, Codec: audio.CodecPCMU, Hold: true,
	}))
	if !strings.Contains(body, "a=sendonly") {
		t.Error("hold answer is missing a=sendonly")
	}
}

func TestOfferRemoteAddr(t *testing.T) {
	offer, err := ParseOffer([]byte(asteriskOffer))
	if err != nil {
		t.Fatalf("ParseOffer: %v", err)
	}
	addr, err := offer.RemoteAddr()
	if err != nil {
		t.Fatalf("RemoteAddr: %v", err)
	}
	if addr.String() != "197.251.10.4:14006" {
		t.Errorf("remote = %s, want 197.251.10.4:14006", addr)
	}
}

func TestParseTrunkURIPlacesDialledNumber(t *testing.T) {
	uri, err := parseTrunkURI("sip:trunk.carrier.gh:5060", "233245224253", "pedango.local")
	if err != nil {
		t.Fatalf("parseTrunkURI: %v", err)
	}
	if uri.User != "233245224253" {
		t.Errorf("user = %q, want the dialled number", uri.User)
	}
	if uri.Host != "trunk.carrier.gh" {
		t.Errorf("host = %q, want trunk.carrier.gh", uri.Host)
	}
	if uri.Port != 5060 {
		t.Errorf("port = %d, want 5060", uri.Port)
	}
}

func TestParseTrunkURIRejectsGarbage(t *testing.T) {
	if _, err := parseTrunkURI("", "123", "example.com"); err == nil {
		t.Error("expected an error for an empty trunk URI")
	}
}
