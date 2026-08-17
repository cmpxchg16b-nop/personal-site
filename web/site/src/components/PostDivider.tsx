"use client";

import { Divider } from "@mui/material";

// PostDivider separates the post's header (or its loading/error stand-in)
// from the body. PostView always places it between the two, so neither the
// server-driven header nor an authored PostHeader needs to bring its own.
export default function PostDivider() {
  return <Divider sx={{ mb: 4 }} />;
}
