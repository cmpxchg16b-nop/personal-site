"use client";

import PostView from "@/components/PostView";
import { P } from "@/components/prose";

export default function Page() {
  return (
    <PostView postId="a-so_reuseport-issue">
      <P>
        起因是需要调查偶发性的 404 not found 问题，然后回想起来自己的多个 caddy
        容器化实例都是 bind 了同一个 netns 的 [::]:443 socket。
      </P>
      <P>
        然后 caddy 竟然也都能正常启动，无报错，推测可能 caddy 默认启用了
        SO_REUSEADDR 和 SO_REUSEPORT 选项。
      </P>
      <P>
        进一步查阅资料得知，当多个进程通过 SO_REUSEADDR 和 SO_REUSEPORT
        监听同一个 ip:port 时，incoming connection 会根据四元组做 hash 然后
        load-balance 到每一个进程。
      </P>
      <P>
        举例来说进程 1 负责 host A，任何针对其它 host 的请求它都会报
        404；同理进程 2 负责 host B。然后，进程 1 和进程 2 都通过 SO_REUSEPORT
        复用 [::]:443。
      </P>
      <P>
        那么这就能解释偶发性 (intermittent) 的 not found
        或者证书错误等问题了。比如说，一个到 host B 的请求被 hash 到了进程 1。
      </P>
      <P>
        解决方法很简单，把两个 caddy 实例的配置文件合并到一起，然后只起一个 443
        和一个 80 http server，并且配置基于 host 的 caddy route
        进行匹配和分流即可。
      </P>
    </PostView>
  );
}
