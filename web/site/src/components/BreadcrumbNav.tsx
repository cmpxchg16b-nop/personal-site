"use client";

import NextLink from "next/link";
import { usePathname, useSearchParams } from "next/navigation";
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
function crumbsFor(
  pathname: string,
  examSessionId: string | null,
  t: TFunction,
): Crumb[] {
  if (pathname === "/") {
    return [{ label: t("nav.home") }];
  }
  // The login page is a modal dialog outside the app's page hierarchy, so it
  // gets no breadcrumb trail at all.
  if (pathname === "/login") {
    return [];
  }
  if (pathname === "/examsession") {
    return [
      { label: t("nav.home"), href: "/" },
      // The ongoing-sessions list lives on Home; there is no dedicated exam
      // sessions page, so this level is intentionally not clickable.
      { label: t("nav.examSessions") },
      { label: examSessionId ?? "…" },
    ];
  }
  // Unknown route: fall back to Home plus the raw path.
  return [{ label: t("nav.home"), href: "/" }, { label: pathname }];
}

// BreadcrumbNav shows where the current page sits in the hierarchy (e.g.
// "Home > Exam Sessions > <exam_session_id>") so the user can jump back up
// with one click. The trail is derived from the URL, so pages need no wiring.
// Rendered inside TopBar, which owns the surrounding layout/spacing.
export default function BreadcrumbNav() {
  const { t } = useTranslation();
  const pathname = usePathname();
  const searchParams = useSearchParams();
  const crumbs = crumbsFor(pathname, searchParams.get("exam_session_id"), t);

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
          // overflowWrap keeps a long exam session id from overflowing
          // narrow viewports.
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
