"use client";

import PostView from "@/components/PostView";
import { H2, Li, P, Ul } from "@/components/prose";

export default function Page() {
  return (
    <PostView postId="media-streaming-solution">
      <P>一套相对完整的多媒体串流方案至少要包含下列要点：</P>
      <Ul>
        <Li>信源与采集</Li>
        <Li>编码(encoding)方式与封装(mux)方式</Li>
        <Li>传输以及信令</Li>
        <Li>播放端</Li>
      </Ul>

      <H2>信源与采集</H2>
      <P>信号源是我在 iPad 上打游戏的时候产生的画面和声音。</P>
      <P>
        采集方式：用 lightning-typeC 数据线连接 iPad 和 Macbook Air，打开 OBS
        软件，新增视频采集设备，在 OBS 界面上可看到音视频都已被识别：能看到 iPad
        上的画面，打开监听开关亦能听到游戏界面的声音。
      </P>

      <P>
        采集到的媒体流并不会实时推送到 media server，而是用 OBS
        的录制功能在本地先存起来，不合适的会被删除。
      </P>

      <P>
        对游戏画面进行 30fps 的采样得到的 1280x720
        分辨率的图像数据，音频数据：采样率是 48000Hz，位宽：16 bit，立体声。
      </P>

      <H2>编码与封装方式</H2>
      <P>
        这里的编码是说对原始图像（或音频波形）数据（YUV 或者
        RGB）进行高效率地保存和压缩的方式。对于存在本地的数据，我们用的是 h264
        yuv420p 视频编码和 flac 立体声无损音频编码。
      </P>
      <P>
        这里的封装是说如何把多个流，可以是不同范畴的流（如视频和音频），混合
        (mux) 在一个容器文件中，我们主要用到是 mkv（也叫
        Matroska）是一种容器格式。mkv 不仅能装 h264 和 flac，也能装
        opus（一种有损音频压缩格式）。
      </P>
      <P>
        出于存储效率的考虑，本地存储的 h264 视频有 B
        帧，但是推流消费端的播放器不支持 B
        帧，所以存储的视频格式和用户的播放器读到的视频格式是不一样的。并且，用户的播放器对
        flac 也不支持，需要转为 opus。
      </P>
      <P>
        我们的媒体服务器部署在一台高配 VPS
        上，能够存储大量的文件数据，也支持实时的 flac-to-opus 转码，但是对于
        h264-to-h264 的实时转码还是比较吃力，所以我们在本地用 Apple 的
        h264_videotoolbox codec（一种硬件加速的 h264 编码方式）进行转码去除 B
        帧然后再把转码结果（还是 mkv 文件）复制到服务器。
      </P>
      <H2>传输与信令</H2>
      <P>
        多媒体的消费端（用户设备 / Client）到媒体服务器的信令格式采用
        WHEP（算是一种 WebRTC 的扩展），基本上就是用 HTTP
        在浏览器和媒体服务器之间交换 SDP，WHEP 就「Client
        和媒体服务器之间怎样进行规范化的信令交换」做了细化。
      </P>
      <P>
        传输：一旦 WebRTC 信令交换完成，则 client
        和媒体服务器进行点对点的传输（IPv4-to-IPv4 或者
        IPv6-to-IPv6），不一定经过 HTTP 网关，Client 在会话中的角色是
        recvonly（仅接收），媒体服务器在会话中的角色是
        sendonly（仅发送）。媒体服务器向 client 发送 RTP 包，受到加密 DTLS
        信道的保护（每个 RTP 包的 datagram 都是被加密的），RTP 包里面 muxed
        了视频流 (h264) 和音频流 (opus)，通过 RTP 包头部的 SSRC 区分。
      </P>
      <H2>播放端</H2>
      <P>
        在主流较新版本的浏览器上播放，浏览器需要启用 JavaScript，支持较新版本的
        WebRTC（浏览器必须要是一个使能的 WebRTC agent），浏览器的 WebRTC
        子系统必须支持 Opus 和 H264 编码。客户端必须要有公网 IP 或公网 IPv6
        地址，这样媒体服务器才能推送。
      </P>
    </PostView>
  );
}
