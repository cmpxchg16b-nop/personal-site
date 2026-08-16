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
import GitHubIcon from "@mui/icons-material/GitHub";
import { useTranslation } from "react-i18next";
import Section from "./Section";
import { useDynProjects } from "@/hooks/useDynBlogData";

// The Projects section: one card per project — name, tech-stack chips, and a
// two-line clamped description — with a Source button linking to the repo.
// The entries come from the backend (GET /api/dyn/projects); the server
// re-reads them from its configuration document on every request, so editing
// serverConfig.xml updates this list without a rebuild.
export default function ProjectsSection() {
  const { t } = useTranslation();
  const { data: projects, isPending, isError } = useDynProjects();

  return (
    <Section
      id="projects"
      title={t("projects.title")}
      subtitle={isError ? t("projects.loadFailed") : t("projects.subtitle")}
    >
      {isPending ? (
        <LinearProgress sx={{ mt: 2 }} />
      ) : (
        !isError &&
        projects.length > 0 && (
          <List>
            {projects.map((project) => (
              <ListItem key={project.id} disableGutters sx={{ mb: 1 }}>
                <Card sx={{ width: "100%" }}>
                  <CardContent>
                    <Box sx={{ display: "flex", alignItems: "center", gap: 2 }}>
                      {/* minWidth: 0 lets the text column shrink so the clamp
                          can kick in instead of pushing the button off-card. */}
                      <Box sx={{ flexGrow: 1, minWidth: 0 }}>
                        <Typography variant="h6" component="div" noWrap>
                          {project.name}
                        </Typography>
                        <Box
                          sx={{
                            display: "flex",
                            flexWrap: "wrap",
                            gap: 0.5,
                            mb: 1,
                          }}
                        >
                          {project.tech.map((tech) => (
                            <Chip key={tech} label={tech} size="small" />
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
                          {project.description}
                        </Typography>
                      </Box>
                      <Button
                        variant="contained"
                        href={project.url}
                        target="_blank"
                        rel="noreferrer"
                        startIcon={<GitHubIcon />}
                      >
                        {t("projects.source")}
                      </Button>
                    </Box>
                  </CardContent>
                </Card>
              </ListItem>
            ))}
          </List>
        )
      )}
    </Section>
  );
}
