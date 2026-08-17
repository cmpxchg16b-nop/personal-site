"use client";

import { Alert, LinearProgress } from "@mui/material";
import { useTranslation } from "react-i18next";
import PostDescription from "./PostDescription";
import PostHeader from "./PostHeader";
import PostTags from "./PostTags";
import PostTitle from "./PostTitle";
import { useDynPost } from "@/hooks/useDynBlogData";

type PostDynHeaderProps = {
  // The post's id in the <dynBlogData/> section of serverConfig.xml — the
  // single source of truth for the title, dates, and tags rendered here.
  postId: string;
};

// PostDynHeader is the post page's server-driven header: it fetches just
// this post's metadata via /api/dyn/posts/{id} and renders a loading bar
// while pending, a warning on failure, or the loaded PostHeader (title,
// date/tag chips, lead description). A post with no metadata entry renders
// nothing, so the body stands alone.
export default function PostDynHeader({ postId }: PostDynHeaderProps) {
  const { t } = useTranslation();
  const { data, isPending, isError } = useDynPost(postId);
  // A 404 answers null: no metadata entry, no header.
  const post = data ?? undefined;

  return isPending ? (
    <LinearProgress sx={{ mb: 4 }} />
  ) : isError ? (
    <Alert severity="warning" sx={{ mb: 4 }}>
      {t("posts.loadFailed")}
    </Alert>
  ) : post ? (
    <PostHeader>
      <PostTitle>{post.title}</PostTitle>
      <PostTags
        creation={post.creation}
        lastModified={post.lastModified}
        tags={post.tags}
      />
      <PostDescription>{post.description}</PostDescription>
    </PostHeader>
  ) : null;
}
