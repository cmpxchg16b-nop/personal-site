"use client";

import { useState } from "react";
import NextLink from "next/link";
import { usePathname } from "next/navigation";
import {
  Drawer,
  IconButton,
  List,
  ListItemButton,
  ListItemIcon,
  ListItemText,
  Tooltip,
} from "@mui/material";
import MenuIcon from "@mui/icons-material/Menu";
import HomeIcon from "@mui/icons-material/Home";
import MusicNoteIcon from "@mui/icons-material/MusicNote";
import ChatBubbleIcon from "@mui/icons-material/ChatBubble";
import LinkIcon from "@mui/icons-material/Link";
import { useTranslation } from "react-i18next";
import { useDynMenu } from "@/hooks/useDynBlogData";

// MENU_ICON maps a menu entry's iconClassName — a string in the server
// configuration's <menuEntry/> elements — onto an imported icon component.
// An unknown class name falls back to a generic link icon.
const MENU_ICON: Record<string, React.ReactNode> = {
  home: <HomeIcon />,
  musicNote: <MusicNoteIcon />,
  chatBubble: <ChatBubbleIcon />,
};
const MENU_ICON_FALLBACK = <LinkIcon />;

// menuEntryHref derives an entry's route from its name: the special name
// "home" is the site root, every other name maps to "/<name>".
function menuEntryHref(name: string): string {
  return name === "home" ? "/" : `/${name}`;
}

// NavDrawer is the site menu at the left end of the top bar: a hamburger
// button that slides a navigation drawer in from the left edge. The drawer
// closes the MUI way — clicking the backdrop (the shade over the rest of
// the page), pressing Escape, or picking an entry. The entries are server
// configuration (the <menu/> element of serverConfig.xml, served by
// GET /api/dyn/menu), so the drawer's contents change without a rebuild;
// the current page's entry is marked selected.
export default function NavDrawer() {
  const { t, i18n } = useTranslation();
  const pathname = usePathname();
  const [open, setOpen] = useState(false);
  const { data: menu } = useDynMenu();

  const closeDrawer = () => setOpen(false);

  return (
    <>
      <Tooltip title={t("nav.menu")}>
        <IconButton
          aria-label={t("nav.menu")}
          aria-haspopup="true"
          aria-expanded={open ? "true" : undefined}
          onClick={() => setOpen(true)}
          sx={{ mr: 1 }}
        >
          <MenuIcon />
        </IconButton>
      </Tooltip>
      <Drawer
        anchor="left"
        open={open}
        onClose={closeDrawer}
        // The top bar sits one step above theme.zIndex.modal (so it stays
        // usable over dialogs), so the drawer and its backdrop must go one
        // step further to cover the bar as well — otherwise the shade
        // would stop at the bar and clicking there wouldn't retract it.
        sx={{ zIndex: (theme) => theme.zIndex.modal + 2 }}
      >
        <List component="nav" aria-label={t("nav.menu")} sx={{ width: 240 }}>
          {(menu ?? []).map((entry) => {
            const href = menuEntryHref(entry.name);
            // The configured per-language caption wins; the displayName
            // attribute is the fallback for languages the entry doesn't
            // cover.
            const displayName =
              entry.i18nDisplayNames?.[i18n.language] ?? entry.displayName;
            return (
              <ListItemButton
                key={entry.id}
                component={NextLink}
                href={href}
                selected={pathname === href}
                onClick={closeDrawer}
              >
                <ListItemIcon>
                  {MENU_ICON[entry.iconClassName] ?? MENU_ICON_FALLBACK}
                </ListItemIcon>
                <ListItemText
                  primary={displayName}
                  secondary={entry.description || undefined}
                />
              </ListItemButton>
            );
          })}
        </List>
      </Drawer>
    </>
  );
}
