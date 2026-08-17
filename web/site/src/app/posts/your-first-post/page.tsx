"use client";

import PostView from "@/components/PostView";
import {
  Code,
  CodeBlock,
  H2,
  Hr,
  Li,
  Link,
  P,
  Quote,
  Ul,
} from "@/components/prose";

// The example posts double as a demo of the prose building blocks (see
// src/components/prose.tsx) and as documentation for how the blog fits
// together. This one walks through the moving parts.
export default function YourFirstPost() {
  return (
    <PostView postId="your-first-post">
      <P>
        Welcome to the blog. This is an example post — its title, dates, and
        tags in the header above come from the server configuration, and its
        body is a <Code>.tsx</Code> file composed from a small set of building
        blocks. Replace it with your own writing when you are ready.
      </P>

      <H2>What this site is</H2>
      <P>
        The site is a static Next.js export embedded in a small Go server. Out
        of the box you get:
      </P>
      <Ul>
        <Li>
          Light and dark themes, following your system or toggled by hand.
        </Li>
        <Li>English and 中文 UI copy, switchable from the top bar.</Li>
        <Li>
          Post, project, and contact lists served live from the server&apos;s
          configuration file — no rebuild needed to edit them.
        </Li>
      </Ul>

      <H2>Editing this post</H2>
      <P>
        The header metadata lives in <Code>serverConfig.xml</Code> under{" "}
        <Code>dynBlogData</Code>; the Go server re-reads it on every request, so
        edits show up without a restart. The body you are reading is{" "}
        <Code>web/site/src/app/posts/your-first-post/page.tsx</Code>. For
        development, run the two side by side:
      </P>
      <CodeBlock>{`cd web/site && npm run dev   # http://localhost:3000, /api proxied to :8080
go run ./cmd/server --config-xml=serverConfig.xml`}</CodeBlock>

      <Quote>
        <P>
          Everything on this site is placeholder text. Make it yours — the copy
          lives in the translation bundles, the lists in the configuration file,
          and the posts in pages like this one.
        </P>
      </Quote>

      <H2>What&apos;s next</H2>
      <P>
        Head back to the <Link href="/#posts">posts list</Link> to see the other
        example posts, or browse the{" "}
        <Link href="https://github.com/your-handle">source on GitHub</Link> once
        you have pushed your fork.
      </P>

      <Hr />
      <P>Thanks for reading — and enjoy making this template your own.</P>
    </PostView>
  );
}
