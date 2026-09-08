"use client";

import NextLink from "next/link";
import {
  Box,
  Card,
  CardActionArea,
  CardContent,
  Typography,
} from "@mui/material";
import type { DynMedia } from "@/hooks/useDynBlogData";

// MediaCard renders one entry of the entertain page's media shelves: a cover
// thumbnail (or a placeholder tile carrying the shelf's icon when the entry
// configures none), the display name, and a two-line clamped description.
// The whole card is the link: site-relative hrefs navigate with Next.js,
// absolute ones open in a new tab — the same convention as the Posts
// section's Read button.
export default function MediaCard({
  media,
  placeholderIcon,
}: {
  media: DynMedia;
  placeholderIcon: React.ReactNode;
}) {
  const external = media.href.startsWith("http");

  return (
    <Card>
      <CardActionArea
        component={external ? "a" : NextLink}
        href={media.href}
        target={external ? "_blank" : undefined}
        rel={external ? "noreferrer" : undefined}
        aria-label={media.displayName}
      >
        {media.thumbnail ? (
          <Box
            component="img"
            src={media.thumbnail}
            alt={media.displayName}
            sx={{
              display: "block",
              width: "100%",
              aspectRatio: "16 / 9",
              objectFit: "cover",
              bgcolor: "black",
            }}
          />
        ) : (
          <Box
            aria-hidden
            sx={{
              display: "flex",
              alignItems: "center",
              justifyContent: "center",
              width: "100%",
              aspectRatio: "16 / 9",
              bgcolor: "action.hover",
              color: "text.secondary",
            }}
          >
            {placeholderIcon}
          </Box>
        )}
        <CardContent>
          <Typography variant="subtitle1" component="div" noWrap>
            {media.displayName}
          </Typography>
          {/* minHeight of two body2 lines keeps the cards of a grid row the
              same height when a description runs short. */}
          <Typography
            variant="body2"
            color="text.secondary"
            sx={{
              display: "-webkit-box",
              WebkitLineClamp: 2,
              WebkitBoxOrient: "vertical",
              overflow: "hidden",
              minHeight: "2.6em",
            }}
          >
            {media.description}
          </Typography>
        </CardContent>
      </CardActionArea>
    </Card>
  );
}
