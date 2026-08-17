"use client";

import { Box } from "@mui/material";

type PostHeaderProps = {
  // The header's blocks — typically PostTitle, PostTags, and
  // PostDescription.
  children: React.ReactNode;
};

// PostHeader is the post page's header band: title, date/tag chips, and
// lead description stacked with the standard bottom spacing before the
// post body.
export default function PostHeader({ children }: PostHeaderProps) {
  return (
    <Box component="header" sx={{ mb: 4 }}>
      {children}
    </Box>
  );
}
