"use client";

import { useState } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  Avatar,
  Button,
  ButtonBase,
  Divider,
  ListItemIcon,
  Menu,
  MenuItem,
  Typography,
} from "@mui/material";
import LogoutIcon from "@mui/icons-material/Logout";
import { useProfile } from "@/hooks/useProfile";
import { useLogout } from "@/hooks/useLogout";
import { useTranslation } from "react-i18next";

// PROFILE_POLL_INTERVAL_MS is how often ProfileMenu re-fetches GET
// /api/profile. Polling keeps the top bar in sync with the session: a login
// or logout in another tab — or a JWT that expired mid-session — is picked
// up here within one interval instead of on the next full page load.
const PROFILE_POLL_INTERVAL_MS = 5000;

// avatarHue hashes the subject id to a stable hue (0–359), so each user gets
// a consistent avatar color without the server assigning one.
function avatarHue(subjectId: string): number {
  let hash = 0;
  for (let i = 0; i < subjectId.length; i++) {
    hash = (hash * 31 + subjectId.charCodeAt(i)) | 0;
  }
  return ((hash % 360) + 360) % 360;
}

// ProfileMenu is the account area at the right end of the top bar. For an
// authenticated caller it is a round avatar with the display name's first
// initial, plus the display name text when the viewport is sm (600px) or
// wider; clicking it opens a menu showing the display name and a Log Out
// item. The display name is the session's username when the JWT carries one,
// falling back to the subject id otherwise. For an unauthenticated caller
// (GET /api/profile fails, e.g. the JWT middleware's 401) it renders as a
// Login link button instead — hidden entirely on pages under /login, where a
// login affordance would be redundant. Anonymous browsing stays allowed
// everywhere; there is no forced redirect to the login page. It renders
// nothing while the profile is loading, so the bar never flashes the wrong
// affordance.
export default function ProfileMenu() {
  const { t } = useTranslation();
  const { data, isPending, isError } = useProfile(PROFILE_POLL_INTERVAL_MS);
  const logout = useLogout();
  const pathname = usePathname();
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const open = anchorEl !== null;

  if (isPending) return null;

  if (isError) {
    // A failed profile fetch means there is no (valid) session, so the
    // visitor is anonymous. Already on the login page the dialog there is
    // the login affordance, so the top bar shows nothing.
    if (pathname.startsWith("/login")) return null;
    // Tell the login page where to return after a successful login: the
    // current location, site-relative. This branch only renders client-side
    // (after the profile query has failed), so window is safe to read.
    const here =
      window.location.pathname + window.location.search + window.location.hash;
    return (
      <Button
        component={Link}
        href={`/login?redirect_if_succeed=${encodeURIComponent(here)}`}
        variant="outlined"
        size="small"
        sx={{ textTransform: "none", mr: 1 }}
      >
        {t("profile.login")}
      </Button>
    );
  }

  const subjectId = data?.subject_id;
  if (!subjectId) return null;

  // displayName prefers the human-friendly username from the JWT claims and
  // falls back to the (always present) subject id when it is unset or blank.
  const username = data?.username?.trim();
  const displayName = username ? username : subjectId;

  const closeMenu = () => setAnchorEl(null);

  const handleLogout = () => {
    closeMenu();
    logout.mutate(undefined, {
      onSuccess: () => {
        // Full-page navigation so the query cache and all other client state
        // tied to the old session are dropped with it. Home, not /login:
        // anonymous browsing is fine everywhere on the public site.
        window.location.assign("/");
      },
    });
  };

  return (
    <>
      <ButtonBase
        aria-label={t("profile.account", { name: displayName })}
        aria-controls={open ? "profile-menu" : undefined}
        aria-haspopup="true"
        aria-expanded={open ? "true" : undefined}
        onClick={(e) => setAnchorEl(e.currentTarget)}
        sx={{ borderRadius: "9999px", gap: 1, py: 0.5, pr: 1, ml: 0.5 }}
      >
        <Avatar
          sx={{
            width: 26,
            height: 26,
            fontSize: 16,
            color: "#fff",
            bgcolor: `hsl(${avatarHue(subjectId)}, 65%, 45%)`,
            border: "2px solid #fff",
            // Keeps the white ring visible against the light-scheme bar.
            boxSizing: "border-box",
          }}
        >
          {displayName.charAt(0).toUpperCase()}
        </Avatar>
        <Typography
          variant="body2"
          noWrap
          sx={{
            display: { xs: "none", sm: "block" },
            maxWidth: { sm: 140, md: 240 },
            textOverflow: "ellipsis",
            overflow: "hidden",
          }}
        >
          {displayName}
        </Typography>
      </ButtonBase>
      <Menu
        id="profile-menu"
        anchorEl={anchorEl}
        open={open}
        onClose={closeMenu}
        anchorOrigin={{ vertical: "bottom", horizontal: "right" }}
        transformOrigin={{ vertical: "top", horizontal: "right" }}
        // The top bar sits one step above theme.zIndex.modal (so it stays
        // usable over dialogs), so its menu must go one step further to not
        // slide under the bar.
        sx={{ zIndex: (theme) => theme.zIndex.modal + 2 }}
      >
        <MenuItem disabled sx={{ "&.Mui-disabled": { opacity: 1 } }}>
          {/* body1 matches the Log Out item's font size; only the color
              marks it as a non-interactive header line. */}
          <Typography
            variant="body1"
            color="text.secondary"
            sx={{ overflowWrap: "anywhere" }}
          >
            {displayName}
          </Typography>
        </MenuItem>
        <Divider />
        <MenuItem onClick={handleLogout} disabled={logout.isPending}>
          <ListItemIcon>
            <LogoutIcon fontSize="small" />
          </ListItemIcon>
          {t("profile.logout")}
        </MenuItem>
      </Menu>
    </>
  );
}
