"use client";

import NextLink from "next/link";
import { Box, Divider, Link as MuiLink, Typography } from "@mui/material";

// The building blocks for post bodies. Posts are .tsx files: their content is
// composed from these components (plus PostView for the header) instead of
// bare HTML tags, so every post gets the same typography, spacing rhythm, and
// theme-aware surfaces without repeating sx boilerplate.
//
// The kit is a client module because Link passes NextLink around as a
// component prop, which cannot cross the server/client boundary. That costs
// nothing here: the app is purely client-rendered (I18nProvider gates the
// whole tree behind mounting), so post bodies render on the client either
// way.
//
// Vertical rhythm: every block owns its own margins (headings breathe more
// above than below), so authors never spacer-div between blocks.

type ChildrenProp = Readonly<{ children: React.ReactNode }>;

// Section heading. scrollMarginTop keeps it clear of the sticky top bar when
// jumped to via a fragment link, mirroring Section on the home page.
export function H2({ children }: ChildrenProp) {
  return (
    <Typography
      variant="h5"
      component="h2"
      sx={{ fontWeight: 500, mt: 5, mb: 2, scrollMarginTop: 8 }}
    >
      {children}
    </Typography>
  );
}

export function H3({ children }: ChildrenProp) {
  return (
    <Typography
      variant="h6"
      component="h3"
      sx={{ fontWeight: 500, mt: 4, mb: 1.5, scrollMarginTop: 8 }}
    >
      {children}
    </Typography>
  );
}

export function H4({ children }: ChildrenProp) {
  return (
    <Typography
      variant="subtitle1"
      component="h4"
      sx={{ fontWeight: 500, mt: 3, mb: 1 }}
    >
      {children}
    </Typography>
  );
}

// Body paragraph. The 1.8 line height is the main readability upgrade over
// the theme's default body1.
export function P({ children }: ChildrenProp) {
  return (
    <Typography variant="body1" sx={{ lineHeight: 1.8, my: 2 }}>
      {children}
    </Typography>
  );
}

// Prose hyperlink: internal hrefs route through Next.js, absolute http(s)
// URLs open in a new tab (the same rule as the Posts cards' Read button).
export function Link({
  href,
  children,
}: Readonly<{ href: string; children: React.ReactNode }>) {
  const external = href.startsWith("http");
  return (
    <MuiLink
      component={external ? "a" : NextLink}
      href={href}
      target={external ? "_blank" : undefined}
      rel={external ? "noreferrer" : undefined}
      sx={{ textUnderlineOffset: "2px" }}
    >
      {children}
    </MuiLink>
  );
}

function List({
  component,
  children,
}: Readonly<{ component: "ul" | "ol"; children: React.ReactNode }>) {
  return (
    <Box
      component={component}
      sx={{ my: 2, pl: 3, "& li::marker": { color: "text.secondary" } }}
    >
      {children}
    </Box>
  );
}

export function Ul({ children }: ChildrenProp) {
  return <List component="ul">{children}</List>;
}

export function Ol({ children }: ChildrenProp) {
  return <List component="ol">{children}</List>;
}

export function Li({ children }: ChildrenProp) {
  return (
    <Typography
      component="li"
      variant="body1"
      sx={{ lineHeight: 1.8, my: 0.5 }}
    >
      {children}
    </Typography>
  );
}

// Inline code: a subtle chip in the monospace font. For blocks use CodeBlock.
export function Code({ children }: ChildrenProp) {
  return (
    <Box
      component="code"
      sx={{
        fontFamily: "var(--font-geist-mono), monospace",
        fontSize: "0.875em",
        bgcolor: "action.hover",
        borderRadius: "6px",
        px: 0.5,
        py: 0.25,
      }}
    >
      {children}
    </Box>
  );
}

// Fenced code block: a bordered paper surface with horizontal scroll for long
// lines. Pass the source as a single template literal; indentation is
// preserved as written.
export function CodeBlock({ children }: ChildrenProp) {
  return (
    <Box
      component="pre"
      sx={{
        my: 3,
        p: 2,
        border: 1,
        borderColor: "divider",
        // Explicit px: numeric borderRadius in sx is multiplied by
        // shape.borderRadius (12px), so 2 would render as a very round 24px.
        borderRadius: "4px",
        bgcolor: "background.paper",
        overflowX: "auto",
        fontFamily: "var(--font-geist-mono), monospace",
        fontSize: "0.875rem",
        lineHeight: 1.7,
      }}
    >
      {children}
    </Box>
  );
}

// Pull quote / aside. The muted color and italic style inherit into the
// blocks inside (Typography sets no color of its own unless asked).
export function Quote({ children }: ChildrenProp) {
  return (
    <Box
      component="blockquote"
      sx={{
        my: 3,
        mx: 0,
        pl: 2,
        borderLeft: 4,
        borderColor: "divider",
        color: "text.secondary",
        fontStyle: "italic",
      }}
    >
      {children}
    </Box>
  );
}

export function Hr() {
  return <Divider sx={{ my: 4 }} />;
}
