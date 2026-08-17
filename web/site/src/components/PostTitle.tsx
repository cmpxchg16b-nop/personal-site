"use client";

import { Typography } from "@mui/material";

type PostTitleProps = {
  // The post's title.
  children: React.ReactNode;
};

// PostTitle is the post page's h1, sized like a page heading with the
// site's medium font weight — the same weight as the section headings.
export default function PostTitle({ children }: PostTitleProps) {
  return (
    <Typography
      variant="h4"
      component="h1"
      sx={{ fontWeight: 500 }}
      gutterBottom
    >
      {children}
    </Typography>
  );
}
