"use client";

import {
  Box,
  Button,
  Card,
  CardContent,
  Chip,
  List,
  ListItem,
  Typography,
} from "@mui/material";
import { useTranslation } from "react-i18next";
import Section from "./Section";
import { localeTagFor } from "@/i18n";

// The Posts section: the latest writing, one card per entry — title, date and
// tag chips, and a two-line clamped excerpt. Entries are placeholders from
// the translation bundle; wire the Read buttons to real post pages once they
// exist.
export default function PostsSection() {
  const { t, i18n } = useTranslation();
  const posts = t("posts.items", { returnObjects: true });
  const dateFmt = new Intl.DateTimeFormat(localeTagFor(i18n.language), {
    dateStyle: "medium",
  });

  return (
    <Section id="posts" title={t("posts.title")} subtitle={t("posts.subtitle")}>
      <List>
        {posts.map((post) => (
          <ListItem key={post.title} disableGutters sx={{ mb: 1 }}>
            <Card sx={{ width: "100%" }}>
              <CardContent>
                <Box sx={{ display: "flex", alignItems: "center", gap: 2 }}>
                  {/* minWidth: 0 lets the text column shrink so the clamp can
                      kick in instead of pushing the button off-card. */}
                  <Box sx={{ flexGrow: 1, minWidth: 0 }}>
                    <Typography variant="h6" component="div" noWrap>
                      {post.title}
                    </Typography>
                    <Box
                      sx={{ display: "flex", flexWrap: "wrap", gap: 0.5, mb: 1 }}
                    >
                      <Chip
                        label={dateFmt.format(new Date(post.date))}
                        size="small"
                      />
                      {post.tags.map((tag) => (
                        <Chip key={tag} label={tag} size="small" />
                      ))}
                    </Box>
                    <Typography
                      variant="body2"
                      color="textSecondary"
                      sx={{
                        display: "-webkit-box",
                        WebkitLineClamp: 2,
                        WebkitBoxOrient: "vertical",
                        overflow: "hidden",
                      }}
                    >
                      {post.excerpt}
                    </Typography>
                  </Box>
                  <Button variant="contained" href="#">
                    {t("posts.read")}
                  </Button>
                </Box>
              </CardContent>
            </Card>
          </ListItem>
        ))}
      </List>
    </Section>
  );
}
