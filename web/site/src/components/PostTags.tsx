"use client";

import { Box, Chip } from "@mui/material";
import { useTranslation } from "react-i18next";
import { localeTagFor } from "@/i18n";

type PostTagsProps = {
  // ISO date strings (e.g. "2026-03-01"); both optional. Same rule as the
  // post cards: when lastModified is present and differs from creation,
  // only an outlined "Updated {date}" chip shows — the creation date adds
  // noise. With no dates at all, only the tag chips render.
  creation?: string;
  lastModified?: string;
  // The post's tags, one chip each; defaults to none.
  tags?: string[];
};

// PostTags renders the post's date and tag chips: one date chip (the
// localized update date for edited posts, the creation date otherwise),
// then one chip per tag.
export default function PostTags({
  creation,
  lastModified,
  tags = [],
}: PostTagsProps) {
  const { t, i18n } = useTranslation();
  const dateFmt = new Intl.DateTimeFormat(localeTagFor(i18n.language), {
    dateStyle: "medium",
  });
  const updated = lastModified !== undefined && lastModified !== creation;

  return (
    <Box sx={{ display: "flex", flexWrap: "wrap", gap: 0.5, mb: 2 }}>
      {updated ? (
        <Chip
          label={t("posts.updated", {
            date: dateFmt.format(new Date(lastModified!)),
          })}
          size="small"
          variant="outlined"
        />
      ) : creation !== undefined ? (
        <Chip label={dateFmt.format(new Date(creation))} size="small" />
      ) : null}
      {tags.map((tag) => (
        <Chip key={tag} label={tag} size="small" />
      ))}
    </Box>
  );
}
