"use client";

import {
  Alert,
  Box,
  Chip,
  Divider,
  LinearProgress,
  Typography,
} from "@mui/material";
import { useTranslation } from "react-i18next";
import { localeTagFor } from "@/i18n";
import { useDynPosts } from "@/hooks/useDynBlogData";

type PostViewProps = {
  // The post's id in the <dynBlogData/> section of serverConfig.xml — the
  // single source of truth for the title, dates, and tags rendered here.
  postId: string;
  // The post's body, authored with the building blocks from
  // src/components/prose.tsx.
  children: React.ReactNode;
};

// PostView is the shared shell of every post page: a header built from the
// post's server-configured metadata (title, the same date/tag chips as the
// Posts cards, and the description as a lead paragraph), a divider, then the
// page's authored body. The header fills in once /api/dyn/posts answers —
// the same pattern the home sections use. If the metadata request fails (or
// has no entry for this id), the body still renders on its own.
export default function PostView({ postId, children }: PostViewProps) {
  const { t, i18n } = useTranslation();
  const { data: posts, isPending, isError } = useDynPosts();
  const post = posts?.find((p) => p.id === postId);
  const dateFmt = new Intl.DateTimeFormat(localeTagFor(i18n.language), {
    dateStyle: "medium",
  });

  // Same rule as the post cards: an edited post shows only when it was last
  // updated; the creation date adds noise.
  const updated =
    post !== undefined &&
    post.lastModified !== undefined &&
    post.lastModified !== post.creation;

  return (
    <Box component="article">
      {isPending ? (
        <LinearProgress sx={{ mb: 4 }} />
      ) : isError ? (
        <Alert severity="warning" sx={{ mb: 4 }}>
          {t("posts.loadFailed")}
        </Alert>
      ) : post ? (
        <Box component="header" sx={{ mb: 4 }}>
          <Typography
            variant="h4"
            component="h1"
            sx={{ fontWeight: 500 }}
            gutterBottom
          >
            {post.title}
          </Typography>
          <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5, mb: 2 }}>
            {updated ? (
              <Chip
                label={t("posts.updated", {
                  date: dateFmt.format(new Date(post.lastModified!)),
                })}
                size="small"
                variant="outlined"
              />
            ) : (
              <Chip
                label={dateFmt.format(new Date(post.creation))}
                size="small"
              />
            )}
            {post.tags.map((tag) => (
              <Chip key={tag} label={tag} size="small" />
            ))}
          </Box>
          <Typography color="text.secondary">{post.description}</Typography>
        </Box>
      ) : null}
      {/* Separate the header (or its error stand-in) from the body; when the
          post has no metadata entry the body stands alone. */}
      {(post !== undefined || isError) && !isPending && (
        <Divider sx={{ mb: 4 }} />
      )}
      {children}
    </Box>
  );
}
