"use client";

import { Typography } from "@mui/material";

type PostDescriptionProps = {
  // The post's description — its one- or two-sentence summary.
  children: React.ReactNode;
};

// PostDescription is the post's lead paragraph, rendered under the title
// and tags in secondary text.
export default function PostDescription({ children }: PostDescriptionProps) {
  return <Typography color="text.secondary">{children}</Typography>;
}
