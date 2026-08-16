"use client";

import {
  Box,
  Button,
  Card,
  CardContent,
  Chip,
  LinearProgress,
  List,
  ListItem,
  Typography,
} from "@mui/material";
import { useTranslation } from "react-i18next";
import Section from "./Section";
import { localeTagFor } from "@/i18n";
import { useDynPosts } from "@/hooks/useDynBlogData";

// The Posts section: the latest writing, one card per post — title, date and
// tag chips, and a two-line clamped excerpt — with a Read button navigating
// to the post's href (external URLs open in a new tab). The entries come
// from the backend (GET /api/dyn/posts); the server re-reads them from its
// configuration document on every request, so editing serverConfig.xml
// updates this list without a rebuild.
export default function PostsSection() {
  const { t, i18n } = useTranslation();
  const { data: posts, isPending, isError } = useDynPosts();
  const dateFmt = new Intl.DateTimeFormat(localeTagFor(i18n.language), {
    dateStyle: "medium",
  });

  return (
    <Section
      id="posts"
      title={t("posts.title")}
      subtitle={isError ? t("posts.loadFailed") : t("posts.subtitle")}
    >
      {isPending ? (
        <LinearProgress sx={{ mt: 2 }} />
      ) : (
        !isError &&
        posts.length > 0 && (
          <List>
            {posts.map((post) => {
              const updated =
                post.lastModified !== undefined &&
                post.lastModified !== post.creation;
              return (
                <ListItem key={post.id} disableGutters sx={{ mb: 1 }}>
                  <Card sx={{ width: "100%" }}>
                    <CardContent>
                      <Box
                        sx={{ display: "flex", alignItems: "center", gap: 2 }}
                      >
                        {/* minWidth: 0 lets the text column shrink so the clamp
                            can kick in instead of pushing the button off-card. */}
                        <Box sx={{ flexGrow: 1, minWidth: 0 }}>
                          <Typography variant="h6" component="div" noWrap>
                            {post.title}
                          </Typography>
                          <Box
                            sx={{
                              display: "flex",
                              flexWrap: "wrap",
                              gap: 0.5,
                              mb: 1,
                            }}
                          >
                            {updated ? (
                              // An edited post shows only when it was last
                              // updated; the creation date adds noise.
                              <Chip
                                label={t("posts.updated", {
                                  date: dateFmt.format(
                                    new Date(post.lastModified!),
                                  ),
                                })}
                                size="small"
                                variant="outlined"
                              />
                            ) : (
                              <Chip
                                label={dateFmt.format(new Date(post.creation))}
                                size="small"
                              />
                            )}
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
                            {post.description}
                          </Typography>
                        </Box>
                        <Button
                          variant="contained"
                          href={post.href}
                          target={
                            post.href.startsWith("http") ? "_blank" : undefined
                          }
                          rel="noreferrer"
                        >
                          {t("posts.read")}
                        </Button>
                      </Box>
                    </CardContent>
                  </Card>
                </ListItem>
              );
            })}
          </List>
        )
      )}
    </Section>
  );
}
