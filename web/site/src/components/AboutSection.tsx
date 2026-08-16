"use client";

import { Typography } from "@mui/material";
import { useTranslation } from "react-i18next";
import Section from "./Section";

// The About section: a few paragraphs of bio. The paragraphs live in the
// translation bundle as a list of placeholder strings.
export default function AboutSection() {
  const { t } = useTranslation();
  const paragraphs = t("about.paragraphs", { returnObjects: true });
  return (
    <Section id="about" title={t("about.title")}>
      {paragraphs.map((paragraph, i) => (
        <Typography
          key={i}
          sx={{ maxWidth: 720, mb: i < paragraphs.length - 1 ? 2 : 0 }}
        >
          {paragraph}
        </Typography>
      ))}
    </Section>
  );
}
