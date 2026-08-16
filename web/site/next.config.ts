import type { NextConfig } from "next";
import { PHASE_DEVELOPMENT_SERVER } from "next/constants";

const nextConfig = (phase: string): NextConfig => {
  const config: NextConfig = {
    /* config options here */
    output: "export",
    images: { unoptimized: true },
  };

  // Dev-only proxy: forward /api/* to the Go server so the frontend's relative
  // fetch URLs work under `next dev`. In production the static export is served
  // by the Go server itself, which handles /api on the same origin.
  //
  // This must stay dev-only: rewrites are incompatible with `output: export`
  // and `next build` fails with export-no-custom-routes if they're present.
  if (phase === PHASE_DEVELOPMENT_SERVER) {
    config.rewrites = async () => [
      {
        source: "/api/:path*",
        destination: "http://localhost:8080/api/:path*",
      },
      {
        source: "/assets/:path*",
        destination: "http://localhost:8080/assets/:path*",
      },
    ];
  }

  return config;
};

export default nextConfig;
