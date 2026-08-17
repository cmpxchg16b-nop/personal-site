"use client";

import PostView from "@/components/PostView";
import { Code, CodeBlock, H2, H3, Hr, Li, P, Ul } from "@/components/prose";

// A longer example post: nested sections, several block types, and a divider,
// to show how a full-length article reads in this layout.
export default function AThirdPost() {
  return (
    <PostView postId="a-third-post">
      <P>
        This third example post shows how a longer article holds together:
        sections and subsections, lists, code, and a divider between acts. If
        your real posts run long, this is the shape they will take.
      </P>

      <H2>First act</H2>
      <P>
        Long-form writing needs rhythm. Headings break the text into digestible
        sections, and the spacing above each heading is generous so a new
        section feels like a new beginning rather than a continuation.
      </P>

      <H3>A subsection</H3>
      <P>
        Subsections use a smaller heading and sit closer to the text that
        introduces them. Use them sparingly — most posts only need one level of
        structure.
      </P>

      <H2>Serving the dynamic bits</H2>
      <P>
        The post list on the home page is not baked into the frontend. The Go
        server re-reads its configuration document on every request and serves
        the entries as JSON, which is why editing <Code>serverConfig.xml</Code>{" "}
        updates the site with neither a rebuild nor a restart. The handler is
        small enough to quote here:
      </P>
      <CodeBlock>{`func (h *DynamicBlogDataHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, err := h.provider.DynBlogData(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, route(r.URL.Path, data))
}`}</CodeBlock>
      <P>A few properties fall out of that design:</P>
      <Ul>
        <Li>Metadata edits are live the moment the file is saved.</Li>
        <Li>The frontend stays a fully static export.</Li>
        <Li>With no configuration mounted, the endpoints serve empty lists.</Li>
      </Ul>

      <Hr />

      <H2>Closing thoughts</H2>
      <P>
        A horizontal rule, like the one above, marks a clean break between
        loosely related parts of a post. Below it, wrap up: restate the point in
        a sentence or two and let the reader go.
      </P>
      <P>
        This is the last of the three example posts. Replace them with your own
        — the layout will take it from there.
      </P>
    </PostView>
  );
}
