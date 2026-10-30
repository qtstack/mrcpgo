# Go MRCPv2 Media Resource Server —— 工程实现任务书（完整修订版）

> 本文档可直接整段粘贴给 Claude Code / Cursor / 同类 Agentic Coding 工具执行。
> 工具读完本文档后应**自主完成全部代码编写、编译、单元测试与自测**，不需要再向用户反复确认选型；凡本文档已给出决策的，直接按决策实现；本文档未覆盖的细节，由实现者按"最小、可测试、符合 RFC"原则自主补齐，并在代码注释中以 `// DECISION:` 标注理由。

---

## 0. 给 Agent 的总指令

你是一名资深 Go 后端 + CTI 协议工程师，熟悉 SIP(RFC3261)、SDP(RFC4566)、MRCPv2(RFC6787)、RTP(RFC3550)、FreeSWITCH `mod_unimrcp`。

请从零实现一个**可编译、可测试、可与 FreeSWITCH `mod_unimrcp` 对接的 MRCPv2 媒体资源服务器（UAS）**。

**本服务角色**：SIP UAS，监听 UDP 5060，接收 FreeSWITCH（UAC）的 INVITE；在 SDP answer 中通告 RTP 端口与 MRCPv2 TCP 控制端口；接收 ACK 后进入 confirmed 状态；在 TCP 控制通道上处理自研 MRCPv2 文本协议；通过 UDP/RTP 收发 PCMU 音频；收到 BYE 后确定性释放全部资源。

**执行纪律**：

1. 不要向用户反复确认选型。本文档已给出的决策直接实现；未覆盖的细节按"最小、可测试、符合 RFC"原则自主补齐，并在代码注释中以 `// DECISION:` 标注理由。
2. 如果第三方库实际 API 与本文档示例有出入，**以 `go doc` / `pkg.go.dev` 最新稳定版签名为准**，并在 `README.md` 的"实现备注"中记录版本与偏差点；但**协议报文语义不得改动**。
3. 先读第 1 节"关键修正"，再开始写代码。第 1 节列出了原始残缺文档中会导致实现失败的技术错误。

---

## 1. 关键修正（必读）

### 1.1 sipgo：不要使用 `NewDialogServerCache` 或 `OnDialog`（实验性/已过时）

当前稳定 sipgo 的 UAS dialog 入口是 **`sipgo.NewDialogServer`**（v0.15.0+），配合 **`DialogServerSession`** 使用。

正确调用模式：

```go
ua, _ := sipgo.NewUA(sipgo.WithUserAgent("mrcpd/0.1"))
client, _ := sipgo.NewClient(ua)                    // 终止 dialog 时可能需要
ds, _ := sipgo.NewDialogServer(client /* , 以 go doc 为准的选项 */)
srv, _ := sipgo.NewServer(ua)

srv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
    dlg, err := ds.ReadInvite(req, tx)              // 必须调用，创建 dialog 上下文
    // 解析 SDP offer → 创建本地资源 → 生成 answer SDP
    dlg.Respond(sip.NewResponseFromRequest(req, 200, "OK", answerSDP))
})

srv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
    ds.ReadAck(req, tx)                             // 必须调用，dialog 进入 confirmed
})

srv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
    ds.ReadBye(req, tx)                             // 必须调用，触发 dialog 终止
    tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil))
})
```

**硬性要求**：

- 必须调用 `ReadInvite` / `ReadAck` / `ReadBye`；只注册 `srv.OnInvite` 而不走 `DialogServer` 会导致 ACK/BYE 无法关联到同一个 dialog。
- 不要使用 `sipgo.NewServerDialog` 或 `srv.OnDialog(...)`——README 明确标注为 "experiment"，且不会走 `DialogServerSession` 的生命周期管理。
- `ReadInvite` 返回的 `DialogServerSession` 是后续 `Respond` 的唯一合法入口；不要直接用 `tx.Respond` 跳过 dialog 层。
- `DialogServerSession.Close()` 应在 BYE、连接断开、进程退出三条路径上都调用，确保 dialog 状态被清理。

### 1.2 MRCPv2 帧定界：**不是**靠 `Content-Length` 从头读，而是靠 start-line 中的 `message-length`

MRCPv2 每条消息的 start-line 第 2 个 token 是 **`message-length`**，表示**从 `MRCP/2.0` 开始到消息体结束的总字节数**（包括 start-line 本身和 CRLF）。TCP 是字节流，没有消息边界。接收循环必须：

1. 读取直到遇到第一个 `CRLF`，解析 start-line；
2. 从 start-line 中取出 `message-length`；
3. 计算还需读取的字节数 = `message-length` - 已消费的 start-line 字节数（包括其 CRLF）；
4. 精确读取该字节数，然后按 CRLF 拆分 headers 与 body；
5. 如果 body 非空，用 `Content-Length` 校验 body 长度是否一致（`Content-Length` 是消息体字节数，不是整条消息长度）。

**不要在 headers 之前做模糊的 `bufio.Scanner` 按行读**——必须实现长度前缀驱动的帧解码器。这是自研编解码器的第一优先级。

### 1.3 Go 1.27 的实际可用特性

Go 1.27.0 已发布，标准库新增 **`uuid`**（支持 v4 与 v7，RFC 9562）和 **`encoding/json/v2`**，运行时新增 **`goroutineleak` profile**（`runtime/pprof`，端点 `/debug/pprof/goroutineleak`）。

**利用方式**：

- **`uuid` 标准库**用于生成 `Channel-Identifier` 的 session-id 部分、RTP SSRC 的辅助标识。不要引入第三方 UUID 库。
- **`goroutineleak` profile** 在测试中强制检查：每个测试用 `pprof.Lookup("goroutineleak")` 做基线对比，确保没有永久阻塞的 goroutine 泄漏。
- `encoding/json/v2` 可用于配置加载（如果决定不用 YAML），但本文档指定 YAML 配置，因此不强制使用。

### 1.4 pion/rtp 的 PCMU 处理

`pion/rtp` 提供 `PayloadTypePCMU = 0`，`codecs.G711Payloader` 存在于 `github.com/pion/rtp/codecs`（v1.1.4+）。但 G.711 是**每 20ms 一个固定长度 payload（8kHz 下 160 字节）**，不需要分片。

**实现决策**：不使用 `G711Payloader`，直接构造 `rtp.Packet{Header: ..., Payload: pcmuBytes}`，将 160 字节 μ-law 帧作为单个 RTP payload 发送。这样更简单，也避免对 pion 子包版本的额外依赖。接收侧同样直接读取 `rtp.Packet.Payload` 交给引擎。

### 1.5 FreeSWITCH `mod_unimrcp` 的 SDP 实际形态

FreeSWITCH 作为 UAC 在 INVITE offer 中通常包含：

```sdp
m=audio 2xxx RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=sendrecv
m=application 9 TCP/MRCPv2 1
a=setup:passive
a=connection:new
a=channel:xxxxxxxx@speechrecog
a=cmid:1
m=application 9 TCP/MRCPv2 1
a=setup:passive
a=connection:new
a=channel:yyyyyyyy@speechsynth
a=cmid:1
```

关键点：

- `m=application` 的端口在 offer 中通常是 `9`（discard），**真正的端口以 answer 为准**；服务器必须在 answer 中填入自己实际监听的 MRCP TCP 端口。
- `a=setup:passive` 表示**服务器是被动方**，FreeSWITCH 会主动向服务器 answer 中的端口发起 TCP 连接。
- `a=channel:<id>@speechrecog` / `@speechsynth` 中的 `<id>` 是客户端提议的 channel-id；服务器应在 answer 中**原样返回或重新分配**并保持一致。MRCPv2 要求所有请求、响应、事件都携带 `Channel-Identifier` 头，其值必须与 SDP `a=channel` 中的值匹配。
- `a=cmid` 将控制通道关联到 audio 的 `a=mid`；answer 中必须保留这种关联。
- 如果 offer 中只出现 `m=audio` 而没有 `m=application`，说明客户端不打算使用 MRCPv2，本服务应返回 `488 Not Acceptable Here` 并结束。

---

## 2. 项目范围（锁定）

### 2.1 In Scope

| 层 | 本期必须完成 |
|---|---|
| SIP | UAS：INVITE → 100 Trying → 200 OK；ACK；BYE → 200 OK；CANCEL → 487；OPTIONS → 200。不实现 REGISTER、鉴权、PRACK、代理。 |
| SDP | 自研轻量解析器：提取 `m=audio`（端口、payload type）与 `m=application TCP/MRCPv2`（`a=channel`、`a=setup`、`a=cmid`）；生成 answer，填入本地 RTP UDP 端口与 MRCP TCP 端口。 |
| MRCPv2 | 自研文本编解码器。通用方法：`SET-PARAMS`、`GET-PARAMS`、`STOP`。Recognizer：`DEFINE-GRAMMAR`、`RECOGNIZE`、`START-INPUT-TIMERS`；事件 `RECOGNITION-COMPLETE`、`START-OF-INPUT`。Synthesizer：`SPEAK`、`STOP`；事件 `SPEAK-COMPLETE`。 |
| RTP | UDP + pion/rtp。PCMU PT=0。接收 FS→Server 音频并交给 Recognizer 引擎；TTS 时 Server→FS 按 20ms 帧发送静音/固定 PCMU 流。维护 seq/ts/SSRC，记录丢包与乱序统计（仅日志，不做 jitter buffer）。 |
| 会话 | 以 SIP Dialog 为聚合根：一个 Session 包含 SIP Dialog、一个或多个 MRCP Channel（TCP）、RTP 收发器、引擎实例。BYE、TCP 断开、进程退出三条路径确定性释放。 |
| 引擎 | `Recognizer` / `Synthesizer` 接口 + `mock` 实现。Mock Recognizer 在收到 RECOGNIZE 后定时产出固定 NLSML；Mock Synthesizer 把文本忽略并发送固定 PCMU 帧，结束时发 `SPEAK-COMPLETE`。 |
| 可观测 | `slog` 结构化日志，携带 `call_id`、`channel_id`、`request_id`、`sip_method`、`mrcp_method`。`/healthz` 可选（`net/http`）。 |

### 2.2 Out of Scope（明确不做）

- SIP 注册、Digest 鉴权、代理/路由、PRACK/100rel。
- TLS/SRTP/DTLS、SCTP、`TCP/TLS/MRCPv2`。
- Speaker Verification/Identification、Recorder 完整语义（可返回 `501 Not Implemented`）。
- 真实 ASR/TTS 推理、VAD、端点检测、jitter buffer、RTCP。
- HTTP 管理后台、集群、持久化。

---

## 3. 技术栈与版本锁定

`go.mod` 首行：

```go
module github.com/your-org/mrcpd

go 1.27.0
```

直接依赖**仅限**：

```go
require (
    github.com/emiago/sipgo   // 取当时最新稳定版；以 go doc 确认 NewDialogServer 签名
    github.com/pion/rtp       // 仅使用 rtp.Packet / rtp.Header；不使用 codecs 子包
    gopkg.in/yaml.v3          // 配置解析
)
```

**不要引入** Gin/Echo、任何 MRCP 现成库、任何 SDP 大库、第三方 UUID 库（Go 1.27 自带 `uuid`）。提交时锁定 `go.sum`。

---

## 4. 包结构与依赖方向

依赖方向**单向向下**，禁止循环导入：

```
cmd/mrcpd/main.go
    ↓
internal/server          （SIP 监听、OnInvite/OnAck/OnBye/OnCancel/OnOptions 注册，DialogServer 管理）
    ↓
internal/session         （Session 聚合根、生命周期、资源释放顺序）
    ├── internal/sdp    （Offer 解析、Answer 生成）
    ├── internal/mrcp   （MRCPv2 帧编解码、Channel 状态机、请求分发）
    ├── internal/media  （RTP UDP 收发、PCMU 帧、seq/ts/SSRC 维护）
    └── internal/engine  （Recognizer / Synthesizer 接口 + mock 实现）
            ↑
internal/model          （纯枚举、纯数据结构、引擎接口；只依赖标准库）
```

**`internal/model` 的硬约束**：

- 允许导入标准库；若接口签名确实需要 `*rtp.Packet`，允许导入 `github.com/pion/rtp`。
- **不得**导入 `internal/mrcp`、`internal/sdp`、`internal/session`、`internal/media`。
- 引擎接口放在 `model/engine.go`，避免 `session` 与 `engine` 循环。

---

## 5. 协议规格（实现时必须逐条满足）

### 5.1 SIP / SDP 行为

**INVITE 处理顺序**：

1. 立即回 `100 Trying`（通过 `dlg.Respond`）。
2. 解析 `Content-Type: application/sdp` 的 offer。
3. 如果缺少 `m=audio` → 回 `488 Not Acceptable Here` 并关闭 dialog。
4. 为每个 `m=audio` 分配一个本地 UDP 端口（`net.ListenUDP("udp4", :0)`）。
5. 为每个 `m=application TCP/MRCPv2` 分配一个本地 TCP 监听端口（`net.Listen("tcp4", :0)`），启动 accept 循环；每个控制通道对应一条 TCP 连接。
6. 生成 answer SDP，包含：
   - `m=audio <本地端口> RTP/AVP 0`，`a=rtpmap:0 PCMU/8000`，`a=sendrecv`。
   - 每个控制通道一条 `m=application <本地 TCP 端口> TCP/MRCPv2 1`，`a=setup:passive`，`a=connection:new`，`a=channel:<channel-id>@<resource>`，`a=cmid:<audio-mid>`。
7. `dlg.Respond(200 OK, answerSDP)`。
8. 创建 `session.Session` 并注册到并发安全的 `map[callID]*Session`（`sync.RWMutex`）。
9. 等待 ACK：`OnAck` 中调用 `ds.ReadAck`；只有 ACK 后，才允许 MRCP 通道进入 `ACTIVE` 并处理请求。

**BYE**：

1. `OnBye` 中调用 `ds.ReadBye`。
2. 回 `200 OK`。
3. 触发 `session.Close(reason)`；`Close` 必须是幂等的，且阻塞到所有资源释放完毕或超时（建议 5s）。

**CANCEL**：

- 如果 dialog 尚未 confirmed，回 `487 Request Terminated`，并清理预分配资源。

**OPTIONS**：

- 回 `200 OK`，`Allow: INVITE, ACK, BYE, CANCEL, OPTIONS`。

### 5.2 MRCPv2 编解码器

**消息类型**：Request（客户端→服务器）、Response（服务器→客户端）、Event（服务器→客户端，异步）。

**start-line 格式**：

```
request:   MRCP/2.0 <message-length> <method-name> <request-id>
response:  MRCP/2.0 <message-length> <request-id> <status-code> <request-state>
event:     MRCP/2.0 <message-length> event <event-name> <request-id>
```

`message-length` 必须是从 `MRCP/2.0` 第一个字节到消息体最后一个字节的总长度，包含所有 CRLF。接收方先读到第一个 `CRLF`，从中解析出 `message-length`，再精确读取剩余字节。

**Headers**：

- 每条消息必须含 `Channel-Identifier: <id>@<resource>`。
- 可选 `Content-Type`、`Content-Length`。`Content-Length` 仅约束 body 字节数。
- Header 行以 CRLF 分隔，空行后是 body。

**请求处理状态机（Channel 级）**：

```
IDLE ──RECOGNIZE/SPEAK──▶ IN-PROGRESS ──引擎完成──▶ COMPLETE
  ▲                            │
  └──────── STOP ──────────────┘
```

- `SET-PARAMS` / `GET-PARAMS`：同步，不改变 request-state，响应为 `COMPLETE`。
- `DEFINE-GRAMMAR`：同步，响应 `COMPLETE`；MVP 仅接受 `Content-Type: application/srgs+xml` 和 `text/uri-list`，其他返回 `403 Unsupported Header` 或 `501 Not Implemented`。
- `RECOGNIZE`：异步。先回 `IN-PROGRESS`（或 `PENDING` 如果资源正忙），然后引擎在后台运行；完成时发送 `RECOGNITION-COMPLETE` 事件，`request-state: COMPLETE`。
- `START-INPUT-TIMERS`：同步 `COMPLETE`，MVP 可空实现。
- `SPEAK`：异步。先回 `IN-PROGRESS`；发送完 PCMU 流后发送 `SPEAK-COMPLETE` 事件，`COMPLETE`。
- `STOP`：终止当前 IN-PROGRESS 的请求，响应 `COMPLETE`，不再发送后续完成事件。

**最小状态码映射**：

| 场景 | 状态码 |
|---|---|
| 成功 | `200 Success` |
| 方法不支持 | `501 Not Implemented` |
| 当前状态不允许 | `402 Method Not Valid in This State` |
| 缺少必需头 | `400 Bad Request` |
| Channel-Identifier 未知 | `406 Resource Not Allocated` |
| 资源忙 | `407 Resource In Use` |
| 内部错误 | `500 Server Internal Error` |

**NLSML 结果**：`RECOGNITION-COMPLETE` 的 body 使用 `Content-Type: application/nlsml+xml`。MVP mock 结果固定为：

```xml
<?xml version="1.0"?>
<result>
  <interpretation confidence="99">
    <input mode="speech">mock input</input>
    <instance>mock-instance</instance>
  </interpretation>
</result>
```

### 5.3 RTP

- 编码：PCMU，payload type 0，8kHz，20ms 帧 = 160 字节。
- 发送侧：`rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc}, Payload: pcmuFrame}`；`seq` 每包 +1，`ts` 每包 +160。SSRC 用 `crypto/rand` 生成 `uint32`，不要用 0。
- 接收侧：`rtp.Packet.Unmarshal(buf)`；只取 `Payload`；记录 `SequenceNumber` 跳变次数到统计，仅日志，不重排。
- 发送节拍：`time.Ticker(20 * time.Millisecond)`；每个 tick 从 Synth 引擎取一帧；引擎返回 `io.EOF` 时发送 `SPEAK-COMPLETE` 并停止 ticker。
- 当 `session.Close` 时，ticker 必须 `Stop()`，UDP 连接必须 `Close()`。

### 5.4 资源释放顺序（硬性）

`session.Close(reason)` 必须按以下顺序执行，且幂等：

1. 设置状态为 `Releasing`，拒绝新 MRCP 请求。
2. 停止 RTP 发送 ticker；关闭 RTP UDP socket。
3. 向所有 MRCP Channel 发送 `STOP`（如果存在 IN-PROGRESS 请求），然后关闭 TCP 连接。
4. 关闭 MRCP TCP 监听器（如果有）。
5. 等待引擎 goroutine 退出（`WaitGroup.Wait` 或 `context` 取消）。
6. 从全局 session map 中删除。
7. 设置状态为 `Closed`。
8. 调用 `DialogServerSession.Close()`（如果 SIP dialog 尚未终止）。

**BYE 路径**由 `OnBye` 触发；**TCP 断开路径**由 MRCP Channel 的 read loop 检测到 `io.EOF` 后触发；**进程退出路径**由 `main` 的 `signal.NotifyContext` 触发全局 `Shutdown`，遍历 map 调用 `Close`。

---

## 6. 关键接口与实现规格

### 6.1 `internal/model/engine.go`

```go
package model

import (
    "context"
    "io"
)

type RecognizerResult struct {
    NLSML []byte
    Err   error
}

type Recognizer interface {
    // Start 启动识别；返回的结果通过 channel 异步送达。
    // 必须响应 ctx 取消，并关闭结果 channel。
    Start(ctx context.Context, grammarURI string) (<-chan RecognizerResult, error)
    Stop() error
}

type Synthesizer interface {
    // Synthesize 返回 PCMU 帧流（每帧 160 字节）。
    // 返回 io.EOF 表示合成结束。
    Synthesize(ctx context.Context, text string) (io.ReadCloser, error)
    Stop() error
}

// 可选：RTP 统计
type RTPStats struct {
    Received, Lost, OutOfOrder uint64
}
```

`model` 包**只**导入标准库。引擎接口本身不需要 `rtp.Packet`，所以 `model` 可以做到零第三方依赖。

### 6.2 `internal/mrcp` 编解码器与 Channel

**帧解码器核心逻辑**：

```go
func ReadMessage(r *bufio.Reader) (Message, error) {
    startLine, err := r.ReadString('\n')
    if err != nil { return Message{}, err }
    // 解析 "MRCP/2.0 <message-length> ..."
    var msgLen int
    if _, err := fmt.Sscanf(startLine, "MRCP/2.0 %d", &msgLen); err != nil {
        return Message{}, fmt.Errorf("mrcp: parse start-line: %w", err)
    }
    consumed := len(startLine)
    remain := msgLen - consumed
    if remain < 0 {
        return Message{}, fmt.Errorf("mrcp: invalid message-length %d", msgLen)
    }
    rest := make([]byte, remain)
    if _, err := io.ReadFull(r, rest); err != nil {
        return Message{}, err
    }
    // 拆分 headers / body，校验 Content-Length
    return parseRest(startLine, rest)
}
```

**Channel** 结构：

```go
type Channel struct {
    ID       string          // <session-id>@<resource>，与 SDP a=channel 一致
    Resource string          // "speechrecog" | "speechsynth"
    Conn     net.Conn
    State    atomic.Value    // IDLE / IN_PROGRESS
    // 请求分发、引擎引用、ctx/cancel、wg、写锁
}
```

每个 Channel 一个 read loop goroutine：`ReadMessage` → 校验 `Channel-Identifier` → 根据 method 分发。写响应必须串行化（`sync.Mutex` 包住 `Conn.Write`），因为事件可能来自引擎 goroutine。

### 6.3 `internal/sdp`

只实现本场景所需的最小解析：

- 按行扫描，提取 `m=audio` 和 `m=application TCP/MRCPv2`。
- 对 `m=application`，提取 `a=channel`、`a=setup`、`a=cmid`。
- 对 `m=audio`，提取端口、payload type 列表、`a=mid`、`a=rtpmap`。
- Answer 生成：保留 offer 中的 `a=channel` 原值（或重新分配并记录映射），填入本地端口，`a=setup:passive`，`a=connection:new`，`a=cmid`。
- **不要**尝试实现完整 RFC 4566 解析器；遇到不认识的 `a=` 行直接跳过。

### 6.4 `internal/media/rtp.go`

- `Transceiver` 持有 UDPConn、远端地址（从 SDP 的 `c=` 和 `m=audio` 端口得到）、SSRC、seq、ts。
- 提供 `ReceiveLoop(ctx, func(*rtp.Packet))` 和 `SendFrame(frame []byte)`。
- `SendFrame` 必须线程安全（ticker goroutine 和 STOP 路径可能并发）。
- 丢包统计：记录期望 seq 与实际 seq 的差值，仅 `slog.Debug` 输出。

### 6.5 `internal/session`

**Session 结构**（简化示意）：

```go
type Session struct {
    CallID     string
    LocalSDP   []byte
    RemoteSDP  []byte
    Dialog     *sipgo.DialogServerSession
    Channels   map[string]*mrcp.Channel   // key: Channel-Identifier
    RTP        *media.Transceiver
    Recognizer model.Recognizer
    Synth      model.Synthesizer

    state     model.SessionState
    cancel    context.CancelFunc
    wg        sync.WaitGroup
    closeOnce sync.Once
}
```

- `NewSession` 在 INVITE 200 OK 之前创建，但资源（TCP accept、RTP）应在 200 OK 之前就绪，因为 FreeSWITCH 收到 200 OK 后可能立即发起 TCP 连接和 RTP。
- `Close` 用 `sync.Once` 保证幂等；必须阻塞到 `wg` 归零或超时。
- Session map 的读写必须通过 `sync.RWMutex` 保护的 `Manager` 类型，禁止裸 map。

### 6.6 `internal/server`

- `Run(ctx)` 创建 `sipgo.NewUA`、`sipgo.NewClient`、`sipgo.NewDialogServer`、`sipgo.NewServer`，注册 `OnInvite`、`OnAck`、`OnBye`、`OnCancel`、`OnOptions`。
- 在 `OnInvite` 中完成 SDP 解析、资源预分配、session 创建、200 OK。**不要**在 `OnInvite` 中阻塞等待 ACK 或 MRCP 连接。
- 监听 `ctx.Done()`，触发 `Manager.Shutdown`。
- SIP 监听：`srv.ListenAndServe(ctx, "udp", cfg.SIP.Listen)`。MVP 只启 UDP。

---

## 7. 测试与验收

### 7.1 单元测试（`go test ./...` 全绿）

| 包 | 测试内容 |
|---|---|
| `internal/mrcp` | 编解码 round-trip：Request/Response/Event 各至少 3 个 fixture，覆盖有 body/无 body、多 header、长 body 跨 buffer；`message-length` 错误时返回错误；`Channel-Identifier` 缺失时拒绝。 |
| `internal/sdp` | 解析 FreeSWITCH 真实 offer fixture（含两条 `m=application`），断言提取出的 channel-id、resource、audio 端口、`a=cmid`；生成 answer 后用同一解析器 round-trip。 |
| `internal/media` | `SendFrame` 后解包得到的 seq/ts/payload 正确；20ms 递增；SSRC 非零。 |
| `internal/session` | 状态转换：`Created → Active → Releasing → Closed`；`Close` 调用两次不 panic；mock 引擎的 `Start`/`Stop` 被调用。 |
| `internal/engine/mock` | Recognizer 在 ctx 取消时关闭结果 channel；Synth 返回 50 帧后 `io.EOF`。 |

### 7.2 集成测试（`go test -tags=integration`）

用 sipgo 自身写一个最小 UAC 测试，**不需要真实 FreeSWITCH**：

1. 启动 `mrcpd` 的 `server.Run`（监听 `127.0.0.1:0` 或指定随机端口）。
2. 用 `sipgo.NewUA` + `sipgo.NewClient` 发送 INVITE，SDP offer 包含 `m=audio` 和 `m=application TCP/MRCPv2`。
3. 读取 200 OK，解析 answer SDP，拿到 RTP 端口和 MRCP TCP 端口。
4. 向 MRCP TCP 端口发起连接，发送 `RECOGNIZE`，断言收到 `IN-PROGRESS` 响应，然后收到 `RECOGNITION-COMPLETE` 事件。
5. 发送 ACK、BYE，断言 200 OK，并断言服务端 session map 为空、TCP/UDP 端口已释放。
6. 用 `runtime/pprof.Lookup("goroutineleak")` 对比测试前后，确认无泄漏。

### 7.3 构建与静态检查

```bash
go build ./...
go vet ./...
gofmt -l .        # 必须无输出
go test ./...
```

**禁止**在最终提交中出现 `go vet` 告警、goroutine 泄漏、裸 map 并发写、错误被 `_` 静默吞掉。

---

## 8. 交付物清单

```
mrcpd/
├── go.mod
├── go.sum
├── README.md                     # 构建、运行、配置、与 FS mod_unimrcp 联调步骤、实现备注
├── config.example.yaml
├── cmd/mrcpd/main.go
├── internal/
│   ├── model/                    # 纯枚举 + 引擎接口
│   ├── sdp/
│   ├── mrcp/
│   ├── media/
│   ├── engine/mock/
│   ├── session/
│   └── server/
└── testdata/
    ├── freeswitch_offer.sdp
    └── mrcp_*.txt
```

`README.md` 必须包含：

- `go 1.27.0` 的要求。
- 实际使用的 sipgo / pion/rtp 版本，以及与本文档示例的偏差点。
- FreeSWITCH `mod_unimrcp` 侧配置示例（`mrcp profile`、`synthparams`、`recogparams`、`codecs=PCMU`）。
- 联调验证命令（`fs_cli` 的 `speak` / `detect_speech` 示例）。

---

## 9. 执行顺序建议（给 Claude Code）

1. 先写 `internal/model` 与 `internal/mrcp` 编解码器 + 单元测试，确保 `ReadMessage` 的长度驱动逻辑正确。
2. 写 `internal/sdp` + fixture 测试。
3. 写 `internal/media` + RTP 单元测试。
4. 写 `internal/engine/mock`。
5. 写 `internal/session` 状态机与 `Close` 顺序。
6. 最后写 `internal/server` 与 `cmd/mrcpd/main.go`，接 sipgo。
7. 跑集成测试，用 `go vet` / `gofmt` / `go test` 收尾。
8. 在 `README.md` 中记录所有 `// DECISION:` 注释的汇总。

这个只是参考，如果你有更好的方案请用更好的方案执行，这个项目是开源项目 一定要符合开源项目的规范 逻辑要清晰 代码结构要清晰 代码要规范