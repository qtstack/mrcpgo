# mrcpgo

`mrcpgo` is a small, dependency-light **MRCPv2 (RFC 6787) media resource server**
written in Go. It acts as a SIP UAS that negotiates an RTP audio stream and one
or more MRCPv2 control channels with a caller such as
[FreeSWITCH](https://freeswitch.org) `mod_unimrcp`, and serves the
`speechrecog` and `speechsynth` resources over a self-contained MRCPv2 text
protocol implementation.

The speech engines are pluggable. A deterministic mock engine ships with the
server so the whole call path can be exercised end to end without an ASR or TTS
backend.

---

## Table of contents

- [Features](#features)
- [Requirements](#requirements)
- [Quick start](#quick-start)
- [Configuration](#configuration)
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
| Engines | `Recognizer` / `Synthesizer` interfaces with a mock implementation |
| Ops | Structured `slog` logging, optional `/healthz` endpoint, YAML configuration |

Out of scope by design: SIP registration, digest authentication, proxying,
TLS/SRTP/DTLS, SCTP, recorder and speaker-verification resources, real ASR/TTS
inference, VAD, jitter buffering and RTCP.

## Requirements

- **Go 1.27.0** or newer. The project uses the standard library `uuid` package
  (RFC 9562) and the `runtime/pprof` `goroutineleak` profile introduced in Go
  1.27.

Direct dependencies, pinned in `go.mod`:

| Module | Version | Used for |
|---|---|---|
| `github.com/emiago/sipgo` | `v1.6.0` | SIP transport, transactions and dialogs |
| `github.com/pion/rtp` | `v1.10.6` | RTP packet marshal/unmarshal |
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
    type: mock
    recognition_delay: 200ms
    frames_per_utterance: 50

http:
  listen: ""
```

Only the SIP listener is fixed; the RTP UDP port and every MRCP TCP port are
allocated per call (`:0`) and advertised in the SDP answer.

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
    └── internal/engine/mock    mock speech engines
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

`testdata/mrcp_*.txt` are wire-format golden files with CRLF line endings;
`.gitattributes` pins their line endings so the round-trip test is stable.

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
- RTP starts with a random SSRC, sequence number and timestamp (RFC 3550).
- Sessions wait for goroutines with a 5 second bound (`internal/syncx`) so a
  misbehaving engine cannot hang shutdown forever.

## Limitations

- UDP SIP only; no TCP/TLS transport, no RFC 3261 authentication, no re-INVITE.
- No RTCP, jitter buffer or packet reordering.
- Speech engines are mocks; the engine interface is the integration point for a
  real ASR/TTS backend.

## License

MIT — see [LICENSE](LICENSE).