"use client";

import { useSearchParams } from "next/navigation";
import { Box, LinearProgress, Typography } from "@mui/material";
import { useTranslation } from "react-i18next";
import CommentZone from "@/components/CommentZone";
import HLSPlayer from "@/components/HLSPlayer";
import WhepVideo from "@/components/WhepVideo";
import {
  useDynEntertain,
  useDynPlayables,
  type DynEntertain,
  type DynMedia,
  type DynPlayable,
} from "@/hooks/useDynBlogData";

// The play page: the site's own player page, reached from media cards that
// carry no href of their own (MediaCard links them to /play?mediaId=<id>).
// It resolves the media's playable sources through the getPlayableByMediaId
// API (useDynPlayables), picks one per the selection policy below, and hands
// its URL to the matching player. Everything is client-side rendered: the
// data is query-param- and API-driven, so static prerendering has nothing to
// work with (useSearchParams needs no Suspense boundary of its own here —
// the root layout already wraps children in one).

// pickPlayable chooses the one source the page actually plays: the first
// WHEP playable when the media offers one (WebRTC's sub-second latency is
// the better live experience), otherwise the first HLS one (the
// compatibility fallback). null means the media is unplayable here.
function pickPlayable(playables: DynPlayable[]): DynPlayable | null {
  return (
    playables.find((p) => p.type === "whep") ??
    playables.find((p) => p.type === "hls") ??
    null
  );
}

// findMedia locates one media entry by id across the entertain page's three
// shelves — the page's header comes from it. An unknown id finds nothing.
function findMedia(
  entertain: DynEntertain | undefined,
  mediaId: string,
): DynMedia | null {
  if (entertain === undefined || mediaId === "") return null;
  for (const shelf of [entertain.live, entertain.videos, entertain.music]) {
    const found = shelf.find((m) => m.id === mediaId);
    if (found !== undefined) return found;
  }
  return null;
}

// PlayerSurface is the 16:9 frame the page renders in place of a player when
// the media cannot be played, framed exactly like the players' own surfaces
// so the page's shape does not change with the outcome.
function PlayerSurface({ message }: { message: string }) {
  return (
    <Box
      sx={{
        maxWidth: 720,
        aspectRatio: "16 / 9",
        bgcolor: "black",
        border: 1,
        borderColor: "divider",
        borderRadius: 1,
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
      }}
    >
      <Typography
        variant="body2"
        sx={{ color: "common.white", textAlign: "center", px: 2 }}
      >
        {message}
      </Typography>
    </Box>
  );
}

export default function PlayPage() {
  const { t } = useTranslation();
  // The media to play, carried by the mediaId query parameter — the page's
  // whole input.
  const mediaId = useSearchParams().get("mediaId") ?? "";
  const {
    data: playables,
    isPending,
    isError,
  } = useDynPlayables(mediaId, mediaId !== "");
  // The header comes from the entertain shelves: the media id addresses the
  // same card entry the visitor clicked through from.
  const { data: entertain } = useDynEntertain();
  const media = findMedia(entertain, mediaId);

  let player: React.ReactNode;
  if (mediaId !== "" && isPending) {
    player = <LinearProgress />;
  } else if (isError) {
    player = <PlayerSurface message={t("play.loadFailed")} />;
  } else {
    const playable = playables !== undefined ? pickPlayable(playables) : null;
    if (playable?.type === "whep") {
      player = <WhepVideo url={playable.url} />;
    } else if (playable?.type === "hls") {
      player = <HLSPlayer url={playable.url} />;
    } else {
      // No playable could be determined — no mediaId given, an unknown id,
      // or a media without sources. The comment zone below still renders.
      player = <PlayerSurface message={t("play.unplayable")} />;
    }
  }

  return (
    <Box>
      {media !== null && (
        <>
          <Typography variant="h4" component="h1">
            {media.displayName}
          </Typography>
          <Typography color="text.secondary" sx={{ mt: 1 }}>
            {media.description}
          </Typography>
        </>
      )}
      <Box sx={{ mt: 2 }}>{player}</Box>
      {/* The comment zone stays even when the media is unplayable — an
          unplayable media is still something to talk about. The channel is
          explicit (media:<mediaId>): /play shares its pathname across every
          media, so CommentZone's pathname-derived default would collapse all
          media into one thread. Only a page without a mediaId at all has no
          channel and no zone. */}
      {mediaId !== "" && <CommentZone channelId={`media:${mediaId}`} />}
    </Box>
  );
}
