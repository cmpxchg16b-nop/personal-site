"use client";

import { Children, isValidElement } from "react";
import { Box } from "@mui/material";
import PostDivider from "./PostDivider";
import PostDynHeader from "./PostDynHeader";
import PostHeader from "./PostHeader";

type PostViewProps = {
  // The post's id in the <dynBlogData/> section of serverConfig.xml —
  // forwarded to PostDynHeader, which fetches and renders the post's
  // server-configured metadata.
  postId: string;
  // The post's body, authored with the building blocks from
  // src/components/prose.tsx. May include one PostHeader element (see
  // below) to replace the server-driven header.
  children: React.ReactNode;
};

// PostView is the shared shell of every post page: the header, a divider,
// then the page's authored body.
//
// The header is the server-driven PostDynHeader unless the page authors its
// own: when a PostHeader element appears among the direct children, it is
// lifted out of the body into the header slot (identified by element type
// at render time, so it must be a direct child — not wrapped in a fragment
// or another component) and PostDynHeader, including its
// /api/dyn/posts/{id} request, is skipped entirely. PostDivider always sits
// between header and body, so an authored header needs no manual <Hr/>.
//
// Element type identity only holds when the page is itself a client module:
// a server component's children cross the RSC boundary as client references
// that no longer === PostHeader. Post pages must therefore start with
// "use client" (the site is fully client-rendered anyway).
export default function PostView({ postId, children }: PostViewProps) {
  const kids = Children.toArray(children);
  const authoredHeader = kids.find(
    (child) => isValidElement(child) && child.type === PostHeader,
  );
  const body =
    authoredHeader === undefined
      ? kids
      : kids.filter((child) => child !== authoredHeader);

  return (
    <Box component="article">
      {authoredHeader ?? <PostDynHeader postId={postId} />}
      <PostDivider />
      {body}
    </Box>
  );
}
