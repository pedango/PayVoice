# Pedango

A SIP ⇄ WebRTC voice bridge in Go. Pedango puts a phone call and a browser call
into the same audio mix, so a caller on a GSM handset and a caller in a web app
can talk to each other — or to the same voice agent — without either side
knowing the other is on a different network.

It was built for [PayPlus](https://github.com/), a Ghanaian payments platform
where a customer with no data connection dials a normal phone number and ends
up talking to a voice agent that lives on the internet. Nothing in Pedango is
specific to payments: it is a media plane, and the application decides what the
conversation means.

```
   GSM / PSTN                                            Browser
       │                                                    │
       │ SIP + RTP (G.711)                 WebRTC (G.711)   │
       ▼                                                    ▼
   ┌─────────────────────── Pedango ───────────────────────────┐
   │  SIP UA  ·  RTP/RFC4733  │  mixer  │  ICE/DTLS-SRTP       │
   └───────────────────────────┬───────────────────────────────┘
                               │ signed webhooks (events)
                               │ REST (control)
                               ▼
                         Your application
```

## What it does

- **Bridges SIP and WebRTC** in one mixing room, in both directions.
- **Mixes N legs** with mix-minus-self, so nobody hears their own voice.
- **Collects DTMF** from both phone and browser, correctly deduplicated.
- **Speaks** through a pluggable TTS provider, paced onto the media path.
- **Transcribes** through a pluggable STT provider.
- **Reports everything** as HMAC-signed webhooks.

## What it deliberately does not do

Pedango holds no call logic, no state machine, no business rules. It does not
decide whether to answer a call, what to say, or what a spoken sentence means.
It reports `leg.ringing` and waits. That separation is what lets the
application be tested without a telephone and lets Pedango be tested without
the application.

## Design decisions worth knowing

**The whole media plane is 8 kHz G.711, including WebRTC.** Browsers support
PCMU, so the media engine registers only G.711 and telephone-event. A browser
leg and a phone leg therefore carry byte-identical payloads and bridging them
is a memory copy rather than an Opus decode plus a G.711 encode. This removes
the `libopus` cgo dependency entirely, keeps the binary statically linkable,
and cuts bridging latency to near zero. The cost is telephone-grade audio,
which is the correct target for anything bridged to the PSTN anyway.

**One media clock for the whole process.** Per-leg tickers drift against each
other, which means joining a leg to a room requires handing it between clocks,
and that handover races. A single 20 ms ticker drives every room and every
unbridged leg, so rooms stay sample-aligned and the race cannot exist. The work
per tick is integer arithmetic over a few hundred samples per leg — far inside
the budget.

**DTMF is deduplicated by RTP timestamp.** One keypress is a burst of packets
repeated every 20 ms for as long as the key is held, plus three redundant end
packets, all sharing the timestamp of the key-down instant. Reporting one digit
per packet turns a single `5` into `5555555555`. On a PIN entry path that means
an instantly locked account. See `TestDTMFCollectorDeduplicatesBurst`.

**Webhooks are sharded by leg, not pooled.** The digits of a PIN arrive as
separate events, and delivering them out of order would authorise the wrong
thing. Events for one leg always take the same worker; different legs run in
parallel.

**Publishing an event never blocks.** It is called from inside the 20 ms media
budget, so a wedged receiver costs dropped events rather than stuttering audio
on every live call.

## Quick start

```bash
go build ./cmd/pedango
PEDANGO_TTS_PROVIDER=tone ./pedango
```

That runs with no credentials at all. The `tone` TTS provider renders text as a
pattern of beeps, so you can verify synthesis, queueing, mixing, packetization
and transport end to end before signing up with any vendor. If you hear the
beeps, the media path works and any later silence is a provider problem.

With Docker:

```bash
cp .env.example .env     # then edit
docker compose up --build
```

## Try it

Create a room, put a browser in it, and make it talk:

```bash
TOKEN=your-api-token

# 1. A room to mix in
curl -sX POST localhost:8080/v1/rooms \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{}'
# → {"id":"room_9f2c...","sample_rate":8000}

# 2. A browser joins by posting its SDP offer
curl -sX POST localhost:8080/v1/webrtc/offer \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"sdp":"<browser offer>","room_id":"room_9f2c..."}'
# → {"leg_id":"leg_4a1b...","sdp":"<answer>","type":"answer"}

# 3. Say something to everyone in the room
curl -sX POST localhost:8080/v1/rooms/room_9f2c.../tts \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"text":"Welcome to PayPlus. Enter your four digit PIN."}'

# 4. Bring a phone into the same room
curl -sX POST localhost:8080/v1/legs \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"type":"sip","to":"233245224253","room_id":"room_9f2c..."}'
```

The browser client must offer PCMU. See [docs/BROWSER.md](docs/BROWSER.md) for
the twelve lines of JavaScript that arranges it.

## Documentation

| Document | Contents |
| --- | --- |
| [docs/PROTOCOL.md](docs/PROTOCOL.md) | Every REST endpoint and every webhook event |
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | How audio actually moves through the process |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | NAT, firewall ports, TURN, and carrier setup |
| [docs/BROWSER.md](docs/BROWSER.md) | Client-side JavaScript, including forcing PCMU |
| [.env.example](.env.example) | Every configuration variable, annotated |

## Configuration

Everything is environment driven. The defaults run locally with no
credentials; `PEDANGO_ENV=production` turns on guardrails that refuse to start
without API tokens, a webhook secret of reasonable length, and HTTPS.

The variables you will always set:

| Variable | Purpose |
| --- | --- |
| `PEDANGO_API_TOKENS` | Comma-separated bearer tokens for the control plane |
| `PEDANGO_WEBHOOK_URL` | Where events are delivered |
| `PEDANGO_WEBHOOK_SECRET` | HMAC-SHA256 signing key for those events |
| `PEDANGO_SIP_ENABLED` | Set `true` to accept and place phone calls |
| `PEDANGO_SIP_PUBLIC_HOST` | The address to advertise in SDP, not the bind address |
| `PEDANGO_TTS_PROVIDER` | `elevenlabs`, `http`, `tone`, or `silence` |

Generate secrets with `openssl rand -hex 32`. The full list is in
[.env.example](.env.example).

## Verifying a webhook

The signature covers the exact bytes sent, so verify against the raw body. A
JSON round trip can reorder keys and invalidate an otherwise valid signature.

```js
const crypto = require('crypto');

function verify(rawBody, header, secret) {
  const expected = crypto.createHmac('sha256', secret).update(rawBody).digest('hex');
  const provided = String(header).replace(/^sha256=/, '');
  const a = Buffer.from(expected, 'hex');
  const b = Buffer.from(provided, 'hex');
  return a.length === b.length && crypto.timingSafeEqual(a, b);
}
```

Pedango sends the same value in both `X-Pedango-Signature` and
`X-Webhook-Signature`, so an existing receiver needs no change.

## Tests

```bash
go test ./...
go test -race ./...
```

The suite runs without a network, a phone line, or a vendor account.
`TestWebRTCOfferEstablishesMedia` stands up a real peer connection against the
service over loopback and pushes G.711 through it, which exercises codec
negotiation, ICE, DTLS-SRTP, depacketization and the jitter buffer together.

## Status

Working and tested: SIP signalling and registration, RTP with symmetric
latching, RFC 4733 DTMF in both directions, WebRTC with trickle ICE, N-way
mixing, barge-in, TTS with caching, Deepgram streaming STT, and signed
webhooks.

Not yet implemented: SRTP on the SIP side (WebRTC legs are always encrypted;
SIP legs are plain RTP and should run over a private link or VPN), call
recording, and multi-node clustering.

## Licence

MIT. See [LICENSE](LICENSE).
