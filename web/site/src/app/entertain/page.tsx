"use client";

import { Box, LinearProgress, Typography } from "@mui/material";
import LiveTvIcon from "@mui/icons-material/LiveTv";
import OndemandVideoIcon from "@mui/icons-material/OndemandVideo";
import MusicNoteIcon from "@mui/icons-material/MusicNote";
import { useTranslation } from "react-i18next";
import Section from "@/components/Section";
import MediaCard from "@/components/MediaCard";
import { useDynEntertain, type DynMedia } from "@/hooks/useDynBlogData";

// The entertain page: three media shelves — Live, Video, Music — each
// rendered as a CSS grid of media cards. The entries are server
// configuration (the <entertain/> element of serverConfig.xml, served by
// GET /api/dyn/entertain and re-read on every request server-side), so
// editing the document updates the shelves with no rebuild and no restart.

// mediaGridSx is the shelves' shared layout: a plain CSS grid of at least
// 240px-wide columns, wrapping to as many as the viewport fits.
const mediaGridSx = {
  mt: 2,
  display: "grid",
  gridTemplateColumns: "repeat(auto-fill, minmax(240px, 1fr))",
  gap: 2,
} as const;

function MediaShelf({
  entries,
  placeholderIcon,
  emptyLabel,
}: {
  entries: DynMedia[];
  placeholderIcon: React.ReactNode;
  emptyLabel: string;
}) {
  if (entries.length === 0) {
    return <Typography sx={{ mt: 2 }}>{emptyLabel}</Typography>;
  }
  return (
    <Box sx={mediaGridSx}>
      {entries.map((media) => (
        <MediaCard
          key={media.id}
          media={media}
          placeholderIcon={placeholderIcon}
        />
      ))}
    </Box>
  );
}

export default function EntertainPage() {
  const { t } = useTranslation();
  const { data: entertain, isPending, isError } = useDynEntertain();

  return (
    <Box>
      <Section
        id="live"
        title={t("entertain.live.title")}
        // No descriptive subtitle: the shelf titles are self-descriptive.
        // The slot only speaks up when the media lists failed to load.
        subtitle={isError ? t("entertain.loadFailed") : undefined}
      >
        {isPending ? (
          <LinearProgress sx={{ mt: 2 }} />
        ) : (
          <MediaShelf
            entries={entertain?.live ?? []}
            placeholderIcon={<LiveTvIcon sx={{ fontSize: 48 }} />}
            emptyLabel={t("entertain.empty")}
          />
        )}
      </Section>
      <Section
        id="video"
        title={t("entertain.video.title")}
        subtitle={isError ? t("entertain.loadFailed") : undefined}
      >
        {isPending ? (
          <LinearProgress sx={{ mt: 2 }} />
        ) : (
          <MediaShelf
            entries={entertain?.videos ?? []}
            placeholderIcon={<OndemandVideoIcon sx={{ fontSize: 48 }} />}
            emptyLabel={t("entertain.empty")}
          />
        )}
      </Section>
      <Section
        id="music"
        title={t("entertain.music.title")}
        subtitle={isError ? t("entertain.loadFailed") : undefined}
      >
        {isPending ? (
          <LinearProgress sx={{ mt: 2 }} />
        ) : (
          <MediaShelf
            entries={entertain?.music ?? []}
            placeholderIcon={<MusicNoteIcon sx={{ fontSize: 48 }} />}
            emptyLabel={t("entertain.empty")}
          />
        )}
      </Section>
    </Box>
  );
}
