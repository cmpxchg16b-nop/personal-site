"use client";

import Link from "next/link";
import { usePathname } from "next/navigation";
import {
  Box,
  LinearProgress,
  Link as MuiLink,
  Typography,
} from "@mui/material";
import { useTranslation } from "react-i18next";
import CommentForm from "./CommentForm";
import CommentList from "./CommentList";
import { useComments, usePostComment } from "@/hooks/useComments";
import { useProfile } from "@/hooks/useProfile";

type CommentZoneProps = {
  // The channel the zone reads and appends to. Optional: when unset or
  // empty, the channel is derived from the pathname of the current location
  // as "pathname:<pathname>" (the prefix keeps the URL-encoded id a
  // well-formed single path segment — a bare "/" would not survive the
  // server's path cleaning) — so a bare <CommentZone /> at the bottom of a
  // page comments on that page. When neither is available, the zone renders
  // nothing.
  channelId?: string;
};

// CommentZone is the commenting block at the bottom of a page: the channel's
// comments oldest-first, then the form to append a new one. The list and the
// editor are extracted into CommentList/CommentItem and CommentForm; this
// component only wires the data (useComments, usePostComment) to them.
//
// Submitting appends onto the channel's last comment as currently loaded; a
// 409 (someone else commented first) refreshes the list and shows a message
// — the written text stays in the form so the commenter can retry.
//
// Reading comments is open to everyone; posting requires a session (the
// server takes the author from it), so an anonymous visitor gets a link to
// the login page instead of the form.
export default function CommentZone({ channelId }: CommentZoneProps) {
  const { t } = useTranslation();
  // usePathname tracks client-side navigations, so the zone follows soft
  // route changes. A derived channel id is "pathname:<pathname>"; the
  // prefix guarantees the id never starts with "/", which the server
  // rejects once URL-encoded.
  const pathname = usePathname();
  const resolvedChannelId =
    channelId || (pathname ? `pathname:${pathname}` : null);

  const {
    data: comments,
    isPending,
    isError,
  } = useComments(resolvedChannelId ?? "", resolvedChannelId !== null);
  const {
    mutateAsync,
    isPending: submitting,
    error,
  } = usePostComment(resolvedChannelId ?? "");
  // Shares ProfileMenu's ["profile"] cache entry, so this check costs no
  // extra request. While it is pending the editor area renders nothing, so
  // it never flashes the wrong affordance (same rule as ProfileMenu).
  const { isPending: profilePending, isError: anonymous } = useProfile();

  if (resolvedChannelId === null) {
    return null;
  }

  const handleSubmit = async (input: { content: string }) => {
    const lastCommentId = comments?.[comments.length - 1]?.id ?? "";
    try {
      await mutateAsync({ ...input, lastCommentId });
      return true;
    } catch {
      // Surfaced through the mutation's error state.
      return false;
    }
  };

  return (
    <Box component="section" sx={{ mt: 6 }}>
      <Typography variant="h4" component="h2" gutterBottom>
        {t("comments.title")}
      </Typography>
      {isPending ? (
        <LinearProgress sx={{ mt: 2 }} />
      ) : isError ? (
        <Typography color="error">{t("comments.loadFailed")}</Typography>
      ) : (
        <>
          {comments.length === 0 ? (
            <Typography color="textSecondary">{t("comments.empty")}</Typography>
          ) : (
            <CommentList comments={comments} />
          )}
          {profilePending ? null : anonymous ? (
            <Typography sx={{ mt: 3 }}>
              {/* Carry the current location so the login flow returns here
                  after a successful sign-in. This branch only renders
                  client-side (after the profile query has failed), so
                  window is safe to read. */}
              <MuiLink
                component={Link}
                href={`/login?redirect_if_succeed=${encodeURIComponent(
                  window.location.pathname +
                    window.location.search +
                    window.location.hash,
                )}`}
                underline="hover"
              >
                {t("comments.loginRequired")}
              </MuiLink>
            </Typography>
          ) : (
            <CommentForm
              submitting={submitting}
              error={error}
              onSubmit={handleSubmit}
            />
          )}
        </>
      )}
    </Box>
  );
}
