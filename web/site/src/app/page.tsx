import { Box } from "@mui/material";
import HeroSection from "@/components/HeroSection";
import AboutSection from "@/components/AboutSection";
import PostsSection from "@/components/PostsSection";
import ProjectsSection from "@/components/ProjectsSection";
import ContactSection from "@/components/ContactSection";
import SiteFooter from "@/components/SiteFooter";
import CommentZone from "@/components/CommentZone";

// The home page stacks the site's sections. All copy is placeholder text from
// the translation bundles (see src/i18n/locales).
export default function Home() {
  return (
    <Box>
      <HeroSection />
      <AboutSection />
      <PostsSection />
      <ProjectsSection />
      <ContactSection />
      <CommentZone />
      <SiteFooter />
    </Box>
  );
}
