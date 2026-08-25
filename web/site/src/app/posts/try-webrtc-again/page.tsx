"use client";

import PostView from "@/components/PostView";
import { H2, Li, P, Ul } from "@/components/prose";
import { Link } from "@mui/material";

// The example posts double as a demo of the prose building blocks (see
// src/components/prose.tsx) and as documentation for how the blog fits
// together. This one walks through the moving parts.
export default function TryWebRTCAgain() {
  return (
    <PostView postId="try-webrtc-again">
      <P>
        再一次尝试开发 WebRTC
        相关的个人项目一个是出于兴趣使然；二个是觉得再自己的个人博客里集成
        WebRTC 比较酷，比如，可以实现基于 WebRTC
        的点对点IP直连聊天和视讯或以及音频点播功能等；第三个原因是试试看，用最新的
        Kimi K3
        能不能做得好一点，当时上一个版本用的是GLM-5.0和GLM-5.1（虽然现在都迭代到GLM-5.3了，也确实变强了不少），再加上当时相当一部分代码是手写的，设计得也很差，想着就是重新实现一个会不会好很多。
      </P>

      <H2>有什么发现</H2>
      <Ul>
        <Li>WebRTC 仍然不是很稳定</Li>
        <Li>AI写的代码确实很规整</Li>
        <Li>
          一部分用户已经习惯性禁用WebRTC了，因为WebRTC leak的问题早已普及开来
        </Li>
      </Ul>

      <H2>技术栈</H2>
      <Ul>
        <Li>
          Kimi K3
          负责实现我的大部分思路，可能这个需求比较偏，出现了比以前多的多的令我不满意的地方
        </Li>
        <Li>
          Golang + github.com/gorilla/websocket + github.com/pion/webrtc
          实现的信令服务器（简称SS），其中 pion/webrtc 主要是提供 WebRTC
          方面的类型定义，SS大部分时候只是 relay client 到 client
          之间的信令报文。
        </Li>
        <Li>前端还是 Next.js + MUI + React</Li>
        <Li>没用什么数据库，一切都存在内存里面</Li>
        <Li>
          辅以人工上层设计：关于各种接口、概念、数据类型和模块之间的边界和交互等
        </Li>
      </Ul>

      <P>
        相比上一个版本，这次做的东西，在登录这方面体验好了很多，流程更加顺其自然了。增加了一个
        redirect_if_succeed 参数，登录成功后能够跳转到指定的接口，并且在后端做了
        allowedOrigins
        校验。后端服务器（SS目前和我的博客共用一个单进程服务器实例）天生支持多域名访问，事实上
        nexus.dn42 和 exploro.one 都是一个站。
      </P>

      <P>
        也是继承自上上一个项目 ExamServer
        和上一个项目（也就是这个博客），很多配置都是在 serverConfig.xml
        这个文件里面实现的，在同目录下的 serverConfig.xsd 定义了 schema
        进行校验。
      </P>

      <P>
        在开发这个项目的时候，以及最近几个项目，经常会遇到的就是模型的上下文不够用，大约
        150K 输入上下文之后，模型从几乎不会犯错变成偶发犯错，到 250K
        之后，几乎就像傻逼一样了。那水平就像是人写的代码一样——没法看。所以在这个项目，我引入了
        session memory
        机制，就是，在我认为必要的时候，让AI把当前的对话总结下来，写成一篇
        markdown 格式的
        note。后续，视具体情况，决定是否引用之前的记忆，以及引用哪一份/哪几份记忆。这样做了之后确实好了许多，不用每一次都手动通过写
        prompt 的时候写前情提要了。
      </P>

      <H2>有什么设计</H2>
      <P>
        设计方面，大部分还是沿用我在第一个版本做的设计，但是在细节上则改良了许多。事实上不能说大部分相同，是指，实际上，有点熟悉又有点陌生。
      </P>
      <P>
        首先从信令服务器 (SS)
        开始说，信令服务器在之前的那一个版本的实现实际上是非常混乱的，基本上模块的划分是为了划分而划分，为了整洁而整洁。怎么说呢，就好比，收拾家具的时候，看起来似乎很有条理，东西都收纳起来了，但是需要找的时候根本不知道在那里找。处于一种看起来有秩序但是很低效的模块划分，也根本不合理。
      </P>
      <P>
        然后这一个版本的SS的设计就合理了许多。我先定义了SS的报文的格式以及一些基本的概念，主要是封包的格式，统一了，然后定义了一个
        SS 服务的接口类型，golang
        接口类型，然后让AI实现一个纯内存版本的SS服务接口的实现类。这相当于MVC架构模型的
        model 层，或者你把它看作是 service 层就行。然后我又让AI定义了一个
        WebSocket http handler，这个算是 controller
        层。但严格来说其实不是严格意义上地按照MVC的模式来进行划分/分工。
      </P>
      <P>
        SS 服务接口的设计非常简单：就一个公开的 Run 方法，接受一个 ctx，一个 SS
        信令报文的只读channel和一个 SS
        信令报文的只写接口，没了。你如果把这个SS服务类看作是一个两个网口的路由器就会觉得非常形象。
      </P>
      <P>
        嗯，那既然有了「路由器」，「交换机」又是什么呢？刚才我们说了其实并不是严格地按照MVC的模式分工，也就是说
        handler 其实是有参与一些工作的，SS服务类会根据 subscriber id 到 endpoint
        addr 的对应关系，改写 SS 信令报文的 DA 字段，然后 SS
        实现类就把这个改写了 DA 地址的 SS 信令报文送到只写 channel，然后 handler
        从这里拿到 SS 信令报文，它就会查找自己的 CAM 表，找到 endpoint address
        到 websocket 的 ip:addr 的对应关系，把 SS 信令报文送到正确的 websocket
        连接。所以，这里，实际上 handler 就是那台「交换机」。
      </P>
      <P>
        subscriber id 类比为 ip 地址，channel id 类比为 vlan，然后 endpoint
        address 类比为 mac 地址，SS 服务类类比为路由器，handler 类比为交换机。
      </P>
      <P>
        在前端，使用了单例模式，就是整个 app 只有一个 instance/object 实际复制到
        SS 的 websocket 连接，通过 React 的 context 提供给下游所有有需要的 hook
        和 component。这个单例，或者你可以把它叫做 SS
        proxy（意为信令服务器代理），代理了整个前端所有到 SS
        的沟通需求，就是说，前端的任何一个 component 也好 hook
        函数也好什么也好，当需要和 SS 通信的时候，它会先取得全局的 SS proxy
        单例，然后向 SS proxy 获取一个 readable stream 或者 writable
        stream，通过 SS proxy 返回的 stream 来和 SS
        通信。后续，也有可能不用浏览器 JavaScript 原生的 Stream，而是用 RxJS
        这样的成熟类库。
      </P>
      <P>
        然后前端除了用了单例和基于 stream 的 SS proxy
        访问模式之外。还大量的使用了 hook
        函数和模块分工来增加代码的可读性和项目整体上的可维护性。我还让AI针对整个
        signalling server 的设计写了文档并且实时更新，包括前面说了的 session
        memory
        机制在内，都是针对保护项目可维护性做的努力。其实有一说一，前端这块的代码是非常多的，毕竟要自己管理
        RTCPeerConnection 以及 DataChannel，以及 in-band 或者 out-of-band
        的消息的处理，要写很多代码。如果不划分模块也不抽取 hook
        和函数的话，很快就不管是人还是AI都看不懂了。
      </P>

      <H2>更加具体一点的实现思路</H2>
      <P>
        前端一个要解决的问题就是，假设现在能建立 datachannel
        了，如何把消息从一端同步到另一端，我们实际上并不区分发起者和接受者，因为我们用了
        perfect negotiation 模式。
      </P>
      <P>
        针对聊天界面的消息，我们定义了一些类型，语义上接近MIME，也就是说有各种字段然后有一个字段存消息内容，in-band的聊天控制信令也是以类似的方式定义，设计上我们一开始就是允许
        in-band 的 chat message 的 on-the-wire
        格式和内存中的表示是解耦的，在内存里我们用一个 JavaScript/TypeScript 的
        ChatMessage 类型（实际上是一个 union 类型）来表示各种 chat message 以及
        chat control message。
      </P>
      <P>
        实际的文件传输，包括附件传输，视频传输和图片传输，不走我们刚才说的 chat
        message 的通道，实际上对于每一个 RTCPeerConnection 共有两个 DC，chat
        message 用一个 label 为 dcmsg 的 DC，另一个传文件二进制内容用的是 label
        为 dcbin 的
        DC。然后我们自己实现了一套类滑窗机制和分片重组机制来传输大文件。这是因为浏览器的
        WebRTC 的 datachannel 默认用 SCTP 作为底层协议，而不是 TCP。当然，SCTP
        仍然是可靠的并且保持顺序的，只不过 SCTP 面向的是 datagram 而不是
        stream，你仍然需要把一个大文件切成 SCTP 能理解的一系列 datagrams
        来传。我们还设计了文件接受端向文件发送端发送端 ACK
        帧。设计这些东西不难，基本上我只提出 TLV 方面的东西，然后各种细节都让 AI
        给我脑补了。
      </P>
      <H2>如何访问</H2>
      <P>
        访问{" "}
        <Link underline="hover" href="https://exploro.one/chat">
          https://exploro.one/chat
        </Link>{" "}
        或者{" "}
        <Link underline="hover" href="https://chat.dn42">
          https://chat.dn42
        </Link>{" "}
        （假如你有 DN42
        连接），首次访问如果还没有登录或者登录已过期会提示需要登录。
      </P>
      <P>
        或者回到本博客的首页，划到页面较底部，然后点击 Chatroom 按钮即可进入。
      </P>
      <P>
        访问{" "}
        <Link
          underline="hover"
          href="https://github.com/cmpxchg16b-nop/personal-site"
        >
          personal-site
        </Link>{" "}
        阅读源码和设计文档。
      </P>
    </PostView>
  );
}
