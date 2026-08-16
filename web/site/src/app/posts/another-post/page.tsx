import PostView from "@/components/PostView";
import { CodeBlock, H2, Li, Ol, P, Quote } from "@/components/prose";

// A shorter, notes-style example: an ordered list, a code block, and a quote
// are enough structure for a quick post.
export default function AnotherPost() {
  return (
    <PostView postId="another-post">
      <P>
        Not every post needs to be long. This one is a quick note — a few
        paragraphs, a list, and a snippet — showing that short entries look just
        as at home here as longer ones.
      </P>

      <H2>A running list</H2>
      <P>Three things worth remembering when writing for this blog:</P>
      <Ol>
        <Li>
          Draft in whatever editor you like; the post is just a component.
        </Li>
        <Li>Keep paragraphs short — the reading column is narrow by design.</Li>
        <Li>
          Update the metadata in the server configuration when you publish.
        </Li>
      </Ol>

      <H2>In code</H2>
      <P>
        Code blocks are bordered surfaces that scroll horizontally instead of
        wrapping, so long lines stay readable:
      </P>
      <CodeBlock>{`type Post = {
  id: string;
  title: string;
  tags: string[];
};

export function byNewest(a: Post, b: Post): number {
  return b.id.localeCompare(a.id);
}`}</CodeBlock>

      <Quote>
        <P>
          Brevity is a feature. Publish the note; expand it later if it earns
          it.
        </P>
      </Quote>

      <P>That is the whole post — short, structured, and done.</P>
    </PostView>
  );
}
