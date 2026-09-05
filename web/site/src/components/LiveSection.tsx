"use client";

import { Box } from "@mui/material";
import { useTranslation } from "react-i18next";
import Section from "./Section";
import WhepVideo from "./WhepVideo";
import { useHomeLiveWHEPURL } from "@/hooks/useHomeLiveWHEPURL";

// The Live section: the home page's window onto the owner's live stream,
// read over WHEP (see src/api/whep.ts) — placed right under the hero, where
// the site's most immediate content belongs. Like every other piece of site
// content, the stream's WHEP endpoint is server configuration: the
// <homeLiveWHEPURL/> element of serverConfig.xml, served by
// GET /api/homeLiveWHEPURL (pkg/api/homelive). A document without the
// element simply has no Live section. Independent of the chat subsystem's
// peer-to-peer WebRTC: different endpoint, different protocol, no shared
// code.
export default function LiveSection() {
  const { t } = useTranslation();
  const { data: whepURL, isPending, isError } = useHomeLiveWHEPURL();

  // While the endpoint is unknown, and when the document configures none,
  // the section stays out of the page entirely: a deployment without a live
  // stream shows no trace of the feature, and one with it never flashes an
  // empty frame while the URL loads.
  if (isPending) return null;
  if (!isError && whepURL === "") return null;

  return (
    <Section
      id="live"
      title={t("live.title")}
      subtitle={isError ? t("live.loadFailed") : t("live.subtitle")}
    >
      {!isError && (
        <Box sx={{ mt: 2 }}>
          <WhepVideo url={whepURL} />
        </Box>
      )}
    </Section>
  );
}
