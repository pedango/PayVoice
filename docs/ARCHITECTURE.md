# Architecture

How audio actually moves through the process, and why it moves that way.

## One uniform media plane

Everything inside PayVoice is **8 kHz, mono, 16-bit linear PCM, in 20 ms frames
of 160 samples**. Everything on the wire is **G.711**, on both the SIP side and
the WebRTC side.

This is the decision the rest of the design falls out of.

The obvious alternative is to speak Opus to browsers, since that is what WebRTC
normally uses. That would mean decoding 48 kHz stereo Opus, downsampling to
8 kHz, mixing, upsampling, and re-encoding — on every frame of every call —
plus a cgo dependency on `libopus` that costs the static binary and the scratch
container image.

Browsers support PCMU. So the media engine registers only G.711 and
telephone-event. A browser leg and a phone leg then carry byte-identical
payloads, and bridging them is a memory copy. The cost is telephone-grade
audio, which is exactly the quality target for something being bridged to the
PSTN.

```
SIP leg                                              WebRTC leg
   │ G.711 payload                      G.711 payload │
   ▼                                                  ▼
 decode ──► jitter buffer ──► mixer ──► jitter buffer ──► decode
             (per leg)     mix-minus-self    (per leg)
```

## The media clock

`internal/media.Engine` runs **one** 20 ms ticker for the whole process. Every
tick it drives every room, then every leg that is not in a room.

A per-leg ticker is the obvious design and the wrong one. Hundreds of
independent 20 ms timers drift against each other, so joining a leg to a room
means handing it from its own clock to the room's clock, and that handover
races: the leg can be read by both clocks in the same instant, or by neither.
One clock removes the race by construction, keeps every room sample-aligned,
and costs a single timer.

The work per tick is integer arithmetic over 160 samples per leg — hundreds of
microseconds for a busy process, against a 20 ms budget. `GET /v1/stats`
exposes `late_ticks`; if that climbs, the process is starved.

## Mixing

`Room.tick` runs in two passes, and the order is load-bearing.

```go
// Pass 1: collect, and build the shared sum.
for i, leg := range legs {
    scratch[i] = leg.receive()   // jitter buffer → VAD → tap
    mixer.Add(scratch[i])
}

// Pass 2: derive each leg's personal feed.
for i, leg := range legs {
    leg.transmit(mixer, scratch[i])   // sum − own
}
```

If a leg transmitted as soon as it was read, early legs would mix against a
partially built sum and hear a different conference from late legs.

Mixing every leg against every other would be O(N²) per tick. Instead one
shared sum is accumulated in 32-bit headroom, and each leg's own contribution
is subtracted when producing that leg's output. That is O(N) and gives
**mix-minus-self**: everyone hears everyone but themselves, so nobody hears
their own voice echoed back.

Saturation happens only at the final subtraction. Clamping the running sum
would distort loud frames even for a listener whose own loud contribution is
about to be removed.

## The jitter buffer

Packets arrive early, late, out of order, duplicated, or not at all. Each leg
has a buffer (`internal/audio.JitterBuffer`) that files packets into a ring
indexed by RTP timestamp and runs a playout cursor one target delay behind.

Three behaviours are worth calling out:

**Priming.** On the first packet the cursor sits *on* that packet's timestamp
and does not advance until the buffer holds the target delay. The tempting
alternative — backdating the cursor by the target delay — makes the buffer
spend its first frames reading timestamps that never existed and reporting them
as packet loss, so a perfectly healthy line looks broken in the metrics.
Priming is bounded, so a stream that stops after one packet cannot wedge a leg.

**Late packets are dropped, not played.** Reordered speech is worse than
concealed speech.

**Concealment decays.** A gap is filled with a fading repeat of the last good
frame for up to three frames, then silence. Digital silence clicks; indefinite
repetition buzzes.

Timestamp comparisons use signed arithmetic on the wrapped difference, so the
32-bit RTP timestamp rolling over does not look like a packet from the distant
past.

## DTMF

RFC 4733 carries keypresses as their own RTP payload type, separate from audio.
One keypress is **not** one packet: it is a burst repeated every 20 ms for as
long as the key is held, plus three redundant end packets, and every packet in
the burst shares the RTP timestamp of the key-down instant.

`DTMFCollector` keys on that timestamp and reports each distinct one once. A
collector that reports per packet turns a single `5` into `5555555555`, which
on a PIN entry path means an instantly locked account. Pressing the same key
twice produces a new timestamp, so `1122` still works.

The same collector serves both transports, because a browser's
`RTCDTMFSender` emits the identical wire format.

## Barge-in

A voice agent the caller cannot interrupt is unusable. Two things stop a
prompt:

- **Voice activity.** The per-leg VAD reports `speech.started`, and playback
  stops.
- **A keypress.** Always, regardless of the barge-in setting — pressing a key
  is unambiguous intent.

The VAD tracks an adaptive noise floor rather than a fixed threshold, because
line noise varies by orders of magnitude between a clean SIP trunk and a GSM
handset in a market. Hysteresis stops it chattering on the pauses inside a
word, which would otherwise chop one utterance into fragments.

Stopping applies a two-frame fade. Cutting speech straight to zero mid-word is
an audible click.

## Playout pacing

A TTS provider returns several seconds of audio in one buffer. A media path is
a real-time clock, so dumping that into a socket at once overruns the far end's
jitter buffer and is heard as a garbled burst. `Player` holds the buffer and
releases exactly one frame per tick.

While a prompt plays, conference audio is ducked to 45% so the agent stays
intelligible over background noise.

## Events

PayVoice holds no call logic. It reports what happened and waits.

Events flow from the media clock into `webhook.Dispatcher`, which must never
block — it is called from inside the 20 ms budget. Publishing is a
non-blocking send onto a queue; if the queue is full, the event is **dropped**
and counted. Stuttering every live call to protect one event is the wrong
trade.

Events are sharded across workers **by leg id**, not spread over a pool. The
digits of a PIN arrive as separate events, and delivering them out of order
would authorise the wrong thing. Sharding preserves per-leg order while letting
different calls overlap.

## Package layout

| Package | Responsibility |
| --- | --- |
| `internal/audio` | G.711, framing, jitter buffer, mixer, VAD, resampling, WAV |
| `internal/media` | Legs, rooms, playout, the media clock |
| `internal/rtpx` | RTP sockets, port pool, RFC 4733 |
| `internal/sipsvc` | SIP signalling, SDP negotiation, registration |
| `internal/rtcsvc` | WebRTC peer connections |
| `internal/tts` | Speech synthesis and its cache |
| `internal/stt` | Streaming recognition |
| `internal/webhook` | Signed event delivery |
| `internal/api` | REST control plane |
| `internal/config` | Environment configuration and validation |

Dependencies point one way: `api` and the transports depend on `media`, and
`media` depends only on `audio`. Nothing in `audio` or `media` knows what SIP
or WebRTC is, which is why the mixer can be tested with a fake transport and no
network.

## Security boundaries

**The control plane can originate calls**, so it must never face the public
internet. Bind it to loopback or a private network, require a bearer token, and
optionally restrict by address.

**Symmetric RTP latching is guarded.** Sending media to wherever packets arrive
from is required for NAT traversal, but latching to any arriving packet would
let anyone who guesses the port inject audio into a live call. PayVoice latches
to the first source and only accepts a different one after the current source
has gone quiet for two seconds — which happens when a carrier legitimately
moves a call between media servers, and does not happen during an injection
attempt against a healthy call.

**WebRTC legs are encrypted** by DTLS-SRTP, always. **SIP legs are plain RTP**
and should run over a private link or VPN to the carrier. SRTP on the SIP side
is not yet implemented.

**Webhook signatures cover the raw bytes sent.** Receivers must verify against
the raw request body, not a re-encoded copy: a JSON round trip can reorder keys
and invalidate an otherwise valid signature.
