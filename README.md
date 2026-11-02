# mrcpgo

`mrcpgo` is a small, dependency-light **MRCPv2 (RFC 6787) media resource server**
written in Go. It acts as a SIP UAS that negotiates an RTP audio stream and one
or more MRCPv2 control channels with a caller such as
[FreeSWITCH](https://freeswitch.org) `mod_unimrcp`, and serves the
`speechrecog` and `speechsynth` resources over a self-contained MRCPv2 text
protocol implementation.

The speech engines are pluggable. A deterministic mock engine ships with the
server so the whole call path can be exercised end to end without an ASR or TTS
backend. Three cloud engines provide real recognition and synthesis over their
native WebSocket protocols: **Alibaba Cloud Intelligent Speech (NLS)**,
**Tencent Cloud** (real-time recognition v2 and streaming synthesis) and
**iFlytek** (real-time transcription and online synthesis).

---

## Table of contents

- [Features](#features)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Speech engines](#speech-engines)
- [FreeSWITCH `mod_unimrcp` integration](#freeswitch-mod_unimrcp-integration)
- [Architecture](#architecture)
- [Protocol behaviour](#protocol-behaviour)
- [Testing](#testing)
- [Implementation notes and deviations](#implementation-notes-and-deviations)
- [Design decisions](#design-decisions)
- [Limitations](#limitations)

---

## Features

| Layer | Implemented |
|---|---|
| SIP | UAS on UDP: `INVITE` → `100 Trying` → `200 OK`, `ACK`, `BYE`, `CANCEL` (487 via the transaction layer), `OPTIONS` |
| SDP | Minimal RFC 4566 offer/answer: `m=audio`, `m=application TCP/MRCPv2`, `a=channel`, `a=setup`, `a=cmid`, `a=mid`, `c=`, `a=rtpmap` |
| MRCPv2 | Length-prefixed codec; `SET-PARAMS`, `GET-PARAMS`, `DEFINE-GRAMMAR`, `RECOGNIZE`, `START-INPUT-TIMERS`, `STOP`, `SPEAK`; events `START-OF-INPUT`, `RECOGNITION-COMPLETE`, `SPEAK-COMPLETE` |
| RTP | UDP + `pion/rtp`, G.711 mu-law (PT 0), 20 ms frames from a 20 ms ticker, sequence/timestamp/SSRC maintenance, loss and reordering counters |
| Sessions | One SIP dialog per session, deterministic teardown on BYE, control-channel disconnect and process shutdown |
| Engines | `Recognizer` / `Synthesizer` interfaces; deterministic mock engine, Alibaba Cloud NLS engine, Tencent Cloud engine (ASR v2 + streaming TTS), iFlytek engine (rtasr + online TTS) |
| Ops | Structured `slog` logging, optional `/healthz` endpoint, YAML configuration with `${ENV}` secret expansion |

Out of scope by design: SIP registration, digest authentication, proxying,
TLS/SRTP/DTLS, SCTP, recorder and speaker-verification resources, VAD based
`START-OF-INPUT`, jitter buffering and RTCP.

## Requirements

- **Go 1.27.0** or newer. The project uses the standard library `uuid` package
  (RFC 9562) and the `runtime/pprof` `goroutineleak` profile introduced in Go
  1.27.

Direct dependencies, pinned in `go.mod`:

| Module | Version | Used for |
|---|---|---|
| `github.com/emiago/sipgo` | `v1.6.0` | SIP transport, transactions and dialogs |
| `github.com/pion/rtp` | `v1.10.6` | RTP packet marshal/unmarshal |
| `github.com/gorilla/websocket` | `v1.5.3` | Alibaba Cloud NLS WebSocket transport |
| `gopkg.in/yaml.v3` | `v3.0.1` | Configuration parsing |

## Quick start

```bash
# install the binary from the module path
go install github.com/qtstack/mrcpgo/cmd/mrcpgo@latest

# or build and test from a checkout
go build ./...
go test ./...

# run with the example configuration
go run ./cmd/mrcpgo -config config.example.yaml
```

The server logs its SIP listen address at startup. With the example
configuration it listens on `0.0.0.0:5060/udp` and derives the advertised
address from the interface used to reach each caller.

A health endpoint can be enabled with `http.listen`:

```yaml
http:
  listen: "127.0.0.1:8080"
```

```bash
curl -s http://127.0.0.1:8080/healthz
# ok
# sessions=0
```

## Configuration

`config.example.yaml` documents every key. Loading a file merges it over the
built-in defaults and rejects unknown keys so typos fail at startup:

```yaml
log:
  level: info          # debug | info | warn | error
  format: text         # text | json

sip:
  listen: "0.0.0.0:5060"
  advertise_addr: ""   # empty = derive from the route to the peer
  user_agent: "mrcpgo/0.1"
  contact_user: "mrcpgo"
  server_name: "mrcpgo"

rtp:
  listen_host: "0.0.0.0"

mrcp:
  listen_host: "0.0.0.0"
  engine:
    type: mock            # mock | alicloud
    recognition_delay: 200ms
    frames_per_utterance: 50
    # See the Speech engines section for the alicloud block.

http:
  listen: ""
```

Only the SIP listener is fixed; the RTP UDP port and every MRCP TCP port are
allocated per call (`:0`) and advertised in the SDP answer.

Values may reference the environment with `${NAME}` so credentials stay out of
the file:

```yaml
sip:
  # ...
mrcp:
  engine:
    type: alicloud
    alicloud:
      access_key_secret: ${ALIYUN_ACCESS_KEY_SECRET}
```

Unresolved references are left as-is and fail validation with a clear message.

## Speech engines

### mock

Deterministic engine used by the tests and for wiring-free experiments. It
returns a fixed NLSML document and produces one second of mu-law silence per
SPEAK.

### Alibaba Cloud Intelligent Speech Interaction (NLS)

Select it with `mrcp.engine.type: alicloud`. Recognition and synthesis use the
NLS WebSocket protocol: JSON control frames carrying `header`/`payload` and raw
binary PCM audio frames, with the same message names as the official SDK
(`StartRecognition`, `RecognitionStarted`, `RecognitionResultChanged`,
`RecognitionCompleted`, `StartSynthesis`, `MetaInfo`, `SynthesisCompleted`,
`TaskFailed`).

```yaml
mrcp:
  engine:
    type: alicloud
    alicloud:
      app_key: ""                              # console appkey
      access_key_id: ${ALIYUN_ACCESS_KEY_ID}
      access_key_secret: ${ALIYUN_ACCESS_KEY_SECRET}
      token: ""                                # optional static token
      region: cn-shanghai
      endpoint: ""                             # default wss://nls-gateway.<region>.aliyuncs.com/ws/v1
      token_endpoint: ""                       # default https://nls-meta.<region>.aliyuncs.com
      token_refresh_before: 5m
      timeout: 5s
      asr:
        format: pcm                            # only pcm
        sample_rate: 8000                      # 8000 or 16000
        enable_intermediate_result: false
        enable_punctuation_prediction: true
        enable_inverse_text_normalization: true
        chunk_ms: 100
        params: {}                             # merged into StartRecognition
      tts:
        voice: xiaoyun
        format: pcm                            # only pcm
        sample_rate: 16000                     # 8000 or 16000
        volume: 50
        speech_rate: 0
        pitch_rate: 0
        long_text: false
        params: {}                             # merged into StartSynthesis
```

How the bridge works:

- **Tokens.** With `token` unset, the server calls `CreateToken` on the NLS meta
  API using the AccessKey pair, signs the request with the Alibaba Cloud RPC
  HMAC-SHA1 scheme, and caches the result until `token_refresh_before` before
  expiry. A failed refresh keeps using the previous token while it is still
  valid. One token source is shared by every channel.
- **Recognition.** `RECOGNIZE` opens one WebSocket per request, sends
  `StartRecognition`, then streams binary PCM. Inbound 20 ms mu-law RTP frames
  are decoded to 8 kHz PCM and upsampled to 16 kHz when configured; audio is
  buffered into `chunk_ms` frames and dropped (with a counter and warning) if
  the upstream queue is full, so the RTP loop is never blocked. Audio received
  while no recognition is active is discarded.
- **Results.** `RecognitionCompleted` becomes a `RECOGNITION-COMPLETE` event
  with an NLSML body; an empty result reports Completion-Cause `001 no-match`.
  `TaskFailed` and transport errors report the failure to the MRCP layer.
  Intermediate results are logged at debug level.
- **Synthesis.** `SPEAK` opens one WebSocket, sends `StartSynthesis` with the
  text, and converts the returned PCM to 8 kHz mu-law frames. 16 kHz output is
  downsampled by averaging sample pairs before encoding. `SynthesisCompleted`
  becomes `SPEAK-COMPLETE`; `TaskFailed` ends the stream with an error.
- **Cancellation.** MRCP `STOP`, BYE and shutdown cancel the request context,
  send `StopRecognition` best effort, and close the socket so the service stops
  billing immediately.
- **Grammar URIs.** Cloud recognition has no SRGS concept. The grammar URI is
  logged and ignored; model or domain selection belongs in
  `alicloud.asr.params` (for example `vocabulary_id`, `customization_id` or
  `max_sentence_silence`).

A note on `START-OF-INPUT`: like the mock engine, the Alibaba adapter is driven
by the channel, which reports `START-OF-INPUT` when recognition begins. The NLS
real-time protocol has no speech-onset event, and adding local VAD is future
work.

### Tencent Cloud

Select it with `mrcp.engine.type: tencent`. The engine speaks two Tencent Cloud
WebSocket APIs directly:

- **Real-time speech recognition v2**:
  `wss://asr.cloud.tencent.com/asr/v2/<app_id>`, HMAC-SHA1 signed query
  parameters, binary PCM frames, JSON results carrying
  `sentences.sentence_list`.
- **Streaming text-to-speech v2**:
  `wss://tts.cloud.tencent.com/stream_wsv2` with
  `Action=TextToStreamAudioWSv2`: wait for `READY`, send `ACTION_SYNTHESIS` and
  `ACTION_COMPLETE`, receive binary PCM, finish on `FINAL`.

```yaml
mrcp:
  engine:
    type: tencent
    tencent:
      app_id: ""                               # numeric AppId, quoted
      secret_id: ${TENCENT_SECRET_ID}
      secret_key: ${TENCENT_SECRET_KEY}
      asr_endpoint: ""                         # default wss://asr.cloud.tencent.com/asr/v2/<app_id>
      tts_endpoint: ""                         # default wss://tts.cloud.tencent.com/stream_wsv2
      timeout: 5s
      asr:
        engine_model_type: 16k_zh
        sample_rate: 16000                     # 8000 or 16000
        need_vad: true
        vad_silence_time: 0                    # ms, 0 = service default
        convert_num_mode: 1
        chunk_ms: 100
        params: {}                             # hotword_id, customization_id, hotword_list, ...
      tts:
        voice_type: 0
        sample_rate: 16000                     # 8000 or 16000
        speed: 0                               # -2..6
        volume: 0                              # -10..10
        emotion_category: ""
        emotion_intensity: 0                   # 50..200, needs a category
        segment_rate: 0                        # 0..2
        params: {}
```

How the bridge works:

- **Signing.** The two APIs use different signature inputs: ASR signs
  `<host><path>?<sorted query>`, TTS signs
  `GET<host><path>?<sorted query>`. Both are HMAC-SHA1 and Base64 encoded,
  implemented locally and checked against golden vectors in the tests. The
  official Tencent speech SDK is not imported: it pins `gorilla/websocket
  v1.4.2` and `google/uuid`, which this project already avoids.
- **Recognition handshake.** `Start` reads the handshake synchronously, so a bad
  signature or parameter surfaces as a failed `RECOGNIZE` (500) instead of a
  late event.
- **Recognition completion.** Tencent v2 reports sentence state with
  `sentence_type` (0 uncertain, 1 definite). The first definite sentence
  completes the request: one MRCP `RECOGNIZE` maps to one utterance, which is
  what `mod_unimrcp` expects. `final=1` with no text reports `001 no-match`.
  The v2 API does not return a confidence score, so the NLSML `confidence`
  attribute is omitted rather than fabricated. Completion relies on server side
  VAD, so keep `need_vad: true` unless the client always sends `STOP`.
- **Model and sample rate.** `sample_rate` is the PCM rate sent upstream and
  `engine_model_type` is the recognition model. Pair `8k_zh` with `8000`, or
  keep the default `16k_zh` with `16000` (telephony audio is upsampled). When
  sending 8 kHz PCM to a 16 kHz engine, add
  `params: { input_sample_rate: "8000" }` to let the service upsample instead.
- **Speech onset.** `sentence_type=0` is Tencent's "speech detected" signal, but
  it is only logged for now; like the other engines, `START-OF-INPUT` is sent
  when recognition starts.
- **Synthesis.** The adapter waits for `READY`, submits the whole text followed
  by `ACTION_COMPLETE`, and converts the returned PCM to 8 kHz mu-law
  (downsampling by averaging sample pairs when configured for 16 kHz). `FINAL`
  ends the stream.
- **Cancellation.** `STOP`, BYE and shutdown cancel the context and close the
  socket; recognition also sends a best effort `{"type":"end"}` frame.

Only the streaming-text v2 synthesis API is used. The basic HTTP `TextToVoice`
API (150 character limit, non-streaming response) and the older
`TextToStreamAudioWS` endpoint do not fit MRCP `SPEAK` as well and are not
implemented.

### iFlytek (讯飞)

Select it with `mrcp.engine.type: xfyun`. Two WebSocket APIs are used:

- **Real-time transcription (rtasr)**: `wss://rtasr.xfyun.cn/v1/ws` signed with
  `signa = Base64(HMAC-SHA1(MD5(app_id + ts), api_key))`, 16 kHz PCM in 40 ms
  frames, and JSON results whose `data` field is a nested JSON string holding
  `cn.st.rt[].ws[].cw[].w` words and `st.type` (`0` final, `1` interim).
- **Online speech synthesis**: `wss://tts-api.xfyun.cn/v2/tts` with the
  HMAC-SHA256 `authorization` handshake, one JSON frame
  (`common`/`business`/`data`) carrying base64 text, and base64 PCM in
  `data.audio` until `data.status == 2`.

```yaml
mrcp:
  engine:
    type: xfyun
    xfyun:
      app_id: ""
      api_key: ${XFYUN_API_KEY}
      api_secret: ${XFYUN_API_SECRET}
      asr_endpoint: ""                         # default wss://rtasr.xfyun.cn/v1/ws
      tts_endpoint: ""                         # default wss://tts-api.xfyun.cn/v2/tts
      timeout: 5s
      asr:
        sample_rate: 16000                     # rtasr requires 16 kHz
        lang: cn
        chunk_ms: 40                           # 1280 bytes at 16 kHz
        params: {}                             # pd, transType, ...
      tts:
        voice: xiaoyan                         # vcn
        sample_rate: 16000                     # 8000 or 16000
        speed: 50                              # 0..100
        volume: 50                             # 0..100
        pitch: 50                              # 0..100
        params: {}                             # bgs, reg, rdn, ...
```

How the bridge works:

- **Signing.** The rtasr `signa` and the TTS `authorization` handshake are
  implemented locally and checked against golden vectors: the documented
  iFlytek `signa` example, and an independently computed HMAC-SHA256 value for
  the synthesis signature.
- **Recognition.** The `started` handshake is read synchronously so a bad
  signature fails the `RECOGNIZE` instead of producing a late event. Inbound
  20 ms mu-law RTP frames are decoded to PCM and upsampled to 16 kHz; audio is
  buffered into `chunk_ms` frames (40 ms / 1280 bytes by default) and dropped
  with a counter if the upstream queue is full. The first final segment
  (`st.type == "0"`) completes the request, one utterance per `RECOGNIZE`.
  Interim segments are logged at debug level.
- **Synthesis.** The whole text is sent as a single final frame
  (`data.status = 2`) with `aue=raw` and `auf=audio/L16;rate=<sample_rate>`.
  Base64 audio is decoded, converted to 8 kHz mu-law (downsampling 16 kHz by
  averaging sample pairs) and streamed to the RTP sender. `data.status == 2`
  ends the stream.
- **Cancellation.** `STOP`, BYE and shutdown cancel the context and close the
  socket; transcription also sends the binary `{"end": true}` marker best
  effort.
- **Confidence.** rtasr does not report a confidence score, so the NLSML
  `confidence` attribute is omitted.

## FreeSWITCH `mod_unimrcp` integration

`mrcpgo` expects the standard FreeSWITCH offer: an audio section plus one
`m=application TCP/MRCPv2` section per resource, each with
`a=setup:passive`, `a=connection:new` and `a=channel:<id>@<resource>`. The
server answers with its own RTP port and one listening MRCP port per control
channel.

Example `conf/autoload_configs/unimrcp.conf.xml`:

```xml
<configuration name="unimrcp.conf" description="UniMRCP Client">
  <settings>
    <param name="default-tts-profile" value="mrcp-synth"/>
    <param name="default-asr-profile" value="mrcp-recog"/>
    <param name="log-level" value="info"/>
  </settings>
  <profiles>
    <profile name="mrcp-synth" type="synth">
      <param name="server-ip" value="127.0.0.1"/>
      <param name="server-port" value="5060"/>
      <param name="sip-transport" value="udp"/>
      <param name="speechsynth" value="speechsynth"/>
      <param name="rtp-ip" value="auto"/>
      <param name="rtp-port-min" value="4000"/>
      <param name="rtp-port-max" value="5000"/>
      <param name="codecs" value="PCMU"/>
      <param name="synthparams" value=""/>
    </profile>
    <profile name="mrcp-recog" type="recog">
      <param name="server-ip" value="127.0.0.1"/>
      <param name="server-port" value="5060"/>
      <param name="sip-transport" value="udp"/>
      <param name="speechrecog" value="speechrecog"/>
      <param name="rtp-ip" value="auto"/>
      <param name="rtp-port-min" value="4000"/>
      <param name="rtp-port-max" value="5000"/>
      <param name="codecs" value="PCMU"/>
      <param name="recogparams" value=""/>
    </profile>
  </profiles>
</configuration>
```

After `reloadxml`, verify the registration and run a call. The exact
application syntax depends on the FreeSWITCH version; run `fs_cli -x "help"` to
confirm:

```bash
# synthesizer: speak a sentence through the mock engine (one second of audio)
fs_cli -x "originate user/1001 &speak(unimrcp,kal,hello from mrcpgo)"

# recognizer: run grammar detection; the mock returns a fixed NLSML result
fs_cli -x "originate user/1001 &detect_speech(unimrcp,builtin:grammar/digits)"
```

A dialplan equivalent:

```xml
<extension name="mrcp-demo">
  <condition field="destination_number" expression="^5000$">
    <action application="answer"/>
    <action application="speak" data="unimrcp|kal|hello from mrcpgo"/>
  </condition>
</extension>
```

Set `sip.advertise_addr` when the server is not reachable at the address of the
interface selected by the routing table (multi-homed hosts, NAT, containers).

## Architecture

Dependencies point in one direction only; there are no import cycles.

```
cmd/mrcpgo                       process entry point, configuration, signals
    │
internal/server                 SIP UAS: handlers, SDP negotiation, allocations
    │
internal/session                aggregate root: lifecycle and resource release
    ├── internal/sdp            offer parsing and answer generation
    ├── internal/mrcp           MRCPv2 codec, channel state machine, dispatch
    ├── internal/media          RTP transceiver (UDP, PCMU)
    └── internal/engine         engine selection and implementations
        ├── mock                deterministic mock engines
        ├── alicloud            Alibaba Cloud NLS WebSocket engine
        ├── tencent             Tencent Cloud ASR v2 and streaming TTS
        └── xfyun               iFlytek rtasr and online TTS
internal/audio                  G.711, resampling and chunking helpers
internal/nlsml                  NLSML document builder
internal/wsutil                 serialised WebSocket writer
internal/model                  pure data types and engine interfaces
internal/config                 YAML configuration and logging setup
internal/syncx                  bounded wait-group helper
```

`internal/model` imports only the standard library and defines the engine
contracts (`Recognizer`, `Synthesizer`, optional `AudioConsumer`), which keeps
`session` and `engine` free of circular dependencies.

### Session lifecycle

```
INVITE ──▶ allocate RTP + control listeners ──▶ 200 OK (blocks until ACK)
                                                        │
                                              ACK ──▶ ACTIVE
                                                        │
BYE / control TCP close / shutdown ──▶ RELEASING ──▶ CLOSED
```

Control channels are accepted as soon as the listeners exist, but requests are
gated until the dialog is confirmed by an ACK. `Session.Close` runs a fixed
release sequence: refuse new work, stop media, cancel in-flight MRCP requests
and close control connections, close listeners, wait for goroutines (bounded),
unregister, publish `CLOSED`, then detach the SIP dialog. It is idempotent and
safe to call concurrently; the first caller blocks until teardown completes.

### MRCP framing

MRCPv2 is length prefixed, not line framed. The decoder reads the start line,
extracts `message-length` (the total size of the message including the start
line itself), reads exactly the remaining bytes, then splits headers from body
and validates `Content-Length`. `message-length` is never trusted for
allocation: a configurable limit (1 MiB by default) is enforced before reading.

## Protocol behaviour

### SIP

| Request | Behaviour |
|---|---|
| `INVITE` | `100 Trying`, then `200 OK` with SDP. `488 Not Acceptable Here` when the offer has no usable audio stream or no MRCPv2 control channel; `415 Unsupported Media Type` when the body is not `application/sdp` |
| `ACK` | Confirms the dialog and activates the session |
| `BYE` | `200 OK`, then deterministic session teardown |
| `CANCEL` | Handled by the transaction layer: `200 OK` to the CANCEL, `487 Request Terminated` to the INVITE; pre-allocated resources are released |
| `OPTIONS` | `200 OK` with `Allow: INVITE, ACK, BYE, CANCEL, OPTIONS` |

### MRCPv2

| Method | Behaviour |
|---|---|
| `SET-PARAMS`, `GET-PARAMS`, `START-INPUT-TIMERS` | `200 Success`, `COMPLETE` |
| `DEFINE-GRAMMAR` | `200 Success` for `application/srgs+xml` and `text/uri-list`; `501 Not Implemented` otherwise |
| `RECOGNIZE` | `200 Success`, `IN-PROGRESS`, then `START-OF-INPUT` and `RECOGNITION-COMPLETE` (NLSML) |
| `SPEAK` | `200 Success`, `IN-PROGRESS`, 20 ms PCMU frames, then `SPEAK-COMPLETE` |
| `STOP` | Terminates the active request, `200 Success`, `COMPLETE`, no completion event |
| anything else | `501 Not Implemented` |

Status codes used: `200`, `400` (missing header/body), `402` (method not valid
in this state), `406` (unknown `Channel-Identifier`), `501`, `500`.

### RTP

PCMU (payload type 0), 20 ms frames of 160 bytes. Outbound packets carry a
random non-zero SSRC and random initial sequence number and timestamp;
`sequence` advances by one and `timestamp` by 160 per frame. Reception records
packets received, lost and out of order; anomalies are logged at debug level
and are not reordered.

## Testing

```bash
go test ./...                     # unit tests
go test -race ./...               # unit tests with the race detector
go test -tags=integration ./...   # end-to-end SIP + MRCP + RTP, no FreeSWITCH needed
go vet ./...
gofmt -l .                        # must print nothing
```

The integration test starts the real server on loopback, drives a full call with
a sipgo UAC (INVITE → 200 OK → ACK → RECOGNIZE/SPEAK over the MRCPv2 control
channel → BYE), verifies that RTP frames arrive, that every port is released and
that `pprof.Lookup("goroutineleak")` reports no leaked goroutines.

`TestCallWithAliCloudEngine` repeats the call with the Alibaba engine pointed at
a fake NLS WebSocket server, covering token handling, the recognition control
frame exchange, audio forwarding and the synthesis stream without cloud
credentials. `TestCallWithTencentEngine` and `TestCallWithXfyunEngine` do the
same for the Tencent and iFlytek engines against fake provider endpoints.

Engine unit tests use `httptest` servers that speak the provider WebSocket
protocols, so protocol regressions are caught without network access. All
signature implementations are checked against golden vectors: the Alibaba Cloud
RPC example, the iFlytek `signa` example, and independently computed HMAC-SHA1
/ HMAC-SHA256 values for the Tencent ASR, Tencent TTS and iFlytek TTS
constructions.

`testdata/mrcp_*.txt` are wire-format golden files with CRLF line endings;
`.gitattributes` pins their line endings so the round-trip test is stable.

### Live cloud checks

Alibaba Cloud:

```bash
export ALIYUN_ACCESS_KEY_ID=...
export ALIYUN_ACCESS_KEY_SECRET=...
export ALIYUN_NLS_APP_KEY=...
# edit a copy of config.example.yaml: type: alicloud, app_key: ${ALIYUN_NLS_APP_KEY}
go run ./cmd/mrcpgo -config alicloud.yaml
```

Tencent Cloud:

```bash
export TENCENT_SECRET_ID=...
export TENCENT_SECRET_KEY=...
# edit a copy of config.example.yaml: type: tencent and app_id: "<your appid>"
go run ./cmd/mrcpgo -config tencent.yaml
```

iFlytek:

```bash
export XFYUN_API_KEY=...
export XFYUN_API_SECRET=...
# edit a copy of config.example.yaml: type: xfyun and app_id: "<your appid>"
go run ./cmd/mrcpgo -config xfyun.yaml
```

Then place a call with `fs_cli` and watch for `NLS token obtained`,
`NLS recognition completed`, `NLS synthesis completed`, `Tencent recognition
completed`, `Tencent synthesis completed`, `iFlytek transcription completed` or
`iFlytek synthesis completed` in the log.

## Implementation notes and deviations

The implementation follows `MRCPD_TASK.md`, with the following deliberate
deviations where the document no longer matched the libraries or RFC 6787:

1. **sipgo dialog API.** sipgo `v1.6.0` has no `NewDialogServer`. The UAS corner
   is `sipgo.DialogUA.ReadInvite`, so the server keeps its own `Call-ID →
   *DialogServerSession` map (the library recommends building your own cache)
   and routes `ACK`/`BYE` through it.
2. **`Respond` blocks.** `DialogServerSession.Respond(status, reason, body,
   headers...)` does not take a `*sip.Response`, and it retransmits the 200 OK
   until the ACK arrives (up to 64·T1). sipgo dispatches every inbound request
   in its own goroutine, so blocking inside `OnInvite` does not block ACK
   handling; the session is activated right after `Respond` returns.
3. **`ReadBye` answers itself.** `DialogServerSession.ReadBye` sends the `200
   OK` to the BYE. The handler must not respond a second time.
4. **MRCP start lines follow RFC 6787 exactly.**
   - request: `MRCP/2.0 <message-length> <method-name> <request-id>`
   - response: `MRCP/2.0 <message-length> <request-id> <status-code> <request-state>`
   - event: `MRCP/2.0 <message-length> <event-name> <request-id> <request-state>`

   The task document's `event <event-name>` form is not RFC compliant and is
   neither produced nor accepted.
5. **Server-side request cancellation is not a wire `STOP`.** `STOP` is a
   client-to-server request. When the server tears down a channel it cancels
   the request context and closes the control connection, which terminates every
   request on that connection (RFC 6787 §4.3).
6. **One audio stream.** Only the first `m=audio` section is accepted; further
   audio sections and unsupported media types are answered with port `0` while
   preserving m-line order (RFC 3264 §6).
7. **`START-OF-INPUT` is emitted immediately** by the channel because the mock
   recognizer has no VAD. A real engine would emit it on speech onset.
8. **`Content-Length` is mandatory for bodies.** A body without `Content-Length`,
   or with a length that disagrees with the body, is rejected.
9. **Channel identifiers may be allocated by the server.** When the client sends
   `a=channel:@speechrecog` (no session-id), the server generates a 32 character
   hexadecimal id with the standard library `uuid` package and returns it in the
   answer. RFC 6787 allows alphanumerics and `_` only, so the dashes are
   stripped.
10. **sipgo's package-level logger is aligned** with the configured logger in
    `server.New`, because a few transport-layer paths bypass the injected
    loggers.

## Design decisions

All non-obvious choices are marked `// DECISION:` in the source. Summary:

- The MRCP codec is length-prefix driven and enforces a size limit before
  allocating, because `message-length` is peer controlled.
- `internal/mrcp` declares a narrow `FrameSender` interface instead of importing
  `internal/media`, and `internal/session` declares a narrow `Dialog` interface
  instead of importing `sipgo`; both keep the core testable without a network or
  a SIP stack.
- A fresh engine is created per control channel rather than shared per session,
  so two recognizer channels in one dialog cannot corrupt each other's state.
- Inbound RTP payloads are offered to recognizers through the optional
  `model.AudioConsumer` interface; engines that do not implement it are valid
  (the mock ignores audio).
- The Alibaba engine implements the NLS protocol directly over
  `gorilla/websocket` instead of importing `alibabacloud-nls-go-sdk`. The
  official Go SDK has not been released since 2023, pins
  `gorilla/websocket v1.4.2` and the archived `satori/go.uuid`, and drags in the
  full `alibaba-cloud-sdk-go`. Speaking the protocol directly keeps the
  dependency surface to one module and gives exact control over per-request
  connection lifetimes and cancellation.
- The Tencent engine does the same instead of importing
  `tencentcloud-speech-sdk-go`, which also pins `gorilla/websocket v1.4.2` and
  `google/uuid`. Both Tencent signature schemes (ASR and TTS) are implemented
  locally with golden-vector tests.
- The iFlytek engine implements rtasr and the online synthesis handshake
  directly; the two auth schemes (`signa` for ASR, HMAC-SHA256
  `authorization` for TTS) are local code with golden-vector tests.
- `CreateToken` is implemented locally (RPC HMAC-SHA1 signature, verified
  against the documented example vector) with a single shared token cache,
  instead of pulling an OpenAPI SDK for one call.
- Upstream audio is queued with a bounded buffer and dropped with a counter when
  full, so a slow cloud connection can never stall the RTP receive loop.
- RTP starts with a random SSRC, sequence number and timestamp (RFC 3550).
- Sessions wait for goroutines with a 5 second bound (`internal/syncx`) so a
  misbehaving engine cannot hang shutdown forever.

## Limitations

- UDP SIP only; no TCP/TLS transport, no RFC 3261 authentication, no re-INVITE.
- No RTCP, jitter buffer or packet reordering.
- Only the mock, Alibaba Cloud, Tencent Cloud and iFlytek engines are built in;
  other providers need their own `model.Recognizer` / `model.Synthesizer`
  implementation.
- The Alibaba engine supports PCM at 8 kHz and 16 kHz only. It does not use
  SRGS grammars (`DEFINE-GRAMMAR` is accepted but the URI is ignored) and does
  not report speech onset, so `START-OF-INPUT` is sent when recognition starts
  rather than when speech is detected.
- The Tencent engine completes a `RECOGNIZE` on the first definite sentence,
  which models a single utterance per request; long multi-sentence dictation
  would need a different completion policy. It uses the streaming TTS v2 API
  only and reports no confidence score.
- The iFlytek engine accepts only 16 kHz PCM (8 kHz audio is upsampled), also
  completes on the first final segment, and reports no confidence score.
- Recognition is one request per cloud connection; there is no reconnection or
  retry in the middle of an utterance.

## License

MIT — see [LICENSE](LICENSE).