import type { Metadata } from "next";
import { Geist, Geist_Mono } from "next/font/google";
import { Box } from "@mui/material";
import InitColorSchemeScript from "@mui/material/InitColorSchemeScript";
import TopBar from "@/components/TopBar";
import { Providers } from "./providers";
import { Suspense } from "react";
import "./globals.css";

const geistSans = Geist({
  variable: "--font-geist-sans",
  subsets: ["latin"],
});

const geistMono = Geist_Mono({
  variable: "--font-geist-mono",
  subsets: ["latin"],
});

export const metadata: Metadata = {
  // No <title> here on purpose. The active language is known only in the
  // browser, so the title is set client-side by I18nProvider via
  // document.title. A server-rendered title would be owned by React's head
  // hydration, which re-asserts it after mount and stomps the localized
  // value. With no SSR title, the client write sticks; the tab shows the
  // URL only until the app mounts.
  description: "My Name's personal site — posts, projects, and contact.",
  icons: {
    // Theme-aware favicons. logo-light.png is the black artwork (for light
    // browser chrome), logo-dark.png the white artwork (for dark chrome).
    // The tab strip tracks the OS scheme — not the in-app toggle — so
    // prefers-color-scheme is the right switch here. The generated
    // favicon.ico (black on white) remains as the fallback for browsers
    // that ignore the media attribute.
    icon: [
      {
        url: "/logo-light.png",
        type: "image/png",
        media: "(prefers-color-scheme: light)",
      },
      {
        url: "/logo-dark.png",
        type: "image/png",
        media: "(prefers-color-scheme: dark)",
      },
    ],
  },
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    // suppressHydrationWarning: InitColorSchemeScript sets
    // data-mui-color-scheme on <html> before hydration, which React would
    // otherwise flag as a server/client attribute mismatch.
    <html
      lang="en"
      className={`${geistSans.variable} ${geistMono.variable}`}
      suppressHydrationWarning
    >
      <body>
        {/* Applies the persisted lightness preference (localStorage
            "mui-mode", default "dark") before first paint so the page
            never flashes the wrong scheme. */}
        <InitColorSchemeScript defaultMode="dark" />
        <Providers>
          <TopBar />
          {/* Layout-level gutters so page content never touches the viewport
              edges. Responsive padding: 16px on phones, 24px on sm, 32px on
              md and up. flexGrow lets the main region fill the flex-column
              body so short pages don't collapse. */}
          <Box
            component="main"
            sx={{ p: { xs: 2, sm: 3, md: 4 }, flexGrow: 1 }}
          >
            <Suspense fallback={<div>Loading...</div>}>{children}</Suspense>
          </Box>
        </Providers>
      </body>
    </html>
  );
}
