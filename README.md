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
  the static export, and answers the site's API endpoints: `GET
/api/profile` (the caller's identity — currently the hard-coded visitor
  session supplied by `StaticVisitorSessionManager`, until a login and
  account system exists), `GET /api/healthz`, and the dynamic blog data
  under `/api/dyn/` (see below).

## Dynamic blog data

The lists a blog-style site changes often — post metadata, projects, author
contacts — are served by the Go backend from the global configuration
document, so they can be edited without rebuilding the frontend:

- **Authored in XML.** The `<dynBlogData/>` section of `serverConfig.xml`
  (validated against `serverConfig.xsd`) holds zero or more of each entry
  kind: `<postMetadata/>` (`id`, `href`, `title`, `description`, `creation`,
  optional `lastModified` — all dates ISO `YYYY-MM-DD` — and optional
  comma-separated `tags`), `<project/>` (`id`, `name`, `description`, `url`,
  optional comma-separated `tech`), and `<authorContact/>` (`id`, `kind`,
  `label`, `url`). Every entry carries a unique `id`.
- **Served under `/api/dyn/`.** `DynamicBlogDataHandler` in `pkg/api/dyn`
  routes the subtree internally: `GET /api/dyn/posts`, `GET
/api/dyn/projects`, and `GET /api/dyn/authorcontacts`, each returning a
  JSON array.
- **Read on the fly.** The handler asks a `DynBlogDataProvider`
  (`pkg/models/dyn`) for the data on every request. The shipped
  implementation, `FSBasedDynBlogData`, keeps only the configuration file's
  path in memory and re-reads and re-parses the document per request — edits
  to `serverConfig.xml` apply without a server restart.
- **Wired unconditionally.** `main.go` mounts the handler at `/api/dyn/`
  even without `--config-xml`; the endpoints then serve empty lists.

The frontend's Posts, Projects, and Contact sections fetch these endpoints
at runtime, so editing `serverConfig.xml` updates the rendered page with no
frontend rebuild and no server restart.

## Layout

- `cmd/server/` — the Go server entrypoint (`main.go`): static export,
  `/api/profile`, `/api/healthz`, and the `/api/dyn/` mount.
- `pkg/` — Go packages kept from the project's previous incarnation (HTTP
  logging middleware, session/auth helpers, and more), plus the ones added
  for this site. They are mostly not wired into `main.go` yet, but are
  available for future dynamic features. Notable:
  - `pkg/models/dyn/` — `DynBlogData`, the `DynBlogDataProvider` interface,
    and `FSBasedDynBlogData`.
  - `pkg/api/dyn/` — `DynamicBlogDataHandler` and `HealthzHandler`.
  - `pkg/api/profile/` — `ProfileHandler`, wired at `GET /api/profile`.
  - `pkg/session/` — the session model; hosts `StaticVisitorSessionManager`,
    the stand-in identity provider until sign-in exists.
  - `pkg/models/serverconfig/` — the parser for `serverConfig.xml`.
- `web/site/` — the Next.js frontend (see its README).
- `webfs.go` — embeds the frontend's static export (`web/site/out`) into the
  Go binary.
- `serverConfig.xml` / `serverConfig.xsd` / `serverConfig.xml.example` — the
  global configuration document, its schema, and a sample. The document also
  still carries sections consumed by the not-yet-wired legacy packages
  (`oidcLoginOptions`, `smtpServer`, `tlsCertKeyStore`, `loginOptions`,
  `allowedOrigin`).

## Running it

Build the frontend, then run the server:

```sh
cd web/site && npm ci && npm run build && cd ../..
go run ./cmd/server --config-xml=serverConfig.xml
```

Then open <http://localhost:8080>. `--config-xml` is optional; without it the
`/api/dyn/` endpoints serve empty lists.

For frontend development with hot reload, run the two side by side:

```sh
cd web/site && npm run dev   # http://localhost:3000, /api proxied to :8080
go run ./cmd/server --config-xml=serverConfig.xml   # in another terminal
```

## Container image

```sh
cd web/site && npm ci && npm run build && cd ../..
docker build -t personal-site .
docker run --rm -p 8080:8080 personal-site
```

The image defines a `HEALTHCHECK` probing `GET /api/healthz`. Because the
runtime stage is `FROM scratch` (no shell, no curl), the probe is the server
binary itself: `--healthz-probe` GETs the endpoint over the loopback
interface and exits non-zero on failure.

Mount a configuration document to serve your dynamic blog data from the
container, e.g. `-v "$PWD/serverConfig.xml:/app/serverConfig.xml"` and
`--config-xml=serverConfig.xml` appended to the entrypoint arguments.

## Making it yours

- Site copy (name, tagline, about paragraphs):
  `web/site/src/i18n/locales/en.json` and `zh.json`.
- Posts, projects, and author contacts: the `<dynBlogData/>` section of
  `serverConfig.xml`, served live under `/api/dyn/`.
- The hard-coded visitor identity: `pkg/session`
  (`StaticVisitorSessionManager`).
- The favicons: `web/site/public/logo-light.png` and `logo-dark.png`.

## Todos

1. Commenting subsystem
2. Icons (for dark and bright variant)
3. Math rendering (example MathML post)
