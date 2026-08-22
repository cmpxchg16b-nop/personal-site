"use client";

import NextLink from "next/link";
import { usePathname } from "next/navigation";
import { Breadcrumbs, Link as MuiLink, Typography } from "@mui/material";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";

// A crumb with href renders as a link; without one it is plain text — used
// both for the current page (the last crumb) and for ancestors that have no
// dedicated page to link to yet.
type Crumb = {
  label: string;
  href?: string;
};

// crumbsFor maps the app's known routes onto their breadcrumb trails.
function crumbsFor(pathname: string, t: TFunction): Crumb[] {
  if (pathname === "/") {
    return [{ label: t("nav.home") }];
  }
  if (pathname === "/chat") {
    return [{ label: t("nav.home"), href: "/" }, { label: t("chat.title") }];
  }
  if (pathname.startsWith("/posts/")) {
    return [
      { label: t("nav.home"), href: "/" },
      // The posts list lives on Home; link straight to its anchor.
      { label: t("posts.title"), href: "/#posts" },
      // The post's title is not available here (it lives in the server
      // configuration, fetched by the page itself), so the last crumb shows
      // the post's URL slug.
      { label: pathname.slice("/posts/".length) },
    ];
  }
  // Unknown route: fall back to Home plus the raw path.
  return [{ label: t("nav.home"), href: "/" }, { label: pathname }];
}

// BreadcrumbNav shows where the current page sits in the hierarchy (e.g.
// "Home > Posts > hello-world") so the user can jump back up with one
// click. The trail is derived from the URL, so pages need no wiring.
// Rendered inside TopBar, which owns the surrounding layout/spacing.
export default function BreadcrumbNav() {
  const { t } = useTranslation();
  const pathname = usePathname();
  const crumbs = crumbsFor(pathname, t);

  // A lone segment (e.g. just "Home" on the home page) has no hierarchy to
  // navigate, so the whole bar is hidden.
  if (crumbs.length < 2) return null;

  return (
    <Breadcrumbs aria-label={t("nav.breadcrumb")}>
      {crumbs.map((crumb, i) => {
        const isLast = i === crumbs.length - 1;
        return !isLast && crumb.href ? (
          <MuiLink
            key={crumb.label}
            component={NextLink}
            href={crumb.href}
            underline="hover"
          >
            {crumb.label}
          </MuiLink>
        ) : (
          // overflowWrap keeps a long crumb (e.g. a post slug) from
          // overflowing narrow viewports.
          <Typography
            key={crumb.label}
            color={isLast ? "text.primary" : "text.secondary"}
            sx={{ overflowWrap: "anywhere" }}
          >
            {crumb.label}
          </Typography>
        );
      })}
    </Breadcrumbs>
  );
}
