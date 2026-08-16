# personal-site

My personal site and blog, built with simplicity and brevity in mind.

## What it is

- **A purely client-rendered web app.** The frontend in `web/site/` is a
  Next.js static export: hero, about, posts, projects, and contact sections,
  all with placeholder copy ("My Name", example descriptions, example
  contacts) ready to be replaced.
- **Dark mode.** Light and dark themes that follow your system setting, or
  flip between them yourself with the toggle in the top bar.
- **Bilingual.** English and 中文 out of the box, with a language switcher in
  the top bar; all site copy lives in the two translation bundles.
- **A small Go backend.** One self-contained binary that embeds and serves
  the static export. It already answers a few API endpoints — `GET
/api/profile` (currently a hard-coded "Visitor" identity, until a login and
  account system exists), `GET /api/contact`, and `GET /api/healthz` — as the
  seed for future dynamic features.

## Layout

- `cmd/server/` — the Go server entrypoint (`main.go`).
- `pkg/` — Go packages kept from the project's previous incarnation (HTTP
  logging middleware, session/auth helpers, and more). They are not wired
  into `main.go` yet, but are available for future dynamic features.
- `web/site/` — the Next.js frontend (see its README).
- `webfs.go` — embeds the frontend's static export (`web/site/out`) into the
  Go binary.

## Running it

Build the frontend, then run the server:

```sh
cd web/site && npm ci && npm run build && cd ../..
go run ./cmd/server
```

Then open <http://localhost:8080>.

For frontend development with hot reload, run the two side by side:

```sh
cd web/site && npm run dev   # http://localhost:3000, /api proxied to :8080
go run ./cmd/server          # in another terminal, from the repo root
```

## Container image

```sh
cd web/site && npm ci && npm run build && cd ../..
docker build -t personal-site .
docker run --rm -p 8080:8080 personal-site
```

## Making it yours

- Site copy (name, tagline, about, posts, projects, contacts):
  `web/site/src/i18n/locales/en.json` and `zh.json`.
- The hard-coded visitor identity and contact entries served by the API:
  `cmd/server/main.go`.
- The favicons: `web/site/public/logo-light.png` and `logo-dark.png`.
