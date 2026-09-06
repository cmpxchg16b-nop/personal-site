import { Box } from "@mui/material";
import SatelliteBackground from "@/components/satellite/SatelliteBackground";
import HeroSection from "@/components/HeroSection";
import LiveSection from "@/components/LiveSection";
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
      {/* Fixed three.js backdrop: Earth seen from a satellite flying a
          configurable orbit, painted behind the whole page (z-index -1).
          Trajectory, attitude, camera offset, spin and lighting are all
          props — see src/components/satellite/types.ts. */}
      <SatelliteBackground />
      <HeroSection />
      <LiveSection />
      <AboutSection />
      <PostsSection />
      <ProjectsSection />
      <ContactSection />
      <CommentZone />
      <SiteFooter />
    </Box>
  );
}
