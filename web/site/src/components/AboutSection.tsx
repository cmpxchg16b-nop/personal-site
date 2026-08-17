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
        关于我自己：INTP、软件开发工程师、开源项目参与者、计算机网络爱好者、业余无线电爱好者、CCNA、CCNP
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
      <NarrowP>我相信，在未来，一切工作流程都会由基于AI的自动化驱动。</NarrowP>
      <NarrowP>——手动分割线——</NarrowP>
      <NarrowP>
        关于站名和网名：网站的域名是 exploro.one
        意为「探索」。网站的站名我还没想好，也许就是 exploro
        吧？「秋信」是我随意起的一个网名，如果重复了请告诉我，我再想一个。关于头像？还没有，我也不会画画。我的
        LDAP DN 是什么？可以是 cn=i,ou=person,dc=exploro,dc=one 也可以是
        cn=qiuxin,ou=person,dc=exploro.one,dc=one. Never mind.
        我的真实资料会在我的简历上。
      </NarrowP>
    </Section>
  );
}
