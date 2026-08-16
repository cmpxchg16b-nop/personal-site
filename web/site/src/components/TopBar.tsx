"use client";

import { Suspense } from "react";
import { AppBar, Box, Toolbar } from "@mui/material";
import BreadcrumbNav from "./BreadcrumbNav";
import ColorModeToggle from "./ColorModeToggle";
import LanguageSwitcher from "./LanguageSwitcher";
import ProfileMenu from "./ProfileMenu";

// TopBar groups the breadcrumb trail (left) and the color-mode toggle
// (right) into one sticky bar above the page content. The bar always
// renders — even where BreadcrumbNav hides itself (single-level pages and
// the login page) — so the toggle stays reachable everywhere.
export default function TopBar() {
  return (
    <AppBar
      position="sticky"
      elevation={0}
      sx={{
        // Quiet chrome: card surface separated by a hairline, instead of the
        // default primary-colored bar with a shadow.
        bgcolor: "background.paper",
        color: "text.primary",
        borderBottom: 1,
        borderColor: "divider",
        // One step above theme.zIndex.modal so the toggle stays clickable
        // over modal dialogs and their backdrops (e.g. the login page's
        // always-open Dialog) — the same guarantee it had as a floating
        // button.
        zIndex: (theme) => theme.zIndex.modal + 1,
      }}
    >
      {/* disableGutters + matching px aligns the bar's content with the main
          region's responsive padding (see app/layout.tsx). */}
      <Toolbar
        variant="dense"
        disableGutters
        sx={{ px: { xs: 2, sm: 3, md: 4 } }}
      >
        {/* useSearchParams inside BreadcrumbNav needs its own Suspense
            boundary so statically prerendered routes don't bail out of
            SSG. */}
        <Suspense>
          <BreadcrumbNav />
        </Suspense>
        {/* Spacer pins the toggle to the far right even when the breadcrumb
            trail is hidden. */}
        <Box sx={{ flexGrow: 1 }} />
        <ProfileMenu />
        <LanguageSwitcher />
        <ColorModeToggle />
      </Toolbar>
    </AppBar>
  );
}
