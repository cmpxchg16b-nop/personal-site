"use client";

import { useState } from "react";
import { Avatar, ButtonBase, Menu, MenuItem, Typography } from "@mui/material";
import { useProfile } from "@/hooks/useProfile";
import { useTranslation } from "react-i18next";

// PROFILE_POLL_INTERVAL_MS is how often ProfileMenu re-fetches GET
// /api/profile. Polling keeps the top bar in sync with the profile the server
// reports, so a change is picked up here within one interval instead of on
// the next full page load.
const PROFILE_POLL_INTERVAL_MS = 10000;

// avatarHue hashes the subject id to a stable hue (0–359), so each user gets
// a consistent avatar color without the server assigning one.
function avatarHue(subjectId: string): number {
  let hash = 0;
  for (let i = 0; i < subjectId.length; i++) {
    hash = (hash * 31 + subjectId.charCodeAt(i)) | 0;
  }
  return ((hash % 360) + 360) % 360;
}

// ProfileMenu is the account area at the right end of the top bar: a round
// avatar with the display name's first initial, plus the display name text
// when the viewport is sm (600px) or wider; clicking it opens a menu showing
// the display name. The display name is the profile's username when the
// server carries one, falling back to the subject id otherwise.
//
// There is no login or account system yet — GET /api/profile answers with a
// hard-coded "Visitor" identity, so that is what everyone sees. It renders
// nothing while the profile is loading (or if the fetch fails), so the bar
// never flashes the wrong affordance.
export default function ProfileMenu() {
  const { t } = useTranslation();
  const { data, isPending, isError } = useProfile(PROFILE_POLL_INTERVAL_MS);
  const [anchorEl, setAnchorEl] = useState<HTMLElement | null>(null);
  const open = anchorEl !== null;

  if (isPending || isError) return null;

  const subjectId = data?.subject_id;
  if (!subjectId) return null;

  // displayName prefers the human-friendly username from the profile and
  // falls back to the (always present) subject id when it is unset or blank.
  const username = data?.username?.trim();
  const displayName = username ? username : subjectId;

  const closeMenu = () => setAnchorEl(null);

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
          {/* Account actions (e.g. Log Out) return here once sign-in exists;
              for now the menu is just the identity header line. */}
          <Typography
            variant="body1"
            color="text.secondary"
            sx={{ overflowWrap: "anywhere" }}
          >
            {displayName}
          </Typography>
        </MenuItem>
      </Menu>
    </>
  );
}
