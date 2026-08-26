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
/api/profile` (the caller's identity, from its JWT session), `GET
/api/healthz`, the dynamic blog data under `/api/dyn/` (see below), and
  short-link redirects under `/links/` (see below).

## Sign-in and sessions

The backend carries a JWT-based login system: a login endpoint issues a
signed session token into an HttpOnly cookie, and a whitelist middleware
(`pkg/auth`) requires a valid token for every `/api/` path except the
public ones (`/api/login/...`, `/api/logout`, `/api/healthz`, `/api/dyn/`,
and `GET /api/comments/` — reads are open, appends need a session).

- **Visitor login.** `GET /api/login/visitor` (`pkg/api/login/visitor`)
  signs an anonymous `visitor:`-prefixed session, paced by a shared ticket
  generator so registrations are rate-limited.
- **GitHub OAuth.** `/api/login/oauth2/github/` (`start` + `auth`) is wired
  when the configuration document carries a `<githubOAuthLogin/>` element
  with a non-empty `clientId`; the flow's `state` nonce is signed with the
  same key as the session tokens.
- **Generic OIDC.** Each `<oidcLoginOption/>` of the server configuration
  document mounts a provider at `/api/login/oidc/{providerName}/`
  (`pkg/api/login/oidc/general`). A relative `redirectURL` is resolved
  against the request's origin when it matches an `<allowedOrigin/>`
  entry.
- **Login options.** `GET /api/login/loginoptions`
  (`pkg/api/loginoptions`) serves the `<loginOptions/>` section of the
  configuration document as JSON, so the login page can render its IdP
  buttons; `POST /api/logout` (`pkg/api/logout`) clears the session
  cookies.

The JWT secret is required at startup: set the `JWT_SECRET` environment
variable (or point `--jwt-auth-secret-from-file` at a file, or change the
variable name with `--jwt-auth-secret-from-env`). Conventional `.env.local`
/ `.env` files are loaded before flag parsing, so a gitignored `.env.local`
is the place for local secrets.

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
/api/dyn/posts/{id}` (a single post's metadata, 404 when the id is
  unknown — post pages query it instead of downloading the whole list),
  `GET /api/dyn/projects`, and `GET /api/dyn/authorcontacts`. The list
  endpoints return JSON arrays.
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

## Short links

Stable short paths that redirect to longer or changing destinations —
meant for sharing, so they are served without the `/api` prefix:

- **Authored in XML.** `<shortlink/>` entries of the `<dynBlogData/>`
  section of `serverConfig.xml`, each with an `id` (the path segment) and
  an `href` (the destination: a site-relative path like
  `/posts/hello-world` or an absolute URL).
- **Served under `/links/`.** `ShortLinkHandler` in `pkg/api/links`
  answers `GET /links/{id}` with a 302 redirect to the entry's `href`, and
  404 when the id is unknown. 302, not 301: the mapping can be edited at
  any time, and browsers cache permanent redirects beyond such edits.
- **Read on the fly.** The handler asks a `ShortLinkDataProvider`
  (`pkg/models/shortlink`) on every request. The shipped implementation,
  `FsShortLinkDataProvider`, keeps only the configuration file's path and
  re-reads and re-parses the document per request, so edits apply without
  a restart.
- **Wired unconditionally.** `main.go` mounts the handler at `/links/`
  even without `--config-xml`; every id then answers 404.

## Layout

- `cmd/server/` — the Go server entrypoint (`main.go`): static export,
  the login/auth wiring, `/api/profile`, `/api/healthz`, the `/api/dyn/`
  mount, and the `/links/` mount.
- `pkg/` — the Go packages behind the server's endpoints:
  - `pkg/models/dyn/` — `DynBlogData`, the `DynBlogDataProvider` interface,
    and `FSBasedDynBlogData`.
  - `pkg/models/shortlink/` — the `ShortLinkDataProvider` interface and
    `FsShortLinkDataProvider`.
  - `pkg/models/serverconfig/` — the parser of the global configuration
    document's login sections (`<loginOptions/>`, `<oidcLoginOptions/>`,
    `<allowedOrigin/>`).
  - `pkg/api/dyn/` — `DynamicBlogDataHandler` and `HealthzHandler`.
  - `pkg/api/links/` — `ShortLinkHandler`, wired at `/links/`.
  - `pkg/api/profile/` — `ProfileHandler`, wired at `/api/profile`.
  - `pkg/api/login/` — the login handlers: `visitor/`, `oauth2/github/`,
    `oauth2/google/`, and `oidc/` (`general/` plus the Cloudflare Access
    JWT validator under `idTokenHeader/cloudflare/`).
  - `pkg/api/loginoptions/`, `pkg/api/logout/` — the login page's IdP list
    and the logout handler.
  - `pkg/api/common/` — the cookie names and the request-origin / redirect
    URL resolution shared by the login handlers.
  - `pkg/auth/` — the JWT issuer/validator, the whitelist auth middleware,
    the subject blacklist, the OAuth nonce issuer and the visitor ticket
    generator.
  - `pkg/cookie/` — the session/nonce cookie builder.
  - `pkg/github/`, `pkg/google/`, `pkg/oidc/` — the token/profile types and
    OIDC helpers behind the OAuth2/OIDC login handlers.
  - `pkg/session/` — the session model and the middleware that builds the
    request-scoped session from the JWT middleware's context values.
  - `pkg/log/` — the HTTP logging middleware wrapping the mux.
- `web/site/` — the Next.js frontend (see its README).
- `webfs.go` — embeds the frontend's static export (`web/site/out`) into the
  Go binary.
- `serverConfig.xml` / `serverConfig.xsd` / `serverConfig.xml.example` — the
  global configuration document, its schema, and a sample.

## Running it

Build the frontend, then run the server:

```sh
cd web/site && npm ci && npm run build && cd ../..
go run ./cmd/server --config-xml=serverConfig.xml
```

The server needs a JWT secret at startup (it signs the login session
tokens): export `JWT_SECRET`, or keep it in a gitignored `.env.local` —
conventional dotenv files are loaded before flag parsing. Then open
<http://localhost:8080>. `--config-xml` is optional; without it the
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
- Short links: the `<shortlink/>` entries of that same section, served live
  under `/links/`.
- The hard-coded visitor identity: `pkg/session`
  (`StaticVisitorSessionManager`).
- The favicons: `web/site/public/logo-light.png` and `logo-dark.png`.

## Todos

1. Icons (for dark and bright variant)
2. Math rendering (example MathML post)
3. Support listing updates from MCP, RSS

### WebRTC sub-system todos

1. voice and video calling
2. message unreads
3. peer connectionstate
