"use client";

import { useTranslation } from "react-i18next";
import { NarrowP } from "./prose";
import Section from "./Section";

// The About section: a few paragraphs of bio. The paragraphs live in the
// translation bundle as a list of placeholder strings.
export default function AboutSection() {
  const { t } = useTranslation();
  return (
    <Section id="about" title={t("about.title")}>
      <NarrowP>
        关于我自己：INTP、软件开发工程师、开源项目参与者、业余计算机网络爱好者、业余无线电爱好者、CCNA、CCNP
        Data Center 持证者。
      </NarrowP>
      <NarrowP>
        4年+ toB 商业项目开发经验。熟悉 Docker, Linux, Kubernetes。熟悉 React,
        Next.js, Golang 等开发技术。
      </NarrowP>
      <NarrowP>
        对 Linux 上的网络虚拟化技术，如 VRF, netns, VXLAN, WireGuard, SRv6
        等有一点的了解。熟悉 BGP、OSPF
        等动态路由协议。对思科技术栈，如数据中心的 SDN 解决方案 ACI
        也有一定的了解。对 Linux 上常用的路由守护进程软件 FRR, Bird
        亦有一定的了解。
      </NarrowP>
      <NarrowP>
        在工作和个人项目中，重度使用AI，力求AI参与度趋近100%，开发理念是：AI的重度参与能提升项目开发的速度和效率，减少人为引入错误的机会，AI化就是工程化。
      </NarrowP>
    </Section>
  );
}
