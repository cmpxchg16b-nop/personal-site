"use client";

import { Box, Typography } from "@mui/material";

type SectionProps = {
  id: string;
  title: string;
  subtitle?: string;
  children: React.ReactNode;
};

// Section is the shared page-section scaffold: an anchored <section> with a
// header and optional subtitle, spaced like every other section on the page.
// scrollMarginTop keeps anchored navigation (#posts, #contact, …) from
// sliding under the sticky top bar.
export default function Section({
  id,
  title,
  subtitle,
  children,
}: SectionProps) {
  return (
    <Box component="section" id={id} sx={{ mt: 4, scrollMarginTop: 8 }}>
      <Typography variant="h4" component="h2" gutterBottom>
        {title}
      </Typography>
      {subtitle ? <Typography>{subtitle}</Typography> : null}
      {children}
    </Box>
  );
}
