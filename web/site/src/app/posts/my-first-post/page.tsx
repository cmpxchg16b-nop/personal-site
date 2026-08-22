"use client";

import PostView from "@/components/PostView";
import { H2, Li, P, Ul } from "@/components/prose";

// The example posts double as a demo of the prose building blocks (see
// src/components/prose.tsx) and as documentation for how the blog fits
// together. This one walks through the moving parts.
export default function YourFirstPost() {
  return (
    <PostView postId="my-first-post">
      <P>
        因为需要一个地方展示自己的项目、自我介绍以及提供短链接服务，所以有了这个网站的需求。
      </P>

      <H2>外观和风格</H2>
      <Ul>
        <Li>Dark Mode 优先</Li>
        <Li>响应式</Li>
        <Li>显眼的节标题</Li>
        <Li>Bilingual，继承自上一个项目 ExamServer</Li>
      </Ul>

      <H2>技术栈</H2>
      <Ul>
        <Li>Kimi K3</Li>
        <Li>Next.js + MUI + Golang</Li>
        <Li>Markup Lanauge 优先</Li>
        <Li>完全 AI 开发</Li>
      </Ul>

      <P>
        通过大篇幅地使用AI，以及让AI深度参与到项目的开发和设计流程当中，减少了之前很多的低级错误，项目的质量下限有了保证，整体而言规范了许多。当然这也离不开
        MUI 高质量的组件库。
      </P>
    </PostView>
  );
}
