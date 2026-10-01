# Deployment

Almost every problem with a media server is a network problem. This document is
mostly about addresses and ports.

## Ports

| Port | Protocol | Who connects | Notes |
| --- | --- | --- | --- |
| 8080 | TCP | Your application only | **Never expose publicly.** It can originate calls. |
| 5060 | UDP | Your carrier only | Restrict by source IP. |
| 20000–20400 | UDP | Your carrier | RTP. One pair per concurrent call. |
| 40000–40400 | UDP | Browsers, anywhere | WebRTC ICE. |

The RTP range caps concurrency: each call takes one even/odd pair, so 400 ports
is 200 simultaneous phone calls. Size it and open it as a range — a partially
open range fails intermittently and looks like a carrier fault.

## The address problem

This is the single most common failure, and it produces one-way audio rather
than an error.

SDP advertises an address for the peer to send media to. Inside a container or
on a cloud VM, the address the process binds is private and the peer cannot
reach it. You must advertise the public address explicitly:

```bash
PAYVOICE_RTP_HOST=0.0.0.0              # bind everywhere
PAYVOICE_SIP_PUBLIC_HOST=41.66.10.20   # advertise the public address
PAYVOICE_WEBRTC_PUBLIC_IP=41.66.10.20  # rewrite the ICE host candidate
```

`PAYVOICE_RTP_SYMMETRIC=true` (the default) covers the reverse direction, by
sending to wherever packets actually arrive from rather than trusting the
address the peer advertised. Between the two, NAT works in both directions.

## Docker

Use `network_mode: host`. Docker's userland proxy rewrites UDP source
addresses, which breaks symmetric RTP latching, and mapping several hundred UDP
ports individually is slow to start and easy to get wrong.

If host networking is unavailable (Docker Desktop on macOS or Windows), publish
the ranges explicitly and keep them **identical** to the configured ranges:

```yaml
ports:
  - "8080:8080/tcp"
  - "5060:5060/udp"
  - "20000-20400:20000-20400/udp"
  - "40000-40400:40000-40400/udp"
```

A mismatch between the published range and `PAYVOICE_RTP_PORT_MIN/MAX` silently
drops inbound audio.

## TURN

STUN is enough when at least one side can accept an inbound UDP packet. It
fails for symmetric NAT and for restrictive corporate firewalls, and the
symptom is a call that connects and then carries no audio.

For production, run [coturn](https://github.com/coturn/coturn):

```bash
PAYVOICE_ICE_URLS=stun:stun.l.google.com:19302,turn:turn.yourdomain.com:3478
PAYVOICE_TURN_USERNAME=payvoice
PAYVOICE_TURN_CREDENTIAL=<secret>
```

Put TURN close to your users. In Ghana that means hosting in Accra or at worst
in Europe, not in us-east-1: every relayed packet takes the round trip twice,
and a 200 ms detour is plainly audible as a conversational stumble.

## Carrier setup

Ghanaian carriers and VoIP wholesalers generally offer either IP allowlisting
or registration.

**IP allowlisting** is preferable when you have a static address: there are no
credentials to leak or rotate.

```bash
PAYVOICE_SIP_ENABLED=true
PAYVOICE_SIP_TRUNK=sip:trunk.carrier.com.gh:5060
PAYVOICE_SIP_IDENTITY=233302000000
PAYVOICE_SIP_REGISTER=false
```

**Registration** is needed on a dynamic address:

```bash
PAYVOICE_SIP_REGISTER=true
PAYVOICE_SIP_REGISTRAR=sip:trunk.carrier.com.gh
PAYVOICE_SIP_USERNAME=your-account
PAYVOICE_SIP_PASSWORD=your-password
```

PayVoice re-registers at half the granted expiry and retries with exponential
backoff. `GET /v1/stats` reports `sip.registered` and the last error.

### Codec

Set `PAYVOICE_CODEC=pcma` for A-law, which is the norm across Africa and Europe.
It only affects calls PayVoice originates: for inbound calls it honours whatever
the peer offers, preferring the first G.711 variant in their `m=` line. That
ordering matters — answering with mu-law when a gateway listed A-law first is a
classic cause of one-way audio.

### Keepalives

Carriers send `OPTIONS` to check the trunk is alive and mark it down if you do
not reply. PayVoice answers these automatically.

## Tuning

`PAYVOICE_JITTER_TARGET` trades latency against resilience. 60 ms suits a good
link; raise it to 120 ms or more for mobile data, which is what most Ghanaian
callers are on.

Watch `GET /v1/stats`:

- `engine.late_ticks` climbing means the process is starved and audio is
  stuttering on **every** call. Give it more CPU before anything else.
- Per-leg `jitter.Late` and `jitter.Resync` climbing means the target is too
  low for that specific network.
- `webhooks.dropped` above zero means your receiver cannot keep up, and events
  are being discarded to protect audio.

## Scaling

PayVoice is stateful: a leg lives in the process that owns its socket, so calls
cannot move between instances.

To scale horizontally, run several instances and have your application place
both legs of a bridged call on the same one. Route by room. There is no
clustering or inter-node mixing, and adding it would mean forwarding audio
between nodes, which costs a network round trip per frame.

One instance handles hundreds of concurrent calls on a modest VM; the mixer is
integer arithmetic. Capacity is bounded first by the RTP port range, then by
CPU.

## Production checklist

- [ ] `PAYVOICE_ENV=production` (this enables the guardrails below)
- [ ] `PAYVOICE_API_TOKENS` set, 32+ characters, from `openssl rand -hex 32`
- [ ] `PAYVOICE_WEBHOOK_SECRET` set, 32+ characters, different from the tokens
- [ ] `PAYVOICE_WEBHOOK_URL` is https
- [ ] Control plane bound to loopback or a private network, never public
- [ ] `PAYVOICE_SIP_PUBLIC_HOST` and `PAYVOICE_WEBRTC_PUBLIC_IP` set to the public address
- [ ] UDP port ranges open and matching the configuration exactly
- [ ] SIP port restricted to carrier source addresses
- [ ] TURN server deployed and regionally close to users
- [ ] SIP traffic on a private link or VPN — SIP legs are plain RTP, not SRTP
- [ ] `/readyz` wired to the load balancer, not just `/healthz`

`Config.Validate` refuses to start in production without the first four, which
turns the most dangerous misconfigurations into a startup failure rather than a
silent vulnerability.
