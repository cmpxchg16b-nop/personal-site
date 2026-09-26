User: 在一个 SIP 电话网络中，假设1001@A.com希望呼叫help@B.com，help@B.com是一个企业 SIP 号码提供 IVR 交互式语音菜单服务。DTMF 通道如何声明（信令层面）以及 DTMF 事件如何传输（数据面）？
Kimi: 这是一个经典的 VoIP 问题，可以拆成两个阶段：**信令面协商"用什么方式传 DTMF"**，**数据面按协商结果实际传送按键事件**。主流做法是 RFC 4733（旧称 RFC 2833）的 telephone-event，下面以它为主线，同时覆盖 SIP INFO、KPML 和带内方式。

## 一、信令层面：DTMF 能力如何声明

### 1. RFC 4733：在 SDP offer/answer 中声明 telephone-event

这是最标准、最常用的方式。DTMF 能力作为 RTP 会话的一种**动态负载类型**（dynamic payload type，96–127）在 SDP 中声明。1001@A.com发出的 INVITE 中：

```sdp
m=audio 49170 RTP/AVP 0 8 101
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
```

help@B.com的 IVR 在 200 OK 的 answer 中回显：

```sdp
m=audio 52234 RTP/AVP 0 101
a=rtpmap:0 PCMU/8000
a=rtpmap:101 telephone-event/8000
a=fmtp:101 0-16
```

关键字段含义：

- `101`：双方为 telephone-event 协商的动态 PT 值，两端可以不同，以各自声明为准；
- `a=fmtp:101 0-16`：**本方向对方声明"我能接收的事件编号"**。0–9 是数字键，10 是`*`，11 是`#`，12–15 是 A–D，16 是 flash。这是有方向性的——发送方只能发送接收方 fmtp 里列出的事件；若对方没带 fmtp 行，发送方应默认只发 0–15；
- 只要 answer 里没有 telephone-event，就表示对端不支持，发送方需回退到带内方式。

### 2. SIP INFO：走信令通道，不在 SDP 中协商

INFO 方式（RFC 2976/6086，Cisco 设备常见）把 DTMF 放在会话内的 INFO 消息体里，通过`Allow: INFO`头部声明支持，不经过 SDP 媒体协商。RFC 6086 已把`application/dtmf-relay`标记为不推荐使用，但现网存量很大。

### 3. KPML：订阅式数字收集

应用服务器向用户话机发 SUBSCRIBE，用 XML digitmap（正则）描述要收集的按键模式，话机通过 NOTIFY 上报。常见于企业应用服务器集中收号的场景（RFC 4730）。

### 4. 带内音频：无需任何声明

DTMF 双音直接混在语音流里编码传输，对端靠 DSP 检测。无需信令声明，但只有 G.711 这类无损/低压缩编码下可靠，G.729 等低码率编码会严重扭曲双音频率导致误检。

## 二、数据面：DTMF 事件如何传输（RFC 4733）

媒体建立后，用户按"1"选择 IVR 菜单，事件以**独立 PT 的 RTP 包**发送，而不是音频。

### 承载关系

- 与语音**同一条 RTP 会话**（相同五元组、相同 SSRC、共享序列号空间），仅凭 PT=101 区分；
- 走媒体路径直达对端（或经 SBC），**不经过 SIP 代理**，因此实时性好；
- 若媒体面启用 SRTP，事件包与语音包一样被加密。

### 事件包格式

事件载荷只有 4 字节：

```
 0                   1                   2                   3
 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1 2 3 4 5 6 7 8 9 0 1
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
|     event     |E|R| volume    |          duration             |
+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+-+
```

- **event**：事件编号（如"1"=1）；
- **E**：End 位，标记事件结束；
- **volume**：音量（0 ～-63 dBm0 的绝对值）；
- **duration**：事件已持续的时长，**以时间戳单位计**（时钟率通常 8000 Hz）。

### 一次按键的实际包序列

假设按下"1"持续 250ms（8000Hz 下共 2000 个时间戳单位，包间隔 50ms）：

| 包  | PT  | M 位 | E 位 | 时间戳 | duration | 说明                 |
| --- | --- | ---- | ---- | ------ | -------- | -------------------- |
| 1   | 101 | 1    | 0    | T      | 400      | 事件开始，M 位置 1   |
| 2   | 101 | 0    | 0    | T      | 800      | 持续更新，时间戳不变 |
| 3   | 101 | 0    | 0    | T      | 1200     | 同上                 |
| 4   | 101 | 0    | 0    | T      | 1600     | 同上                 |
| 5   | 101 | 0    | 1    | T      | 2000     | 结束包，E=1          |
| 6   | 101 | 0    | 1    | T      | 2000     | 结束包重传第 2 次    |
| 7   | 101 | 0    | 1    | T      | 2000     | 结束包重传第 3 次    |

设计要点：

- **时间戳始终等于事件起点 T**，重传和续包共享同一时间戳，靠递增的 duration 表达进度；
- **结束包重复发送 3 次**，防止丢包导致对端"按键粘住"——对 IVR 来说这很关键，丢一个结束包可能让菜单逻辑误判成长按；
- 接收方据此精确还原**按了哪个键、按了多久**，duration 信息是带内检测难以稳定提供的。

### SIP INFO 的数据面

DTMF 走信令路径，逐跳经过代理：

```sip
INFO sip:help@B.com SIP/2.0
Content-Type: application/dtmf-relay

Signal=1
Duration=250
```

## 三、落到这个呼叫场景的完整流程

1. 1001@A.com → B.com 代理 → IVR：INVITE 携带 SDP offer（含 PT 101 telephone-event）；
2. IVR 回 200 OK 确认 answer，双方完成 DTMF 能力协商；
3. IVR 播放欢迎菜单（普通语音 RTP，PT=0）；
4. 用户按"1"，话机向 IVR 发送 PT=101 的事件包序列，同时停发或抑制语音包；
5. IVR 解析事件码 1，切换到下一级菜单，继续放音。

## 四、几种方式对比与工程注意点

| 方式     | 通道       | 可靠性         | 时延               | 编码依赖         | 典型用途             |
| -------- | ---------- | -------------- | ------------------ | ---------------- | -------------------- |
| RFC 4733 | 媒体面 RTP | 高（E 位重传） | 低                 | 无               | IVR、会议控制，主流  |
| SIP INFO | 信令面逐跳 | 中             | 高（经代理有抖动） | 无               | Cisco 互通、遗留系统 |
| KPML     | 信令面订阅 | 高             | 中                 | 无               | 应用服务器集中收号   |
| 带内     | 音频流内   | 依赖编码       | 低                 | 强（G.729 下差） | 模拟网关、PSTN 互通  |

实际部署中常见的坑：

- **两端能力不匹配**：一端只支持 INFO、另一端只支持 4733 时，需要 SBC/B2BUA 做 DTMF interworking（转换），企业边界上这几乎是标配功能；
- **PT 冲突**：级联多个 B2BUA 时动态 PT 可能被改号，需保证逐段协商正确；
- **安全**：INFO 方式下按键明文出现在信令里（账号密码类输入会泄露），4733+SRTP 则无此问题；
- **传真/Modem 音**也复用同一 telephone-event 机制（事件 32 等），不要把 fmtp 事件表过滤得太死。

总结：信令面用 SDP 的`telephone-event`+`fmtp`完成双向能力声明，数据面用同一会话内带独立 PT 的 RTP 事件包传送按键及持续时间——信令只负责"约定"，事件本身永远在媒体面流动。
User: 现在 SIP 网络换成基于 WebRTC 的网页客服平台，A 是顾客，在浏览器上和客服坐席 B 聊天，双方的 WebRTC 通过企业 B 的自定信令通道交换 SDP 和 ICE
User: 现在 SIP 网络换成基于 WebRTC 的网页客服平台，A 是顾客，在浏览器上和客服坐席 B 语音通话，双方的 WebRTC 实例通过企业 B 的自定信令通道交换 SDP 和 ICE candidate 建立了会话，A 和 B 之间直传 RTP 媒体流。那么 RFC4733 应该兼容？
Kimi: **兼容，而且是开箱即用的**——WebRTC 没有另造 DTMF 机制，直接把 RFC 4733 作为终端的必选实现（RFC 7874《WebRTC 音频编解码要求》明确规定 WebRTC 终端必须支持 telephone-event）。主流浏览器（Chrome/Firefox/Safari）的音频 m 行默认就带 telephone-event，所以"声明"这一步甚至不需要你写代码。变化的只是承载环境和 API 形态，下面分两层说。

## 一、信令层面：SDP 语义不变，通道换皮

企业 B 的自定信令通道扮演的角色，等价于原 SIP 场景里的 SIP 代理——但它**不解析、不修改 SDP**，只是把浏览器生成的 offer/answer 和 ICE candidate 原样搬运（WebRTC 的 offer/answer 状态机叫 JSEP，RFC 8829）。SDP 内部对 DTMF 的声明和 SIP 时代完全一样：

Chrome 实际生成的 offer 片段：

```sdp
m=audio 9 UDP/TLS/RTP/SAVPF 111 63 126
a=rtpmap:111 opus/48000/2
a=fmtp:111 minptime=10;useinbandfec=1
a=rtpmap:126 telephone-event/8000
```

要点：

- `126 telephone-event/8000`就是 RFC 4733 的声明，PT 值由浏览器自选；
- 浏览器通常**不带`a=fmtp:126`行**——按 RFC 4733 的规则，这表示接收方默认支持事件 0–15（0-9、`*`、`#`、A-D），对 IVR 场景完全够用；
- 对端 answer 中保留该 PT 即协商成功；如果被媒体服务器/SFU 剥掉，发送端的`sender.dtmf`会是`null`（下文细说）。

## 二、数据面：事件包格式不变，承载环境全换

按键事件的 4 字节载荷格式（event / E 位 / volume / duration）、开始包 M 位置 1、结束包 E=1 连发 3 次、时间戳锁定事件起点、duration 递增——这些和之前讲的 SIP 场景**逐字节一致**。变化的是它外面的"壳"：

| 维度       | SIP+RTP 场景              | WebRTC 场景                                                                                   |
| ---------- | ------------------------- | --------------------------------------------------------------------------------------------- |
| 加密       | RTP 可明文                | **强制 SRTP**（DTLS-SRTP 握手协商密钥），事件包载荷同样加密                                   |
| 传输复用   | 语音/DTMF 同一条 RTP 会话 | 更进一步：BUNDLE + rtcp-mux 强制，所有 m 行、RTP/RTCP 跑在**同一条 ICE 通道、同一个五元组**上 |
| 选路       | 按路由表直达              | 走 ICE nominated 的 candidate pair（可能经 TURN 中继），DTMF 包与语音包同路                   |
| 时钟       | 事件时钟通常 8000 Hz      | Opus 时钟 48000 Hz，但 telephone-event**仍独立用 8000 Hz**，互不影响                          |
| 信令面备选 | SIP INFO、KPML            | **不存在**。等价物是 RTCDataChannel 或信令通道（见下文）                                      |

## 三、浏览器 API：不直接摸 RTP 包，用 RTCDTMFSender

WebRTC 把 4733 封装成了高层 API，发送方 A：

```js
const sender = pc.getSenders().find((s) => s.track?.kind === "audio");

if (sender.dtmf) {
  // 队列式发送："1"按100ms，键间间隔70ms
  sender.dtmf.insertDTMF("1", 100, 70);
  // 可以连续排队：insertDTMF('123#') 会逐个发送
}
```

- 合法字符：`0-9`、`A-D`（不区分大小写）、`#`、`*`，逗号表示插入约 2 秒停顿；
- duration 范围 40–6000ms（默认 100ms），interToneGap 最小 30ms（默认 70ms）；
- `sender.dtmf.toneBuffer`可查看还在排队的按键。

接收方 B 通过`tonechange`事件感知按键（注意这个事件挂在对端的`RTCDTMFSender`对象上，是规范的一个反直觉设计）：

```js
const receiver = pc.getReceivers().find((r) => r.track.kind === "audio");
receiver.dtmf?.addEventListener("tonechange", (e) => {
  console.log("收到按键:", e.tone);
});
```

## 四、落到这个客服场景的工程建议

1. **两端都是浏览器：零成本**。telephone-event 自动协商，insertDTMF 直接可用，事件包经 SRTP 加密直传对端。
2. **中间有媒体服务器要当心**。如果 A 的媒体先经过 SFU/IVR 服务器（Janus、mediasoup、FreeSWITCH 等）再到坐席，telephone-event 可能被不识别的 PT 直接丢弃——mediasoup 默认就不转发，需要显式配置。这正是很多平台实测"DTMF 不通"的根因。
3. **客服/IVR 场景更务实的替代：RTCDataChannel**。如果只是把"用户按了 1"这个选择告诉服务器端菜单逻辑，走 DataChannel（或干脆走信令通道的 WebSocket）更省事：天然可靠有序、不怕媒体服务器拦截、不受 SRTP/PT 协商影响。代价是没有 4733 的 duration/时间戳语义，但菜单选择本来也不需要。
4. **要对接传统设备时，4733 是唯一通路**。如果坐席 B 后端实际连着 PSTN 网关、传统 IVR 或 SIP 话机，保持 RFC 4733 端到端，SBC 可以无缝做 WebRTC↔SIP 的 DTMF 互通。

一句话总结：信令面照旧用 SDP 声明 telephone-event（浏览器自动完成），数据面照旧发 4733 事件包（只是强制套了 SRTP、跑在 ICE 通道上），真正需要决策的是"这条链路里有没有会吃掉 telephone-event 的中间件"，以及"IVR 按键这件事是不是交给 DataChannel 更合适"。
User: 查一下用 pion/webrtc/v4 这个 golang 库收 DTMF 事件的方式 https://pkg.go.dev/github.com/pion/webrtc/v4#RTPReceiver
Kimi: 查完了，结论先说：**pion/webrtc v4 没有高层的 DTMF API**——类型索引里不存在 `RTCDTMFSender`，`RTPReceiver` 的全部方法就是 `Read/ReadRTCP/Track/Tracks/GetParameters/...`，没有任何 DTMF 相关入口 。所以"收 DTMF"在 pion 里的正确姿势是三步：**手动注册 telephone-event 编解码 → 从协商结果里拿到它的 PT → 在 OnTrack 里读 RTP 包自己解析 RFC 4733 载荷**。

另外文档里有个容易踩的点：`RTPReceiver.Read()` 读的是**RTCP**（"Read reads incoming RTCP for this RTPReceiver"），RTP 媒体包要从 `TrackRemote` 读 。

## 第一步：注册 telephone-event（必须，否则协商不出来）

`RegisterDefaultCodecs` 的源码里音频只注册了 Opus/G722/PCMU/PCMA 四项，没有 telephone-event 。不注册的话，浏览器的 offer 里虽然带着 `telephone-event`，pion 生成的 answer 会把它丢掉，对端就不会发事件包：

```go
mediaEngine := &webrtc.MediaEngine{}
if err := mediaEngine.RegisterDefaultCodecs(); err != nil {
	panic(err)
}
if err := mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
	RTPCodecCapability: webrtc.RTPCodecCapability{
		MimeType:    "audio/telephone-event", // v4 没有这个常量，用字面量
		ClockRate:   8000,
		SDPFmtpLine: "0-16",
	},
	PayloadType: 101,
}, webrtc.RTPCodecTypeAudio); err != nil {
	panic(err)
}

api := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine))
pc, err := api.NewPeerConnection(webrtc.Configuration{})
```

两个细节：

- pion 做 answerer 时会在 answer 里**回显浏览器给的 PT**（比如 Chrome 的 126），你本地注册的 101 只在 pion 主动 offer 时生效——所以运行时**不要硬编码 PT**，见第二步；
- fmtp 匹配是子集语义：`paramsEqual` 只比较两边都出现的 key，一边没有该行不影响匹配 。所以 Chrome 不带 fmtp 行也能精确匹配上，注册 `"0-16"` 或留空都行。

## 第二步：协商完成后取实际的 telephone-event PT

```go
var dtmfPT uint8
found := false
for _, c := range receiver.GetParameters().Codecs {
	if strings.EqualFold(c.MimeType, "audio/telephone-event") {
		dtmfPT, found = uint8(c.PayloadType), true
		break
	}
}
```

## 第三步：OnTrack 里过滤并解析

DTMF 事件包和语音**共享音频 track 的 SSRC**，会出现在同一个 `TrackRemote` 的读流里，仅凭 PT 区分。载荷就是那 4 字节，pion/rtp 的 codecs 包里没有任何 telephone-event 解析器（只有编解码器的 payloader/depacketizer），自己解析即可：

```go
pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	if track.Kind() != webrtc.RTPCodecTypeAudio {
		return
	}
	// …第二步取 dtmfPT，未协商到则直接 return…

	var lastEndTS uint32 // 结束包会重传，用于去重
	for {
		pkt, _, err := track.ReadRTP() // SRTP 已由 pion 内部解密
		if err != nil {
			return
		}
		if pkt.PayloadType != dtmfPT {
			continue // 普通语音包：转发/解码走这里
		}
		if len(pkt.Payload) < 4 {
			continue
		}

		code := pkt.Payload[0]               // 0-9=数字, 10=*, 11=#, 12-15=A-D
		end := pkt.Payload[1]&0x80 != 0      // E 位
		// volume := pkt.Payload[1] & 0x3F   // 一般用不到
		duration := binary.BigEndian.Uint16(pkt.Payload[2:4]) // 单位 1/8000 s

		if pkt.Marker && !end {
			log.Printf("DTMF 开始: %s", digit(code))
		}
		if end {
			if pkt.Timestamp == lastEndTS { // 结束包连发 3 次，去重
				continue
			}
			lastEndTS = pkt.Timestamp
			log.Printf("DTMF 结束: %s, 时长 %d ms", digit(code), duration/8)
			// 在这里触发 IVR 菜单逻辑
		}
	}
})
```

```go
func digit(code uint8) string {
	switch {
	case code <= 9:
		return string(rune('0' + code))
	case code == 10:
		return "*"
	case code == 11:
		return "#"
	case code <= 15:
		return string(rune('A' + code - 12))
	default:
		return fmt.Sprintf("event-%d", code)
	}
}
```

## 工程注意点

1. **不要把"开始"只押在 marker 位上**：个别实现首包不置 M，更稳的判定是"同一 (code, timestamp) 首次出现即开始"。
2. **加超时兜底**：若收到开始却一直没收到 E 包（结束包全丢了），按最后看到的 duration 加一个上限（比如 1–2 秒）主动判定结束，防止按键"粘住"。
3. **duration 时钟固定按声明的 rate**（这里是 8000 Hz），`duration/8` 得毫秒，与语音用 Opus/48000 无关。
4. **转发场景很方便**：因为 DTMF 就在音频读流里，做客服平台转发时一个读循环就能同时处理"语音包转发给坐席 + DTMF 包解析给 IVR"。
5. **发送方向同样没有现成 API**：`TrackLocalStaticRTP.WriteRTP` 会把 PT 改写成绑定的编解码 PT，想在同一 SSRC 上发 telephone-event 需要走更底层的写法（自建 interceptor 或直接写 SRTP session），这是 pion 长期未补的缺口，如果服务端需要主动发 DTMF 要有心理预期。

一句话：信令面靠 `MediaEngine.RegisterCodec` 把 telephone-event 补进协商，数据面就是 RFC 4733 的手工解析——pion 帮你做完了 ICE/DTLS/SRTP，DTMF 这层留给了应用。
User: 从浏览器 WebRTC API 的角度看，决定是否有必要渲染 Dial pad （按键面板）以供用户触发 DTMF 事件的可靠判据是什么？
Kimi: 可靠判据是 **`audioSender.dtmf?.canInsertDTMF === true`**——注意不是 `sender.dtmf !== null`。这两者的差别正是这个问题的关键，规范把它们拆成了两层：

## 一、为什么 `dtmf !== null` 不够

W3C 规范对 `dtmf` 属性的定义是：返回 `[[Dtmf]]` 内部槽，"未设置时为 `null`"，而这个槽**在 sender 的 track kind 为 `"audio"` 时就会被设置** 。也就是说：

- `dtmf === null` → 这个 sender 没有音频发送轨（track 为 null 或不是音频），彻底排除；
- `dtmf !== null` → 只说明"这是个音频 sender"，与 telephone-event 是否协商成功**无关**。

## 二、真正的判据：canInsertDTMF 的算法

规范第 7.3 节定义的判定算法，依次检查六项 ：

1. `pc.connectionState` 必须是 `"connected"`（ICE+DTLS 已建立）；
2. transceiver 没有被 stop；
3. sender 当前有 track；
4. `transceiver.currentDirection` 是 `"sendrecv"` 或 `"sendonly"`（被 hold 成 inactive/recvonly 时为 false）；
5. 第一个 encoding 的 `active` 为 true；
6. **该 sender 协商出了 `audio/telephone-event` 编解码用于发送** 。

前 5 项是连接/方向状态，第 6 项才是"协商维度"——正好对应前面聊的链路：如果对端（比如那个没注册 telephone-event 的 pion 服务端）在 answer 里没带回 `telephone-event`，第 6 项不成立，`canInsertDTMF` 就是 false。此时即使强行渲染按键并调 `insertDTMF()`，规范要求直接抛 `InvalidStateError` ，现实中就是 Chrome 控制台那句经典报错 "this sender cannot send DTMF" 。

## 三、时序陷阱与两段式判定

`canInsertDTMF` 的第 1 项要求连接已建立，所以**协商刚完成、ICE 还没通时它必为 false**——不能拿它做"渲染与否"的早期决策。实务上拆成两段：

- **协商完成时**（`setRemoteDescription` 返回后）：决定是否渲染面板。此时用 `sender.getParameters().codecs` 检查协商结果——它返回的就是"本次协商确定用于发送的编解码列表"，等价于 canInsertDTMF 的第 6 项，但不要求连接建立；
- **连接建立后**（`connectionstatechange` → `"connected"`）：决定按键是否可用。用 `canInsertDTMF` 做运行时闸门。

```js
function dialPadPolicy(pc) {
  const sender = pc.getSenders().find((s) => s.track?.kind === "audio");
  if (!sender?.dtmf) return "hidden"; // 无音频发送轨

  const negotiated = sender
    .getParameters()
    .codecs.some((c) => c.mimeType.toLowerCase() === "audio/telephone-event");
  if (!negotiated) return "hidden-or-datachannel"; // 对端没接受 telephone-event

  return sender.dtmf.canInsertDTMF ? "enabled" : "disabled";
}

// 两个时机各评估一次：setRemoteDescription 完成后、connectionstatechange 时
// hold/unhold 等重协商后也应重新评估（direction 变化会影响结果）
```

## 四、几个不可靠的判据，避免误用

| 判据                                                       | 问题                                                                                             |
| ---------------------------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `sender.dtmf !== null`                                     | 只表示音频轨存在，与协商无关（见上）                                                             |
| `RTCRtpSender.getCapabilities('audio')` 含 telephone-event | 静态的浏览器能力，不含任何协商结果——主流浏览器永远为真                                           |
| 检查本地 offer 的 SDP                                      | offer 只表达"我支持"，不表达"对端接受了"；要看也得看 answer，而 `getParameters()` 已经替你封装了 |
| 试调 `insertDTMF()` 捕获异常                               | 能工作但代价是抛异常，且无法区分"还没 connected"和"根本没协商"                                   |

## 五、落到客服平台的决策逻辑

- `'enabled'`：渲染并启用按键，走 RFC 4733；
- `'hidden-or-datachannel'`（对端未协商 telephone-event）：渲染一个按了没反应的拨号盘比不渲染更糟——要么隐藏，要么渲染但把按键改走 DataChannel/信令通道（前面讨论过的备选路径），此时面板照常可用，只是传输语义从"带时长的事件"降级为"按键消息"；
- `'disabled'`：面板在、按键置灰，等 `connectionState` 变为 `"connected"` 后放开。

一句话：渲染与否看协商结果（`getParameters().codecs` 里有没有 telephone-event），按键可用与否看 `canInsertDTMF`，两者都不要用 `dtmf` 是否非空来代替。
User: 这个平台是这样子的架构，这个用户 1001@A.com 不仅能外呼 help@B.com，理论上任何和 SIP A 服务器有 SIP 对等互联关系的 IP phone 都能打给 1001@A.com。但不是对于所有的 IP 电话，在 1001@A.com 的网页端都有渲染 dial pad （以发送 DTMF）的必要。

我想要表达的是，渲染 DTMF 当且仅当 SDP 协商层面看起来有需要渲染（1）以及能够渲染，其中（1）要分 1001@A.com 作为主叫和被叫的情形分别考虑，也就是分 1001@A.com 主动往外打电话（可以打给一个不需要 DTMF 普通电话或者打给一个 IVR）和 SIP 网络主动给 1001@A.com 打电话的情形。
Kimi: 这个框架可以形式化成一句话：**渲染 ⟺ 有能力渲染（SDP 可判定）∧ 有必要渲染（SDP 不可判定，需应用层上下文）**。结合你这张架构图逐层拆开：

## 一、"能够渲染"：纯 SDP 协商问题，但只在浏览器 ↔SBC 这一段判定

注意架构里的关键事实：浏览器的 WebRTC 对等体是 **B2BUA & SBC**，SIP A/SIP B 那一段的 SDP 对浏览器完全不可见。所以"能够"的判定天然分两段：

| 段                            | 判定方式                                                                                                                                                  |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 浏览器 ↔ SBC（WebRTC 段）     | 浏览器可直接判定：协商完成后 `sender.dtmf` 非空且 `sender.getParameters().codecs` 含 `audio/telephone-event`；运行时以 `sender.dtmf.canInsertDTMF` 为闸门 |
| SBC ↔ SIP A ↔ SIP B（SIP 段） | 浏览器**无法直接观测**。SIP 侧谈的是 4733、INFO 还是带内，由 SBC 做 interworking 转换                                                                     |

由此引出一个平台设计要求：如果你想要**端到端**的"能够"判据，SBC 必须把 SIP 侧的 DTMF 协商结果通过自定义信令通道透传给网页端（比如呼叫元数据里加 `remoteDtmf: "rfc4733" | "info" | "inband" | "none"`）。否则网页端的"能够"只覆盖到 SBC 为止——好在 SBC 做 DTMF 转换是标准能力，这段通常可以视为平台保证。

## 二、"需要渲染"：SDP 里没有这个信息位

SDP offer/answer 协商的是**能力**（能不能收发 telephone-event），不存在任何字段表达"对端是 IVR 还是人"——打给 IVR 和打给普通座机，协商出来的 SDP 一模一样。所以必要性必须按呼叫方向从别的层面取：

**主叫情形（1001@A.com 外呼）**，判定来源有两个，按可靠性排序：

1. **应用层上下文**——最可靠。用户是从"联系客服"按钮发起的呼叫，平台自己知道 `help@B.com` 是 IVR，渲染；用户拨的是普通号码，必要性未知。
2. **SIP 侧早媒体信号**——可用作弱提示。IVR/媒体服务器通常回 183 Session Progress 带 SDP 直接放菜单音，普通座机是 180 Ringing 等摘机。这发生在 SIP 段，浏览器看不到，但同样可以让 SBC 把"收到早媒体"作为提示位透传到网页端。

**被叫情形（任意 IP phone 呼入）**：判定来源是信令通道携带的**呼叫元数据**——主叫号码、是否来自已知队列/服务路由等。从与 SIP A 对等互联的任意话机进来的人对人呼叫，默认按"不需要"处理（不渲染或弱呈现）；元数据表明主叫是自动化系统（如"请按 1 确认"类回呼业务）时再渲染。

## 三、合成决策矩阵

| 场景                           | 需要？           | 能够？              | 结论                                                       |
| ------------------------------ | ---------------- | ------------------- | ---------------------------------------------------------- |
| 主叫客服 IVR（应用已知目的地） | 是（应用层判定） | 是（4733 协商成功） | 渲染                                                       |
| 主叫普通号码                   | 未知             | 是                  | 保守渲染（见下）                                           |
| 被叫，普通 IP phone 呼入       | 否（元数据判定） | 是                  | 不渲染/弱呈现                                              |
| 被叫，自动化系统呼入           | 是（元数据判定） | 是                  | 渲染                                                       |
| 任意方向，SBC 未协商 4733      | —                | 否                  | 不渲染，或渲染但改走 DataChannel/信令通道由平台转 SIP INFO |

## 四、两个判定失败时的默认策略

- **"需要"判定为未知时倾向渲染**：传统电话的通话中键盘常驻不是偶然——通话中途转接进 IVR、输入会议 PIN 都是事后才出现的 DTMF 需求，主叫普通号码时无法排除这种可能。代价只是一个 UI 组件。
- **"能够"判定为否时优雅降级**：面板渲染与否和传输通道解耦——按键照样收集，改走 DataChannel 或信令 WebSocket，由平台侧转换成 SIP INFO/4733 注入 SIP 段。此时 4733 的 duration 语义丢失，但对菜单选择类交互没有影响。

回到你的表述，只需把"（1）在 SDP 协商层面看起来有需要"修正为"（1）在**呼叫上下文层面**有需要"——主叫看应用层目的地知识（辅以早媒体提示），被叫看信令元数据；SDP 协商层面能且只能回答问题（2）。
User: 我再强调一遍以防你把问题考虑得过度复杂：

1. 需要从 SDP 协商层面判定 necessity 。
2. 允许假阳性，也就是 SDP 判据看起来 necessity 为真但实际上不需要（打给一个真人）。
   Kimi: 明白，允许假阳性的话，问题就收敛了：**necessity 和 capability 合并成同一个 SDP 谓词——"协商结果里包含 telephone-event（发送方向）"**。真值为假时不渲染；为真时渲染，哪怕对端其实是真人（假阳性无害，按键面板摆在那里不用即可）。

## 统一判据

协商完成后（`setRemoteDescription` 返回）：

```js
const sender = pc.getSenders().find((s) => s.track?.kind === "audio");
const showPad =
  !!sender?.dtmf &&
  sender
    .getParameters()
    .codecs.some((c) => c.mimeType.toLowerCase() === "audio/telephone-event");
```

运行时用 `sender.dtmf.canInsertDTMF` 做按键可用性闸门（它额外要求连接已建立）。

## 分方向看，判据是同一个，只是读取位置不同

**主叫（浏览器是 offerer）**：Chrome 的 offer 默认就带 telephone-event，所以信息全在 answer 里——SBC 的 answer 保留了 telephone-event，谓词为真，渲染。

**被叫（浏览器是 answerer）**：信息在 SBC 发来的 offer 里——offer 带 telephone-event，浏览器（Chrome 默认）会在 answer 中接受，谓词为真，渲染。

两个方向最终都收敛到同一件事：**本次协商的交集里有没有 telephone-event**，`getParameters().codecs` 就是这个交集的官方读法。

## 让这个谓词真正携带 necessity 信息的前提

浏览器看到的只是 WebRTC 段。这个 SDP 判据要包含"需要"的语义，SBC 必须做一个映射策略：**仅当 SIP 侧链路协商出了某种 DTMF 通道时，才在向浏览器的 offer/answer 中放行 telephone-event；SIP 侧没有 DTMF 能力时就把它剥掉**。

- 做了这个映射：谓词为假意味着"SIP 侧根本没有 DTMF 通路"，不渲染是正确的；谓词为真分两种——对端是 IVR（真阳性）或对端是支持 4733 的真人话机（假阳性），都渲染，符合你的容忍度。
- 没做这个映射（SBC 无条件和浏览器谈 4733）：谓词退化成恒真，只剩 capability 没有 necessity——此时判据仍然"安全"（永不漏渲染），只是"不需要时不渲染"这条省不掉了。

考虑到现网 IP phone/SIP 链路绝大多数都支持 4733，这个判据的假阳性率本来就高——所以它的实际价值全在"为假"那一支：SIP 侧是纯带内或 SBC 明确拒绝时，网页端据此不渲染。平台是你自己的，SBC 上配一条"telephone-event 透传取决于 SIP 侧协商结果"的策略，这套逻辑就闭环了。
User: 我认为更好的实现是像 linphone 那样 dial pad 始终展示（对应这个 webrtc ip 电话的情形就是只看 capability），然后只在 ui 层面允许用户 hide/show dial pad ？
Kimi: 同意，这个分解更干净。本质上是把一个二元决策拆成了两个正交的问题：**键盘"存不存在"由 capability 自动判定，键盘"显不显示"交还给用户**。前面讨论的 necessity 分析并没有浪费——它从"渲染门"降级为"默认展开/收起的提示"，这才是它该在的位置。

## 为什么这是更优解

1. **误判成本不对称**：需要键盘时没渲染（用户对着 IVR 干瞪眼）是功能性故障；不需要时渲染了只是一块闲置 UI。成本相差悬殊，默认策略就该倾向常驻。
2. **necessity 不仅协商时不可知，通话中还会变**：打给真人，聊到一半被转进 IVR、要输会议 PIN——这类需求在呼叫建立的瞬间不存在，SDP 永远不可能表达。任何自动判定 necessity 的逻辑都在对抗一个本质上不可判定的量。
3. Linphone 和几乎所有硬话机/软话机的 in-call 键盘常驻、用户 toggle，正是对这个现实的工程回应。

## 落地结构

| 层                        | 判据                                                                   | 失败时                                                                    |
| ------------------------- | ---------------------------------------------------------------------- | ------------------------------------------------------------------------- |
| 存在性（自动）            | telephone-event 协商成功（`getParameters().codecs` / `canInsertDTMF`） | 不渲染，或渲染但按键改走 DataChannel/信令通道——别留一个按了没反应的死键盘 |
| 默认可见性（上下文 hint） | 主叫 `help@B.com` → 默认展开；被叫人对人 → 默认收起                    | 猜错无所谓，用户一键切换                                                  |
| 可见性（用户）            | toggle 按钮 + 记住偏好（localStorage/账号设置）                        | —                                                                         |

## 一个配套的平台侧推论

UX 定为常驻键盘之后，SBC 的最优策略跟着变了：**对浏览器一侧恒定协商 telephone-event，SIP 一侧的 DTMF 交付（4733/INFO/带内转换）由 SBC 自己兜底**。这样 capability 从前端的"判定条件"变成"平台常量"，浏览器里那个 `getParameters` 检查退化为纯粹的故障探针（值不为真说明 SBC 配置退化，该报警而不是该隐藏 UI）。

这比"SBC 按 SIP 侧结果决定是否放行 telephone-event"更适合常驻键盘的模型：前者把 SIP 侧的异构性屏蔽在平台内部，前端逻辑归零；后者省的是"不需要时不渲染"的一点 UI 成本，代价是把 SIP 段的协商结果耦合进了前端渲染逻辑。

总结成一句：**capability 决定键盘存不存在，necessity 只决定默认展开还是收起，最终可见性归用户**——SDP 检查从 UI 决策者降级为一条监控断言。
User: webrtc 这边可能不只有一次信令层面的协商，可能会有多次协商，协商结果也会变，我怎么监听这个变化？比如 telephone-event 能力一开始没声明后面又声明了，比如一开始没有视频，后面又有了视频
Kimi: WebRTC 没有"能力变化"这种细粒度事件——没有 `dtmfchanged`、没有 `codecchanged`。正确的模式是：**用几个粗粒度事件当触发器，每次触发后重新从当前状态推导一遍结果，再和上一次 diff**。状态本身随时可读，难的是选对"重新读"的时机。

## 可用的事件及其角色

| 事件                    | 角色           | 说明                                                                                                            |
| ----------------------- | -------------- | --------------------------------------------------------------------------------------------------------------- |
| `negotiationneeded`     | **发起方**信号 | 本地 addTrack/addTransceiver/改方向后触发，告诉你"该重新协商了"。它不含结果，别在这里读协商结果                 |
| `signalingstatechange`  | **结果检查点** | 一轮 offer/answer 完成、回到 `"stable"` 时触发——这是读协商结果的时机。每次状态跃迁都触发，要判断 `=== 'stable'` |
| `connectionstatechange` | 运行时闸门     | `canInsertDTMF` 依赖 `connectionState === 'connected'`，连接态变化要重估                                        |
| `track`                 | 媒体集合变化   | 远端新增轨道（如后来加了视频）时触发；注意它在 `setRemoteDescription(offer)` 处理过程中就触发，早于 stable      |
| track 的 `ended`/`mute` | 媒体消失       | 对端移除轨道时轨道被 mute/end                                                                                   |

关键结论：**没有一个事件覆盖全部变化，但每个变化都会落到"重推导函数"能读出的当前状态上**，所以事件只负责叫醒它。

## 推荐模式：单一 refresh + diff

```js
let prev = {};

function refresh() {
  // 每次重新找，不要缓存——重协商后 transceiver 集合可能变了（加视频 = 新 transceiver）
  const audioSender = pc.getSenders().find((s) => s.track?.kind === "audio");

  const now = {
    // telephone-event 后来才声明 / 被移除，都会反映在这里
    dtmfNegotiated: !!audioSender
      ?.getParameters()
      .codecs.some((c) => c.mimeType.toLowerCase() === "audio/telephone-event"),
    // 运行时闸门：连上且方向允许发送时才为 true（被 hold 成 inactive 时为 false）
    canSendDTMF: !!audioSender?.dtmf?.canInsertDTMF,
    // 视频从无到有：看 transceiver 的接收方向和实际轨道
    hasRemoteVideo: pc
      .getTransceivers()
      .some(
        (t) =>
          t.receiver.track?.kind === "video" &&
          t.currentDirection?.includes("recv") &&
          !t.receiver.track.muted
      ),
  };

  if (now.dtmfNegotiated !== prev.dtmfNegotiated)
    ui.setDialPadExists(now.dtmfNegotiated);
  if (now.canSendDTMF !== prev.canSendDTMF)
    ui.setDialPadEnabled(now.canSendDTMF);
  if (now.hasRemoteVideo !== prev.hasRemoteVideo)
    ui.setVideoVisible(now.hasRemoteVideo);
  prev = now;
}

pc.onsignalingstatechange = () => {
  if (pc.signalingState === "stable") refresh();
};
pc.onconnectionstatechange = refresh;
pc.ontrack = (e) => {
  if (e.track.kind === "video") attachVideoElement(e.track); // 元素挂接还是得在 ontrack 做
  refresh();
};
pc.onnegotiationneeded = async () => {
  /* 跑你的 offer/answer 流程，完成后 stable 会触发 refresh */
};
```

`refresh()` 每次从 ground truth（`getSenders()`/`getTransceivers()`/`getParameters()`）重新推导，所以事件漏挂一两种也不会出错，顶多 UI 晚一拍——这是这个模式比"逐事件增量维护 UI 状态"稳健的原因。

## 几个与重协商相关的细节

1. **同一 m-line 重协商，sender/transceiver 对象不变**。audio 那条线反复谈，`audioSender` 和它的 `dtmf` 对象是同一个，变的是 `getParameters()` 和 `canInsertDTMF` 的返回值——直接重读即可。
2. **新增媒体 = 新 transceiver**。视频后加会多出一个 transceiver，所以"找音频 sender"这类查询不要缓存结果。
3. **`getParameters().codecs` 的时序**：首轮协商完成前是空列表，完成后反映"本次协商确定的发送编解码"。telephone-event 中途出现/消失（比如客服场景里的**呼叫转接、升级**：从 IVR 转人工、转 PSTN 网关，SBC 对浏览器重新 offer，DTMF 能力随之翻转）正是在这里观察。
4. **hold 是免费的副产品**：对端 hold 呼叫会把 `currentDirection` 变成 `inactive`，`canInsertDTMF` 变 false，上面的 diff 会自动把键盘置灰——hold 期间禁按 DTMF 本来就是合理 UX。
5. 远端发起的重协商（对方加视频）在你这边的事件序是：`ontrack`（SRD 处理中）→ `signalingstatechange → stable`（你 setLocalDescription(answer) 后），两个钩子都挂上就覆盖了。

一句话：**`negotiationneeded` 管"发起"，`stable` 管"读结果"，`connectionstatechange` 管"能不能按"，媒体增减靠 `ontrack` 兜底，全部汇入同一个重推导函数做 diff**——这套结构对任意多次重协商都成立。
