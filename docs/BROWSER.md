# Browser client

PayVoice negotiates **G.711 only**. Browsers support it, but they offer Opus
first and will happily agree to it if you let them, so the client has to be
told what to offer. This is the whole difference between a working client and
a `no_common_codec` error.

## Minimal client

```js
async function connect(apiBase, token, roomId) {
  const pc = new RTCPeerConnection({
    // Fetch these from GET /v1/config rather than hardcoding, so ICE is
    // configured in one place.
    iceServers: [{ urls: 'stun:stun.l.google.com:19302' }],
  });

  const mic = await navigator.mediaDevices.getUserMedia({
    audio: {
      // The browser is about to resample to 8 kHz anyway. Doing this here
      // gives its own echo canceller and noise suppression the narrowband
      // signal they will actually be working with.
      echoCancellation: true,
      noiseSuppression: true,
      autoGainControl: true,
    },
  });

  const [track] = mic.getAudioTracks();

  // Create the transceiver explicitly so there is a handle to set codec
  // preferences on. This is the line that makes the whole thing work.
  const transceiver = pc.addTransceiver(track, {
    direction: 'sendrecv',
    streams: [mic],
  });
  preferG711(transceiver);

  // Play what PayVoice sends back.
  const audio = new Audio();
  audio.autoplay = true;
  pc.ontrack = (e) => { audio.srcObject = e.streams[0]; };

  const offer = await pc.createOffer();
  await pc.setLocalDescription(offer);
  await iceGathered(pc);

  const res = await fetch(`${apiBase}/v1/webrtc/offer`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({ sdp: pc.localDescription.sdp, room_id: roomId }),
  });

  if (!res.ok) throw new Error((await res.json()).error.message);
  const { sdp, leg_id } = await res.json();

  await pc.setRemoteDescription({ type: 'answer', sdp });

  return {
    pc,
    legId: leg_id,
    sender: transceiver.sender,
    stop: () => { pc.close(); track.stop(); },
  };
}
```

## Forcing G.711

`setCodecPreferences` reorders what the browser offers. Put G.711 first and
Opus never gets chosen.

```js
function preferG711(transceiver) {
  const { codecs } = RTCRtpReceiver.getCapabilities('audio');

  // telephone-event must be kept alongside the codec: it is how DTMF reaches
  // PayVoice, and DTMF is the only input channel that cannot be misheard.
  const wanted = codecs.filter((c) =>
    /pcmu|pcma|telephone-event/i.test(c.mimeType)
  );

  if (!wanted.some((c) => /pcmu|pcma/i.test(c.mimeType))) {
    throw new Error('This browser does not offer G.711');
  }

  transceiver.setCodecPreferences(wanted);
}
```

Listing only the wanted codecs removes everything else from the offer entirely,
which is stronger than merely ranking G.711 first.

## Waiting for candidates

PayVoice returns an answer with its candidates already gathered, so the simplest
correct client gathers its own before offering:

```js
function iceGathered(pc) {
  if (pc.iceGatheringState === 'complete') return Promise.resolve();

  return new Promise((resolve) => {
    // Never wait forever: a blocked STUN server would hang the call setup.
    const timer = setTimeout(done, 2000);

    function done() {
      clearTimeout(timer);
      pc.removeEventListener('icegatheringstatechange', check);
      resolve();
    }
    function check() {
      if (pc.iceGatheringState === 'complete') done();
    }

    pc.addEventListener('icegatheringstatechange', check);
  });
}
```

To trickle instead, post each candidate as it appears:

```js
pc.onicecandidate = ({ candidate }) => {
  if (!candidate || !legId) return;
  fetch(`${apiBase}/v1/legs/${legId}/ice-candidates`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({ candidate }),
  });
};
```

## Sending DTMF

For anything that authorises money, collect digits from the keypad rather than
from speech. A keypress cannot be misheard, and it does not leak the PIN to
anyone standing nearby.

```js
const { sender } = await connect(apiBase, token, roomId);
sender.dtmf.insertDTMF('4071', 160, 70);   // digits, tone ms, gap ms
```

PayVoice deduplicates the resulting packet burst and emits exactly one
`dtmf.received` event per keypress.

## Falling back to a phone call

When the browser cannot hold a data path, ask PayVoice to call the user instead
and put that call in the same room. The user experiences an ordinary incoming
phone call; the agent never notices the difference.

```js
async function fallbackToPhone(apiBase, token, roomId, phoneNumber) {
  const res = await fetch(`${apiBase}/v1/legs`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: `Bearer ${token}`,
    },
    body: JSON.stringify({ type: 'sip', to: phoneNumber, room_id: roomId }),
  });
  return res.json();
}
```

Trigger it on connection failure:

```js
pc.onconnectionstatechange = () => {
  if (pc.connectionState === 'failed') {
    fallbackToPhone(apiBase, token, roomId, userPhoneNumber);
  }
};
```

Note that this call is placed by your application server, not the browser: the
control plane token must never reach the client. The snippets above show the
token inline for brevity, but in production the browser should talk to your own
backend, which holds the token and proxies to PayVoice.

## Troubleshooting

| Symptom | Cause |
| --- | --- |
| `no_common_codec` | The offer was Opus-only. `setCodecPreferences` was not applied, or was applied to the wrong transceiver. |
| Connects, but silence both ways | ICE picked an unreachable candidate. Set `PAYVOICE_WEBRTC_PUBLIC_IP` on a cloud VM, and open the UDP port range. |
| Audio one way only | Almost always NAT. Add a TURN server. |
| DTMF never arrives | `telephone-event` was filtered out of the codec preferences. |
| Choppy audio | Check `engine.late_ticks` in `GET /v1/stats`, then raise `PAYVOICE_JITTER_TARGET`. |
