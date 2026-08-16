import { Box } from "@mui/material";

// Layout for everything under /posts: a centered reading column inside the
// root layout's padded <main>. The 768px measure keeps prose lines at a
// comfortable length while leaving room for wide code blocks; mx: "auto"
// centers the column once the viewport outgrows it. Horizontal padding stays
// with the root layout and TopBar so the column's edges line up with the
// bar's content.
//
// The vertical padding adds to <main>'s own: a little air between the top
// bar and the post header, and a more generous bottom margin so the article
// doesn't end flush against the viewport edge (post pages have no footer).
export default function PostsLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <Box
      sx={{
        maxWidth: 768,
        mx: "auto",
        pt: { xs: 2, sm: 4 },
        pb: { xs: 6, sm: 8 },
      }}
    >
      {children}
    </Box>
  );
}
