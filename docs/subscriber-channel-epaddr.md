## User

你给我讲一下这个项目的 WebRTC 子系统的 "subscriber" 和 "channel" 这两个概念

## Assistant

<thinking>
I can respond now. Should source refs clickable with line numbers? Project paths plus :line typical. Respond Chinese concise but sufficiently thorough. No need validation or session note because explanation only, no code. I should perhaps mention read sources at end.

Structure:
"一句话：channel 是信令层的成员命名空间/发现范围；subscriber 是某客户端在某个 channel 中的一次注册身份。二者属于 SS 信令层，不是 WebRTC 标准里的发布/订阅或 RTCDataChannel。"
Analogy: channel room/VLAN, subscriber seat/room-local number. Full address (channelId, subscriberId).
Detailed:
1 subscriber:

- registered with register {subscriberId, channelId, username}; empty auto assign.
- member state id username addr lastActive.
- not userId: user identity from JWT; bound to (user id, user session id, channel id). A subscriber closer "登录会话里的一个客户端 incarnation" not account. Browser reload new id; same page WS reconnect reuse.
- not PeerConnection: peer discovery then one PC per pair.
- expiration default 30s, keepalive.
  2 channel:
- globally unique id, metadata name + member maps.
- member discovery and signalling relay scope; c2c single channel; both sender and receiver resolved there.
- current only main.
- no media mixing/routing/broadcast.
  Flow example A 1001 B1002 main:
  register -> list -> create sessions keyed composite -> SDP/ICE through WS/SS -> direct P2P carrying dcmsg/dcbin/media. channel not involved after actual P2P except membership and signalling.
  Potential nuance actual SS handles calls over WS, but okay.
  "为什么需要两个 ID" composite uniqueness allows same subscriber id across channels and prevents cross-channel misrouting.
  "术语防混淆" table Signal channel vs RTCDataChannel vs Go channel perhaps user likely confused. Could include small table with 3 rows. `PeerSession.channels` is RTCDataChannel, not signalling channel. "subscriber" not subscriber in pub/sub.
  Reference files exact:
- `web/site/src/api/ss/types.ts:15` etc
- `pkg/models/ss/ss.go:536`, `:766`, `:830`
- `web/site/src/api/ss/peersessions.tsx:3`, `:776`
- docs.
  No mermaid needed perhaps simple text flow.
  Should avoid claim subscriber id globally can repeat across
  </thinking>

一句话概括：

> **Channel 是信令系统里的“成员空间”；Subscriber 是某个客户端在这个空间里的注册身份。**  
> 它们首先属于这个项目的 **Signalling Server（SS，信令服务器）**，不是 WebRTC 本身的标准概念。

可以把它类比成：

- **Channel**：房间或 VLAN
- **Subscriber**：房间里的一个成员及其房间内部编号
- 完整地址不是单独的 `subscriberId`，而是 **`(channelId, subscriberId)`**

## 1. Subscriber 是什么？

`Subscriber` 表示一个客户端在某个 channel 中的注册项。客户端通过下面这个信令事件加入 channel：

```ts
register: {
  subscriberId: "",
  channelId: WELLKNOWN_CH_ID_MAIN,
  username: "",
}
```

如果 `subscriberId` 为空，服务器会从 `1000–1999` 中分配一个；注册结果返回最终的 `(channelId, subscriberId)`。相关定义在 `web/site/src/api/ss/types.ts:44`，注册逻辑在 `pkg/models/ss/ss.go:536`。

服务器为每个 subscriber 保存的信息大致是：

```go
type subscriberState struct {
    id         SubscriberId
    username   string
    addr       EPAddr
    lastActive time.Time
}
```

见 `pkg/models/ss/ss.go:830`。

### 它不等于登录用户

这里有三层身份需要区分：

| 概念            | 含义                                        |
| --------------- | ------------------------------------------- |
| `UserId`        | 登录用户，全局身份                          |
| `UserSessionId` | 用户的一次登录会话                          |
| `SubscriberId`  | 该会话中的客户端在某个 channel 里的信令身份 |

Subscriber 注册会绑定到：

```text
(userId, userSessionId, channelId)
```

因此，**subscriber 更接近“一个客户端实例在信令系统里的化身”**，而不是用户账号。

例如：

- 页面重新加载：通常创建一个全新的 subscriber。
- 同一个页面只是 WebSocket 断线重连：沿用原 subscriber ID，保留原注册和现有 WebRTC peer sessions。
- Bot 也可以是一个 subscriber；它不必是真人用户。

### 它也不等于 RTCPeerConnection

Subscriber 是信令层身份；`RTCPeerConnection` 是两个 subscriber 之间建立的 WebRTC 连接。

同一个 subscriber 可以同时和多个其他 subscriber 通信，因此它可以对应多个 peer connection：

```text
subscriber A
├── 与 B 的 RTCPeerConnection
├── 与 C 的 RTCPeerConnection
└── 与 music bot 的 RTCPeerConnection
```

另外，subscriber 注册会过期。默认 30 秒没有活动就会失效，客户端通过 `channelKeepAlive` 等活动续期；当前协议没有显式的 `unregister/leave`，见 `docs/signalling-server.md:593`。

## 2. Channel 是什么？

Channel 是信令服务器维护的一个**成员集合和寻址命名空间**。服务器内部结构大致是：

```go
type channelState struct {
    name          string
    subscribers   map[SubscriberId]*subscriberState
    idsByUsername map[string]SubscriberId
    nextAutoId    int
}
```

见 `pkg/models/ss/ss.go:850`。

它主要负责两件事：

### 成员发现

客户端可以查询：

- 有哪些 channel：`listChannels`
- channel 的资料：`channelProfileQuery`
- channel 里有哪些 subscriber：`listChannelMembers`
- 某个 subscriber 的资料：`userProfileQuery`

前端发现同 channel 的其他成员后，会自动为每个成员建立 peer session，见 `web/site/src/api/ss/peersessions.tsx:776`。

### 限定信令寻址范围

两个客户端交换 SDP、ICE candidate 等信令时，消息地址包含：

```ts
{
  channelId,
  fromSubscriber,
  toSubscriber,
}
```

信令服务器会在**同一个 channel** 中解析发送方和接收方：

```text
(channelId, toSubscriber)
           ↓
接收方注册时使用的 EPAddr
           ↓
对应的 WebSocket 连接
```

所以 subscriber ID 只需要在 channel 内唯一。不同 channel 中即使出现相同的 subscriber ID，也代表不同成员。代码中的类比就是“subscriber ID 像 IP 地址，channel 像它所在的 VLAN”，见 `docs/signalling-server.md:160` 和 `pkg/models/ss/ss.go:766`。

目前服务器是原型实现，**只有固定的 `main` channel**，状态也只保存在内存里，见 `pkg/models/ss/ss.go:470`。

## 3. 它们在一次 WebRTC 建连中怎么配合？

假设 A 和 B 都进入 `main`：

```text
1. A 注册为 (main, 1000)
2. B 注册为 (main, 1001)

3. A 查询 main 的成员，发现 B
4. A 为 B 创建 RTCPeerConnection

5. A/B 通过 SS 交换 SDP offer/answer 和 ICE candidates
   消息使用 (main, fromSubscriber, toSubscriber) 寻址

6. WebRTC 建连成功
7. 聊天、文件和音视频走 A ↔ B 的 P2P 连接
```

关键点是：

> **Channel 用来发现成员和转发信令；Subscriber 用来标识信令端点；RTCPeerConnection 才承载真正的数据与媒体。**

这是一个 P2P mesh 模型，不是“加入 channel 后服务器把媒体广播给所有 subscriber”的 SFU/发布订阅模型。

## 4. 特别注意：这里的 channel 不是 RTCDataChannel

项目里 “channel” 有几种不同含义：

| 名称                             | 含义                                                    |
| -------------------------------- | ------------------------------------------------------- |
| `ChannelId` / signalling channel | 上面说的信令成员空间，例如 `main`                       |
| `RTCDataChannel`                 | 两个 peer 之间的 WebRTC 数据通道，例如 `dcmsg`、`dcbin` |
| Go `chan`                        | Go 的并发通信原语                                       |

尤其 `PeerSession` 里的：

```ts
channels: Map<string, RTCDataChannel>;
```

指的是 **RTCDataChannel 集合**，和 `ChannelId` 所表示的信令 channel 完全不是一回事。每对 subscriber 之间只有一个 `RTCPeerConnection`，上面可以再复用多个 RTCDataChannel 和媒体轨道，见 `web/site/src/api/ss/peersessions.tsx:354`。

## User

那 EPAddr 又是什么？

## Assistant

<thinking>
I need respond Chinese based code. Already context. Should explain EPAddr packed transport routable address vs subscriber logical channel local. Should verify nuances perhaps read websocket handler to be accurate about addressing mapping and semantics. I have docs exact. Should have no tools? I can respond from verified. But perhaps should inspect ws implementation to explain endpoints and service id. Already read docs lines 555 etc. Should enough. EPAddr contains optional userId userSessionId serviceId. It is packed address of envelope from/to, not IP/port, not subscriber. `from` server overwritten JWT; subscriber registration learns `ev.From` into subscriberState.addr. Provider maps (channel, sub)->EPAddr. WS handler CAM maps EPAddr to websockets by sniffing from. Address may match multiple connections (tabs or reconnect old). User/session pair as unit? EPAddr optional fields; a client endpoint uses both. Service address uses serviceId. Routing to every connection address learned on. Distinguish From C2C payload remains subscriber ids while outer from EPAddr auth identity. Could show example:
```
from: {userId:"u42",userSessionId:"s7"}
to:{serviceId:WELLKNOWN...}
```
registration. SS stores sub 1000 -> EPAddr. A sends c2c to SS with payload toSubscriber 1001; SS rewrites outer ev.To = B EPAddr. WS handler then routes. No sensitive values actual example clearly hypothetical.

"EP" endpoint packed address likely "endpoint address". Does code define full name? Type comment "A packed address struct type". EPAddr likely Endpoint Address. Can say "可以理解为 endpoint/packed address" not assert acronym expansion? Could say "`EPAddr` 是消息信封上的端点地址（packed address）". Good.

Why two addressing systems:

- c2s/s2c and WS transport know authenticated user/session/service, no channels.
- c2c application uses channel+subscriber, SS translates.
- avoids exposing user identity? Does not need claim security.
- Decoupling transport from subscriber: provider only understands subscriber; WS handler never learns sub IDs. Actually docs says provider only component understands subscriber addressing; WS handler never learns. Good.
- `EPAddr` not physical network address, no IP port, and not WebRTC ICE address.
- Registration can outlive connection, EPAddr remains logical identity address; handler learns connections dynamically.
- optional union semantics? Struct has three optional fields not formally oneof? In practice either service or user pair. Should phrase "通常表示两类端点" rather than strict validation union. Server identity requires both user fields for auth. Service uses serviceId. `from` for browser left empty; server populates. `to` when client to SS uses service id; after relay target user/session.
- subscriber has `addr EPAddr`, so relation diagram:

```
(channelId, subscriberId) --SS registration table--> EPAddr --WS handler CAM--> 0..n WS conns
```

This crisp addresses follow-up.

Should have no preamble for response direct. No session note. Cite paths.
Could mention example outer and inner address:

```
SignallingEvent {
 from: EPAddr...
 to: EPAddr...
 c2CEv: { channelId, fromSubscriber, toSubscriber }
}
```

Two layers often confusing. Explain c2c sender outer to SS, c2c inner to target. SS rewrites outer to. `from` untouched after authentication, c2c fields unchanged. `fromSubscriber` potential forged? WS handler override? docs tests forged from overridden perhaps websocket_ss sets C2C FromSubscriber? Should inspect? Wait docs says from and payload pass unchanged, but test `TestForgedFromOverridden` perhaps refers outer From not c2c FromSubscriber. Likely yes. Don't dive.

One nuance listChannels registration? irrelevant.

I can provide ~700 Chinese chars? Good.
</thinking>

`EPAddr` 是 **`SignallingEvent` 信封上的端点地址**，用于让信令服务器和 WebSocket 传输层把一条消息送到正确的接收端。

它和你刚问的 `SubscriberId` 是两层不同的地址：

> **`(channelId, subscriberId)` 是信令业务层的地址；`EPAddr` 是传输路由层使用的地址。**

## EPAddr 长什么样？

定义在 `web/site/src/api/ss/types.ts:32`：

```ts
interface EPAddr {
  userId?: UserId;
  userSessionId?: UserSessionId;
  serviceId?: ServiceId;
}
```

它通常表示两类端点。

### 1. 一个用户会话

```ts
{
  userId: "user-123",
  userSessionId: "session-456",
}
```

也就是“某个用户的某次登录会话”，而不是单纯的“这个用户”。

### 2. 一个服务

例如信令服务器自己：

```ts
{
  serviceId: WELLKNOWN_SVC_ID_SS,
}
```

所以浏览器向 SS 注册时，消息大致是：

```ts
{
  from: {},                              // 由服务器根据 JWT 填写
  to: { serviceId: WELLKNOWN_SVC_ID_SS },
  c2SEv: {
    register: {
      subscriberId: "",
      channelId: WELLKNOWN_CH_ID_MAIN,
      username: "",
    },
  },
}
```

服务器不会信任浏览器提交的 `from`，而是根据当前连接的登录 session 覆盖它，防止客户端冒充其他用户，见 `docs/signalling-server.md:636`。

## 它和 Subscriber 是怎么关联的？

客户端注册 subscriber 时，SS 会同时记住：

```text
(channelId, subscriberId) → 注册消息的 from EPAddr
```

例如：

```text
(main, "1000") → { userId: "alice", userSessionId: "session-A" }
(main, "1001") → { userId: "bob",   userSessionId: "session-B" }
```

因此，subscriber 的注册信息里才会有这个字段：

```go
type subscriberState struct {
    id         SubscriberId
    username   string
    addr       EPAddr
    lastActive time.Time
}
```

见 `pkg/models/ss/ss.go:830`。

## 为什么一条消息里会有两套地址？

客户端之间发送 WebRTC 信令时，结构同时包含外层 `EPAddr` 和内层 subscriber 地址：

```ts
{
  // 外层：当前消息先交给谁处理
  from: { userId: "...", userSessionId: "..." },
  to: { serviceId: WELLKNOWN_SVC_ID_SS },

  // 内层：最终要和哪个 peer 通信
  c2CEv: {
    channelId: "main-channel-id",
    fromSubscriber: "1000",
    toSubscriber: "1001",
    sessionDesc: { /* SDP */ },
  },
}
```

处理过程是：

```text
发送方
  │
  │ 外层 to = SS
  ▼
Signalling Server
  │ 根据 c2CEv 中的 (channelId, toSubscriber)
  │ 查出接收方的 EPAddr
  │
  │ 把外层 to 改写成接收方的 EPAddr
  ▼
WebSocketSSHandler
  │ 根据 EPAddr 找到对应 WebSocket 连接
  ▼
接收方
```

对应实现是 `pkg/models/ss/ss.go:766`。

这样分层的好处是：

- **SS provider** 理解 channel 和 subscriber，负责解析业务地址。
- **WebSocket handler** 不需要理解 subscriber，只根据 `EPAddr` 路由。
- WebRTC negotiator 只关心自己与对方的 `(channelId, subscriberId)`。

## EPAddr 也不是 IP 地址或 WebSocket 连接

它不保存 IP、端口或 socket，而是保存逻辑身份。WebSocket handler 另外维护：

```text
EPAddr → 当前学到的一个或多个 WebSocket 连接
```

因此：

- 同一用户会话打开两个 tab，可能对应多个连接。
- WebSocket 重连后，新连接可以重新关联到同一个 `EPAddr`。
- Subscriber 注册可以短暂地比某次 WebSocket 连接活得更久。

所以完整关系可以概括为：

```text
(channelId, subscriberId)
          │
          │ SS 注册表解析
          ▼
        EPAddr
          │
          │ WebSocket handler 动态路由
          ▼
   当前的一个或多个 WebSocket 连接
```

最简短地说：**Subscriber 回答“在这个 channel 里你是谁”，EPAddr 回答“这条信令消息应该路由给哪个用户会话或服务”。**

## User

描述 c2CEV 和 c2SEV ，另外对于 c2CEV，如果一个 client 不知道另一个 client 的 EPAddr 它怎么把信令消息（通过 WebSocket）送到对方（不管出于何种理由）？

## Assistant

<thinking>
I need respond Chinese describe c2CEv and c2SEv and specifically if client doesn't know EPAddr how send via WS regardless reasons. Already know answer: c2c uses channelId/fromSubscriber/toSubscriber, outer to SS service EPAddr; provider resolves target registration addr and rewrites outer To. WebSocket handler routes based on EPAddr CAM. Sender never needs target EPAddr. Should explain c2SEv client-to-SS requests register, keepalive, profile queries, channel list, ping. c2CEv client-to-client payload relayed by SS sessionDesc, ICE candidate, iceRestartRequest, call, ping/pong. Names casing actual `c2SEv`, `c2CEv` (user c2CEV). Envelope exactly one payload slot c2SEv/s2CEv/c2CEv. Should clarify no direct WebSocket between clients: each connected to server; sender write its server WS, server forwards to receiver server WS. `WebSocketSSHandler` routes EPAddr after SS resolves. Client can send c2c if knows target `(channelId,subscriberId)`, discovered listChannelMembers. If doesn't know EPAddr fine. If doesn't know subscriber id? Could list members. "不管出于何种理由" likely challenges previous description and wants exact mechanism. Should explicitly distinguish knowing EPAddr not required by design, not fallback. Full step.

Potential subtlety: c2c outer To initially SS; receiver sees To changed own EPAddr; outer From remains sender's user/session, fromSubscriber unchanged. Provider validates source exists in channel but does it ensure c2c.FromSubscriber matches authenticated outer from? `handleRelay` only `ch.live(FromSubscriber)` not compare to ev.From. This might allow spoofing c2c fromSubscriber? Tests `TestForgedFromOverridden` maybe WS layer sets? Should check to avoid inadvertently say authenticated c2c sender. The question not security. But describe actual. It resolves both in channel. No need claim binding verification. Yet perhaps relevant addressing mechanism. Could mention SS requires both subscriber IDs valid/live in channel. It doesn't verify? Avoid overclaim.

- c2SEv to SS, server processes and responds s2CEv (registerResult, err, etc)
- c2CEv to peer via SS, not business handled deeply, except address resolution, optional fields.
- WebSocket handler stamping From at ingress and username. For c2SEv requests no registration required ping; other queries maybe no auth channel membership? not relevant.
- Both over same WebSocket JSON SignallingEvent envelope and msgId/inReplyTo. c2c events not replies typically? Call control etc.
- Should discuss `s2CEv` perhaps to distinguish.

Potential question "另外对于 c2CEV，如果一个 client 不知道另一个 client 的 EPAddr 它怎么把信令消息（通过 WebSocket）送到对方（不管出于何种理由）？" Respond by composite subscriber. Step:
1 A only knows B `channelId`+`toSubscriber`, gets from listChannelMembers.
2 A sets envelope `to = {serviceId: SS}`; c2CEv has target.
3 A's WS handler overwrites From with A authenticated EPAddr and forwards provider.
4 provider handleRelay uses channel state, resolves `to := ch.live(c2c.ToSubscriber)`, sets `ev.To = to.addr`.
5 output WS handler routes to learned connections for B EPAddr. It maps based on inbound From, not subscriber.
6 B receives event with c2c target info.
If B has not registered or expired => SubscriberNotFound to A. Unknown channel => ChannelNotFound. If B registered but socket temporarily disconnected: registration can remain, but handler may have no active connection; outbound dropped if address not learned on live conn. This is important actual delivery limitation: resolution to EPAddr not guarantee delivery. The phrase "怎么把信令消息送到对方" should be precise: If B online/learned, routed. If not, no queue; dropped. Docs says events destination never learned dropped; disconnect purges. Could mention to avoid implying guaranteed delivery. This is useful.

- If same EPAddr multiple connections, sends to every learned connection. Not necessary but perhaps helpful.
- "不管出于何种理由" perhaps user says imagine any reason client doesn't know EPAddr. It's normal. Emphasize **所有普通 client 都不需要知道对端 EPAddr**. That's intended design. Knowing only subscriber address is sufficient.
- EPAddr of target is deliberately hidden? Not necessarily deliberately hidden, receiver events outer From reveals it, and registration replies to. But sender needn't. Don't claim hidden for security.
- How discover subscriber id: listChannelMembers gives IDs; userProfileQuery metadata. I can show.
- An analogy: postal room alias mapped by SS; c2SEv talking to receptionist, c2CEv via receptionist to peer. Could help but perhaps no need after previous analogy.
- Should have citations paths:
  `web/site/src/api/ss/types.ts:100` and `:272`, `pkg/models/ss/ss.go:509`, `:766`, `pkg/api/websocket_ss/websocket_ss.go` perhaps lines unknown 19. docs. Use references no tools needed. But perhaps I should inspect c2SEV docs already exact. No tool needed. User asks conceptual no changes.

Could include concise sequence mermaid? Perhaps not needed, text easier. I can include a code block message and step. I should respond in Chinese.

Potential exact case `c2SEv` not c2SEV; say code uses `c2SEv`/`c2CEv`.

I'll draft:

"这两个字段是 SignallingEvent 的两种互斥 payload，差别在‘消息由谁处理’，不是两种连接。"

table:
c2SEv client→SS control plane membership/discovery; processed SS; response s2CEv.
c2CEv client→client logical, SS relay; payload WebRTC negotiation etc; no s2c success ack? In handleRelay no ack on success, error reply possible. Could mention errors.
"二者都跑在 client 与 server 的 WebSocket 上；c2CEv 不是 client-client WebSocket."

List fields c2SEv exact current: register, channelKeepAlive, userProfileQuery, channelProfileQuery, ping, listChannelMembers, listChannels. c2c exact: fromSubscriber,toSubscriber,channelId required; sessionDesc,rtcICECandidate,iceRestartRequest,call,ping,pong optional. Could mention payload only one optional intended but no need.

Then direct answer:
"A 不需要 B EPAddr. It addresses SS outer and B inner."
Code example with placeholders `main` perhaps channel actual UUID but use `<main-channel-id>`, not real IDs.

```
{
 from:{}, // stamped server
 to:{serviceId: SS_ID},
 msgId:"...",
 c2CEv:{channelId: MAIN,fromSubscriber:"A-ID",toSubscriber:"B-ID",sessionDesc:...}
}
```

No actual values needed.
Step diagram.
At end caveat: only addressing, not guaranteed; unknown/expired errors; no active WS drop, no offline queue. Does unknown target provider return err even if sender not know EPAddr yes.

Perhaps clarify "fromSubscriber" client sets but outer from server stamps. Fine.

I can respond with enough detail.
</thinking>

这两个字段都是 `SignallingEvent` 信封里的 **payload 类型**，区别在于：**这条消息最终由 SS 自己处理，还是由 SS 转发给另一个 client。**

它们不是两种不同的网络连接，都运行在同一套 WebSocket 信令通道上。

## 1. `c2SEv`：Client → Signalling Server

`c2SEv` 表示客户端**直接请求信令服务器做一些事情**，例如：

```ts
interface ClientToSSEv {
  register?: ClientToSSRegEv;
  channelKeepAlive?: ClientToSSChannelKeepAlive;
  userProfileQuery?: ClientToSSUserProfileQuery;
  channelProfileQuery?: ClientToSSChannelProfileQuery;
  listChannelMembers?: ClientToSSListChannelMembers;
  listChannels?: ClientToSSListChannels;
  ping?: PingPongMsg;
}
```

见 `web/site/src/api/ss/types.ts:100`。

典型用途包括：

- `register`：把自己注册成某个 channel 的 subscriber。
- `channelKeepAlive`：续期 subscriber 注册。
- `listChannels`：发现 channel。
- `listChannelMembers`：发现 channel 中的其他 subscriber。
- `userProfileQuery`：查询某个 subscriber 的显示名等资料。
- `ping`：检测客户端到 SS 的信令连接是否存活。

例如，A 查询 main channel 的成员：

```text
A ── c2SEv.listChannelMembers ──> SS
A <── s2CEv.channelMbsListResult ── SS
```

SS 处理请求后，通常用 `s2CEv` 回复，携带：

- 查询结果；
- `registerResult`；
- `pong`；
- 或者 `err`。

所以可以把 `c2SEv` 理解为：**客户端对信令服务器本身的控制面请求。**

## 2. `c2CEv`：Client → Client，经 SS 中转

`c2CEv` 表示逻辑上的**客户端到客户端消息**：

```ts
interface ClientToClientEv {
  fromSubscriber: SubscriberId;
  toSubscriber: SubscriberId;
  channelId: ChannelId;

  sessionDesc?: RTCSessionDescriptionInit;
  rtcICECandidate?: RTCIceCandidateInit;
  iceRestartRequest?: boolean;
  call?: ClientToClientCallEv;
  ping?: PingPongMsg;
  pong?: PingPongMsg;
}
```

见 `web/site/src/api/ss/types.ts:272`。

主要承载：

- `sessionDesc`：WebRTC SDP offer/answer；
- `rtcICECandidate`：ICE candidate；
- `iceRestartRequest`：请求对端发起 ICE restart；
- `call`：通话的 SIP 子集信令；
- `ping/pong`：client 之间的带外探测。

它的路径不是：

```text
A ═══════ WebSocket ═══════> B
```

而是：

```text
A ── WebSocket ──> SS ── WebSocket ──> B
```

SS 对 SDP、ICE 等业务 payload 基本不做处理，主要负责**验证地址所指的 channel/subscriber 存在，并转发消息**。

---

## 不知道对方的 `EPAddr`，怎么发送 `c2CEv`？

答案是：

> **Client 本来就不需要知道对方的 `EPAddr`。它只需要知道对方的 `(channelId, subscriberId)`。**

这正是两套地址同时存在的原因。

假设 A 是 subscriber `1000`，B 是 subscriber `1001`，两者都在 main channel。A 可以通过 `listChannelMembers` 发现 B 的 subscriber ID。

A 发出的消息大致是：

```ts
{
  // 外层地址：这条消息先交给 SS
  from: {},
  to: {
    serviceId: WELLKNOWN_SVC_ID_SS,
  },

  msgId: "message-123",

  // 内层地址：最终接收者是 B
  c2CEv: {
    channelId: WELLKNOWN_CH_ID_MAIN,
    fromSubscriber: "1000",
    toSubscriber: "1001",

    sessionDesc: {
      type: "offer",
      sdp: "...",
    },
  },
}
```

这里有两个目的地，作用不同：

- 外层 `to.serviceId`：**先把消息交给 SS**；
- 内层 `c2CEv.toSubscriber`：**告诉 SS 最终要转给谁**。

### 完整转发过程

```text
A
│
│ 1. 外层 to = SS
│    内层 toSubscriber = B
▼
WebSocketSSHandler
│
│ 2. 根据 A 的登录 session，覆盖外层 from
│    from = A 的 EPAddr
▼
SimpleOnMemorySSProvider
│
│ 3. 根据 c2CEv.channelId 找到 channel
│ 4. 根据 c2CEv.toSubscriber 找到 B 的注册项
│ 5. 从注册项取出 B 的 EPAddr
│ 6. 把外层 ev.To 改成 B 的 EPAddr
▼
WebSocketSSHandler
│
│ 7. 根据 B 的 EPAddr 找到 B 当前的 WebSocket 连接
│ 8. 将消息写入该连接
▼
B
```

核心代码就是 `pkg/models/ss/ss.go:766` 中的：

```go
to := ch.live(c2c.ToSubscriber, now, p.aging)
// ...
ev.To = to.addr
```

因此，A 不需要、也不应该自己填写 B 的 `EPAddr`：

```text
A 知道：
(channelId, B 的 subscriberId)
              │
              │ SS 查询注册表
              ▼
       B 的 EPAddr
              │
              │ WebSocket handler 查询连接表
              ▼
       B 的 WebSocket 连接
```

## 需要注意：能寻址不等于保证送达

这套机制只负责解析和路由，不是离线消息队列：

- channel 不存在：SS 给 A 返回 `ChannelNotFound`。
- B 没注册或注册已过期：返回 `SubscriberNotFound`。
- B 的注册仍有效，但当前没有可用的 WebSocket 连接：消息可能被丢弃，不会保存到离线队列。
- 同一个 `EPAddr` 当前关联多个连接时，WebSocket handler 会把出站事件发到它学到的这些连接。

所以，**client 不知道对方 `EPAddr` 完全不影响正常寻址；真正需要的是对方有效的 `(channelId, subscriberId)`，以及对方当前有可路由的连接。**
