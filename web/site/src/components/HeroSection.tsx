"use client";

import { Box, Button, Stack, Typography } from "@mui/material";
import { useTranslation } from "react-i18next";

// HeroSection is the top of the home page: the owner's name, a tagline, a
// short introduction, and call-to-action buttons jumping to the Posts and
// Contact sections below. All copy lives in the translation bundles and is
// placeholder text.
export default function HeroSection() {
  const { t } = useTranslation();
  return (
    <Box component="section" sx={{ py: { xs: 4, sm: 6, md: 8 } }}>
      <Typography variant="h3" component="h1" gutterBottom>
        {t("hero.greeting", { name: t("hero.name") })}
      </Typography>
      <Typography variant="h5" color="text.secondary" gutterBottom>
        {t("hero.tagline")}
      </Typography>
      <Typography variant="body1" sx={{ maxWidth: 640, mt: 2 }}>
        {t("hero.intro")}
      </Typography>
      <Stack direction="row" spacing={2} sx={{ mt: 3, flexWrap: "wrap" }} useFlexGap>
        <Button variant="contained" size="large" href="#posts">
          {t("hero.ctaPosts")}
        </Button>
        <Button variant="outlined" size="large" href="#contact">
          {t("hero.ctaContact")}
        </Button>
      </Stack>
    </Box>
  );
}
