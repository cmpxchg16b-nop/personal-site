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

All copy is placeholder text living in the translation bundles
(`src/i18n/locales/en.json`, `src/i18n/locales/zh.json`): the owner's name,
tagline, about paragraphs, post and project cards, and contact entries.
Edit those two files to make the site yours.

## Building

```sh
npm run build
```

emits the static export to `out/`, which the Go server embeds at compile time
(see `webfs.go` at the repository root).
