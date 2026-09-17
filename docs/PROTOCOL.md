# Protocol

Pedango exposes a REST control plane and delivers events as signed webhooks.
The control plane is how the application acts; the webhooks are how it learns.

## Authentication

Every `/v1/*` endpoint requires a bearer token from `PEDANGO_API_TOKENS`:

```
Authorization: Bearer <token>
```

Tokens are compared in constant time against every configured value, so a
timing side channel cannot be used to recover one. `/healthz` and `/readyz` are
unauthenticated so a load balancer can probe them, and carry no call detail.

`PEDANGO_API_ALLOWED_IPS` adds an optional network allowlist on top.

## Errors

Every failure uses one envelope:

```json
{ "error": { "code": "leg_not_found", "message": "No such leg" } }
```

| Code | Status | Meaning |
| --- | --- | --- |
| `unauthorized` | 401 | Missing or wrong bearer token |
| `forbidden` | 403 | Caller address not in the allowlist |
| `invalid_request` | 400 | Malformed body or missing field |
| `leg_not_found` | 404 | No such leg |
| `room_not_found` | 404 | No such room |
| `room_full` | 409 | Room is at `PEDANGO_MAX_LEGS_PER_ROOM` |
| `leg_busy` | 409 | Leg is already mixed into another room |
| `not_ringing` | 409 | Answer called on a leg that is not ringing |
| `no_common_codec` | 415 | Browser offered no G.711 |
| `sip_disabled` | 503 | `PEDANGO_SIP_ENABLED` is false |
| `no_trunk` | 503 | No `PEDANGO_SIP_TRUNK` configured |
| `stt_disabled` | 503 | No recognition provider configured |
| `originate_failed` | 502 | The carrier rejected the call |
| `tts_failed` | 502 | The speech provider failed |

## Rooms

A room is a mixing bridge. Legs in a room hear each other. A two-leg room is a
bridged call; a one-leg room is a caller talking to the agent.

### `POST /v1/rooms`

```json
{ "id": "room_custom", "sample_rate": 8000 }
```

Both fields are optional. `id` is generated when omitted. `sample_rate` is
accepted for compatibility and ignored: Pedango always mixes at 8 kHz and the
response states the real rate.

```json
{
  "id": "room_9f2c1e",
  "created_at": "2026-09-17T08:00:00Z",
  "legs": [],
  "max_legs": 8,
  "sample_rate": 8000
}
```

### `GET /v1/rooms` · `GET /v1/rooms/{id}` · `DELETE /v1/rooms/{id}`

List, fetch, destroy. Destroying a room detaches its legs but does not hang
them up.

### `POST /v1/rooms/{id}/legs`

```json
{ "leg_id": "leg_4a1b" }
```

Mixes an existing leg into the room. Returns the room.

### `DELETE /v1/rooms/{id}/legs/{legID}`

Detaches a leg. The call stays up.

### `POST /v1/rooms/{id}/tts`

Speaks to everyone in the room.

```json
{
  "text": "Welcome to PayPlus. Enter your four digit PIN.",
  "provider": "elevenlabs",
  "voice": "21m00Tcm4TlvDq8ikWAM",
  "gain": 1.0,
  "interrupt": false
}
```

Only `text` is required. `interrupt` clears anything already queued, which is
how the agent cuts itself off to answer a caller who just spoke.

```json
{
  "playback_id": "pb_7c31",
  "room_id": "room_9f2c1e",
  "duration_ms": 2840,
  "queued": 1
}
```

`202 Accepted` means the audio is queued, not finished. Wait for the
`playback.finished` event carrying the same `playback_id`.

## Legs

A leg is one call, either SIP or WebRTC.

### `POST /v1/legs`

Originates an outbound phone call. This is the path that turns a browser
session into a phone call when the caller loses their data connection.

```json
{
  "type": "sip",
  "to": "233245224253",
  "from": "233302000000",
  "room_id": "room_9f2c1e",
  "app_ref": "voice_session_abc"
}
```

`app_ref` is opaque and echoed on every event for that leg, so the application
can correlate without keeping its own map. Joining a room here rather than
after `leg.answered` means the caller hears the room the instant they pick up.

Returns `201` with a leg in state `ringing`. It becomes `answered` when the far
end picks up, which arrives as an event.

### `GET /v1/legs` · `GET /v1/legs/{id}`

```json
{
  "id": "leg_4a1b",
  "type": "sip",
  "state": "answered",
  "direction": "inbound",
  "from": "233244000000",
  "to": "233245224253",
  "room_id": "room_9f2c1e",
  "app_ref": "voice_session_abc",
  "remote_addr": "197.251.10.4:14006",
  "muted": false,
  "playing": true,
  "created_at": "2026-09-17T08:00:01Z",
  "duration_ms": 18400,
  "jitter": {
    "Received": 920, "Played": 918, "Lost": 2,
    "Late": 0, "Duplicate": 0, "Resync": 0,
    "Depth": 3, "Priming": false
  }
}
```

`duration_ms` counts from answer, so it is the billable duration. The jitter
block is the honest health of the receive path: rising `Late` or `Resync` means
`PEDANGO_JITTER_TARGET` is too low for that network.

### `POST /v1/legs/{id}/answer`

Answers a ringing inbound SIP call with 200 OK and an SDP answer. Browser legs
answer themselves when ICE connects, so this is a no-op for them and returns
success to keep callers uniform.

### `DELETE /v1/legs/{id}`

Hangs up. Optional body `{"code": 486, "reason": "Busy Here"}` sets the SIP
status for a call that has not been answered yet.

### `POST /v1/legs/{id}/tts`

Speaks to one leg only, even if it is in a room. Same body and response as the
room variant.

### `POST /v1/legs/{id}/stop`

Stops playback immediately and returns the interrupted playback ids. A short
fade is applied, because cutting speech straight to zero mid-word produces an
audible click on a phone line.

### `POST /v1/legs/{id}/dtmf`

```json
{ "digits": "1234#", "duration_ms": 160 }
```

Sends RFC 4733 telephone-events toward the peer, for driving somebody else's
IVR. Not supported toward a browser, which would not render them.

### `POST /v1/legs/{id}/mute`

```json
{ "muted": true }
```

Drops the leg's contribution to the mix without tearing down the call.

### `POST /v1/legs/{id}/transcribe`

```json
{ "enabled": true }
```

Attaches speech recognition. Results arrive as `stt.partial` and `stt.final`
events. Recognition is closed automatically when the leg ends.

### `POST /v1/legs/{id}/ice-candidates`

```json
{ "candidate": { "candidate": "candidate:...", "sdpMid": "0", "sdpMLineIndex": 0 } }
```

Feeds a trickled candidate from the browser. Pedango's own candidates are
already inside the answer, so this channel is one-way.

## WebRTC

### `POST /v1/webrtc/offer`

```json
{
  "sdp": "v=0\r\no=- ...",
  "room_id": "room_9f2c1e",
  "from": "browser-session-1",
  "app_ref": "voice_session_abc"
}
```

```json
{ "leg_id": "leg_4a1b", "sdp": "v=0\r\no=- ...", "type": "answer" }
```

The answer already contains gathered candidates, so the browser can apply it
directly. Gathering is capped at two seconds so a slow STUN server cannot stall
the request.

The offer **must** include PCMU. Pedango does not negotiate Opus, and an
Opus-only offer is rejected with `no_common_codec`. See
[BROWSER.md](BROWSER.md).

## Introspection

### `GET /healthz`

Unauthenticated liveness.

### `GET /readyz`

Unauthenticated readiness. Returns 503 when the media clock is not running. A
process that is listening but whose clock has stopped is worse than one that is
down, because a load balancer would keep sending it calls.

### `GET /v1/config`

Everything a browser client needs, so ICE servers are configured in one place
rather than duplicated into the frontend.

### `GET /v1/stats`

Engine, SIP, WebRTC, webhook, TTS and STT counters. `engine.late_ticks` is the
number that matters: if it climbs, the process is starved and audio is
stuttering on every call.

## Webhook events

Delivered as `POST` to `PEDANGO_WEBHOOK_URL`:

```json
{
  "id": "evt_8b2f",
  "type": "dtmf.received",
  "created_at": "2026-09-17T08:00:12Z",
  "data": { "leg_id": "leg_4a1b", "room_id": "room_9f2c1e", "digit": "4" }
}
```

Headers:

| Header | Contents |
| --- | --- |
| `X-Pedango-Signature` | `sha256=<hex HMAC-SHA256 of the raw body>` |
| `X-Webhook-Signature` | The same value, for receivers that read this name |
| `X-Pedango-Timestamp` | Unix seconds at signing, for replay rejection |
| `X-Pedango-Delivery` | Unique per attempt, for idempotent receivers |
| `X-Pedango-Event` | The event type, for cheap routing |

Every event carries `leg_id` where one applies, `room_id` when the leg is
mixed, and `app_ref` when the application supplied one.

| Event | Meaning |
| --- | --- |
| `leg.ringing` | A call arrived, or an outbound call is ringing. Answer or hang up. |
| `leg.answered` | Media is flowing. |
| `leg.ended` | Terminal. Carries `cause`, `duration_ms` and final jitter stats. |
| `leg.failed` | An outbound call never connected. Carries `reason`. |
| `room.created` / `room.destroyed` | Room lifecycle. |
| `room.leg_joined` / `room.leg_left` | Mixing membership changed. |
| `dtmf.received` | One keypress. Already deduplicated: one event per press. |
| `speech.started` / `speech.ended` | Voice activity detection boundaries. |
| `playback.started` / `playback.finished` | A prompt began or completed. |
| `playback.interrupted` | A prompt was cut short. `cause` is `barge_in` or `dtmf`. |
| `stt.partial` | Interim transcript. **Never act on these.** |
| `stt.final` | Stable transcript, with `confidence`. |

### Delivery guarantees

Events for one leg are always delivered in order: they are sharded to a worker
by leg id rather than spread across a pool. This is not cosmetic — the digits
of a PIN arrive as separate `dtmf.received` events, and delivering them out of
order would authorise the wrong thing.

Delivery is at-least-once. A 5xx or a timeout is retried with exponential
backoff up to `PEDANGO_WEBHOOK_RETRIES`; a 4xx other than 429 is not retried,
because a malformed event will never become valid. Use `X-Pedango-Delivery` to
deduplicate.

If the receiver stalls long enough to fill the queue, events are **dropped**
rather than queued indefinitely. Publishing runs inside the 20 ms media budget,
so blocking would stutter audio on every live call. Dropped events are counted
in `GET /v1/stats`.

### Acting on transcripts

`stt.partial` results change as the caller keeps speaking. An interim
"send five" can resolve to "send fifty". Only ever act on `stt.final`, and for
anything that moves money prefer DTMF: a keypress cannot be misheard.
