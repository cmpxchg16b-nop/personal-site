# personal-site web frontend

The Next.js frontend of the personal site. It is a purely client-rendered
static export (`output: "export"`) with light/dark themes, bilingual copy
(English/中文 via i18next), and no server-side data of its own.

## Developing

```sh
npm ci
npm run dev
```

Open <http://localhost:3000>. During development, requests to `/api/*` are
proxied to the Go backend at <http://localhost:8080> (see `next.config.ts`),
so run `go run ./cmd/server` from the repository root alongside it.

## Site content

Static copy lives in the translation bundles (`src/i18n/locales/en.json`,
`src/i18n/locales/zh.json`): the owner's name, tagline, and about
paragraphs. The posts, projects, and contact sections are dynamic — they
fetch `GET /api/dyn/posts`, `GET /api/dyn/projects`, and `GET
/api/dyn/authorcontacts` from the Go backend, which re-reads the
`<dynBlogData/>` section of `serverConfig.xml` on every request.

The home page's Live section reads the owner's live stream over WHEP
(WebRTC HTTP Egress Protocol): its endpoint is runtime configuration
too — the `<homeLiveWHEPURL/>` element of `serverConfig.xml`, fetched
from `GET /api/homeLiveWHEPURL` (see `src/hooks/useHomeLiveWHEPURL`); a
document without the element shows no Live section at all. The WHEP
client (`src/api/whep.ts`) is independent of the chat subsystem's
peer-to-peer WebRTC.

## Building

```sh
npm run build
```

emits the static export to `out/`, which the Go server embeds at compile time
(see `webfs.go` at the repository root).
