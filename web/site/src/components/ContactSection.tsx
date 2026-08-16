"use client";

import { Button, Stack, Typography } from "@mui/material";
import EmailOutlinedIcon from "@mui/icons-material/EmailOutlined";
import GitHubIcon from "@mui/icons-material/GitHub";
import PublicIcon from "@mui/icons-material/Public";
import { useTranslation } from "react-i18next";
import Section from "./Section";
import { useDynAuthorContacts } from "@/hooks/useDynBlogData";

// An entry's kind picks its icon; unknown kinds fall back to the generic
// globe. Kinds come from the backend's author-contact entries (GET
// /api/dyn/authorcontacts).
const KIND_ICON: Record<string, React.ReactNode> = {
  email: <EmailOutlinedIcon />,
  github: <GitHubIcon />,
  website: <PublicIcon />,
};

// The Contact section: one pill button per contact entry, opening the
// entry's URL. External links open in a new tab; mailto: and similar schemes
// stay in the current one. The entries come from the backend (GET
// /api/dyn/authorcontacts), re-read from serverConfig.xml on every request.
export default function ContactSection() {
  const { t } = useTranslation();
  const { data: contacts, isPending, isError } = useDynAuthorContacts();

  return (
    <Section
      id="contact"
      title={t("contact.title")}
      subtitle={isError ? t("contact.loadFailed") : t("contact.subtitle")}
    >
      {isPending ? (
        <Typography>…</Typography>
      ) : (
        !isError &&
        contacts.length > 0 && (
          <Stack
            direction="row"
            spacing={2}
            sx={{ flexWrap: "wrap" }}
            useFlexGap
          >
            {contacts.map((contact) => (
              <Button
                key={contact.id}
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
        )
      )}
    </Section>
  );
}
