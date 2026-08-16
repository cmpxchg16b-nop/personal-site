"use client";

import { Button, Stack } from "@mui/material";
import EmailOutlinedIcon from "@mui/icons-material/EmailOutlined";
import GitHubIcon from "@mui/icons-material/GitHub";
import PublicIcon from "@mui/icons-material/Public";
import { useTranslation } from "react-i18next";
import Section from "./Section";

// An entry's kind picks its icon; unknown kinds fall back to the generic
// globe. Kinds come from the translation bundle's contact.items list.
const KIND_ICON: Record<string, React.ReactNode> = {
  email: <EmailOutlinedIcon />,
  github: <GitHubIcon />,
  website: <PublicIcon />,
};

// The Contact section: one pill button per contact entry, opening the entry's
// URL. External links open in a new tab; mailto: and similar schemes stay in
// the current one.
export default function ContactSection() {
  const { t } = useTranslation();
  const contacts = t("contact.items", { returnObjects: true });

  return (
    <Section
      id="contact"
      title={t("contact.title")}
      subtitle={t("contact.subtitle")}
    >
      <Stack direction="row" spacing={2} sx={{ flexWrap: "wrap" }} useFlexGap>
        {contacts.map((contact) => (
          <Button
            key={contact.url}
            variant="outlined"
            startIcon={KIND_ICON[contact.kind] ?? <PublicIcon />}
            href={contact.url}
            target={contact.url.startsWith("http") ? "_blank" : undefined}
            rel="noreferrer"
          >
            {contact.label}
          </Button>
        ))}
      </Stack>
    </Section>
  );
}
