# personal-site

My personal site and blog, built with simplicity and brevity in mind.

## What it is

- **A purely client-rendered web app.** The frontend in `web/site/` is a
  Next.js static export: hero, live, about, posts, projects, and contact
  sections, all with placeholder copy ("My Name", example descriptions,
  example contacts) ready to be replaced.
- **Dark mode.** Light and dark themes that follow your system setting, or
  flip between them yourself with the toggle in the top bar.
- **Bilingual.** English and 中文 out of the box, with a language switcher in
  the top bar; all site copy lives in the two translation bundles.
- **A small Go backend.** One self-contained binary that embeds and serves
  the static export, and answers the site's API endpoints: `GET
/api/profile` (the caller's identity, from its JWT session), `GET
/api/healthz`, the dynamic blog data under `/api/dyn/` (see below), and
  short-link redirects under `/links/` (see below), and the live stream's
  WHEP endpoint at `GET /api/homeLiveWHEPURL` (see below).

## Sign-in and sessions

The backend carries a JWT-based login system: a login endpoint issues a
signed session token into an HttpOnly cookie, and a whitelist middleware
(`pkg/auth`) requires a valid token for every `/api/` path except the
public ones (`/api/login/...`, `/api/logout`, `/api/healthz`, `/api/dyn/`,
`GET /api/iceServers`, `GET /api/homeLiveWHEPURL`, and
`GET /api/comments/` — reads are open, appends need a session).

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

Machine clients that cannot log in interactively — the built-in bots'
`<echoBot/>` and `<musicBot/>` elements are the examples — carry a static
session token instead.
Issue one with the server binary's `sign` subcommand, which signs with the
same secret indirection as the server itself:

```sh
go run ./cmd/server sign --sub bot:echo --username "Echo Bot" --validity 720h
```

The token alone is printed on stdout, so the command composes in a shell.
`--jti` defaults to a fresh random session id; `--validity 0` issues a token
without an expiry claim.

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

## WebRTC chat

The site also carries a chat subsystem: signed-in visitors discover each
other in a shared channel on the `/chat` page and then talk
**peer-to-peer** — text lines, file/image/video attachments with live
progress, and voice and video calls — with two built-in bots to talk to.
`docs/signalling-server.md` is the full protocol write-up; the short
version:

- **Signalling over WebSocket.** `WebSocketSSHandler`
  (`pkg/api/websocket_ss`), mounted at `/api/ss/ws` on the in-memory
  `SimpleOnMemorySSProvider` (`pkg/models/ss`), registers each client as
  a channel subscriber and relays the WebRTC handshake — session
  descriptions and trickle ICE candidates — between subscribers.
  Discovery and establishment only: chat traffic itself never passes
  through the server. Identity comes from the caller's JWT session (the
  path is not whitelisted): the endpoint stamps the session's subject
  and session id onto every event and overrides the display name with
  the token's username claim. Registrations live in process memory and
  age out lazily (`--ss-aging`, default `10s`) unless the client keeps
  them alive. ICE servers come from `GET /api/iceServers` — public
  bootstrap data serving the `<iceServer/>` entries of
  `serverConfig.xml`.
- **Perfect negotiation.** Each end runs the MDN perfect-negotiation
  pattern over the relay — `web/site/src/api/ss/negotiate.ts` in the
  browser, `PerfectNegotiator` (`pkg/rtc`, on `pion/webrtc`) in Go —
  with the lexicographically smaller subscriber id as the polite peer.
- **Two data channels per pair.** Once connected, the peers talk
  directly and in-band: `dcmsg` carries JSON frames — plain text,
  file-transfer status, chat-control edits (delete/amend, own messages
  only), and a SIP subset (`INVITE` / `200 OK` / `603 Decline` /
  `CANCEL` / `BYE`) driving the calls — while `dcbin` carries the file
  bytes as compact binary frames (16 KiB chunks under a 64-frame sliding
  window, cumulative acknowledgements, strict reassembly). Ordinary
  messages echo back to the sender so both sides build the same history;
  call dialog messages, like real SIP, never echo.
- **Calls ride the same connection.** Accepting a call attaches the
  microphone — and the camera, for a video call — to the pair's existing
  peer connection, and the negotiators renegotiate on their own. All
  call audio runs through one shared `AudioContext` graph (per-call
  volumes, FFT visualizations); remote videos render in floating,
  draggable cards.
- **Headless Go bots.** `HeadlessRTCClient` (`pkg/rtc`) is the Go
  counterpart of the browser client, and the server binary hosts two
  bots on it as ordinary authenticated clients when the configuration
  document carries an `<echoBot/>` or `<musicBot/>` element: the echo
  bot (`pkg/rtc/echobot`) answers text verbatim, declines every call,
  and reports the running sha256 of any file it receives; the music
  bot (`pkg/rtc/musicbot`) answers a chat CLI (`/help`, `/list-songs`,
  `/play <song>`) and voice calls with a song of its configured
  songbook. A song is an `<audioSource/>` child of `<musicBot/>`,
  modeled by `pkg/models/audiosource`'s `AudioSourceData` — inline
  (base64) or at a url, optionally FLAC compressed or Ogg-framed, with
  three accepted format combinations: μ-law 8 kHz mono (played as PCMU,
  byte for byte), linear PCM 48 kHz stereo (played as opus, nothing
  downmixed; integer sources encode through libopus's 16-bit API, float
  sources through its float API — the encoder is libopus via cgo, so
  pure-Go builds carry a stub that refuses linear PCM songs), and
  Ogg-framed opus 48 kHz (played as-is: the packets pass through with
  no decoding or re-encoding — the one linear-quality family a pure-Go
  build plays). Sample data loads lazily as a stream and loops by
  rewinding; the shipped example song (`assets/chiptune.ulaw`) is
  regenerated with `go run ./cmd/synthchiptune`. Their static session
  tokens are issued with the `sign` subcommand (see _Sign-in and
  sessions_).

## Live streaming

The home page carries a Live section under the hero: a video element
reading the owner's live stream over WHEP (WebRTC HTTP Egress Protocol)
from a stream server — MediaMTX, for instance, serves a path's stream at
`http://host:8889/<path>/whep`. The endpoint is public bootstrap data
from the global configuration document: its `<homeLiveWHEPURL/>`
element is served by `GET /api/homeLiveWHEPURL` (`pkg/api/homelive`, on
the JWT whitelist), and a document without the element simply has no
Live section. The browser client (`web/site/src/api/whep.ts`) POSTs a
recvonly SDP offer to the endpoint, takes the answer and the session
Location back, trickles its ICE candidates by PATCH and tears the
session down by DELETE; while the stream is offline it reconnects on its
own. It shares no code or configuration with the chat subsystem's
peer-to-peer WebRTC.

## Layout

- `cmd/server/` — the Go server entrypoint: the `serve` subcommand
  (`main.go`) serves the static export and wires the login/auth stack,
  `/api/profile`, `/api/healthz`, the `/api/dyn/` mount, the `/links/`
  mount and the signalling endpoint; the `sign` subcommand (`sign.go`)
  issues session JWTs for machine clients.
- `cmd/synthchiptune/` — the one-off synthesizer of the music bot's
  example song: renders the chiptune loop to `assets/chiptune.ulaw`
  (8 kHz mono μ-law, byte-deterministic).
- `assets/` — the music bot's example audio asset
  (`chiptune.ulaw`), pointed at by the sample configuration's
  `<audioSource/>` entry.
- `pkg/` — the Go packages behind the server's endpoints:
  - `pkg/models/audiosource/` — `AudioSourceData` and its streaming
    `Open`/`Rewind` access to lazily loaded, lazily decoded sample data
    (the music bot's songbook model).
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
go run ./cmd/server serve --config-xml=serverConfig.xml
```

The server needs a JWT secret at startup (it signs the login session
tokens): export `JWT_SECRET`, or keep it in a gitignored `.env.local` —
conventional dotenv files are loaded before flag parsing. Then open
<http://localhost:8080>. `--config-xml` is optional; without it the
`/api/dyn/` endpoints serve empty lists.

For frontend development with hot reload, run the two side by side:

```sh
cd web/site && npm run dev   # http://localhost:3000, /api proxied to :8080
go run ./cmd/server serve --config-xml=serverConfig.xml   # in another terminal
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
`serve --config-xml=serverConfig.xml` appended to the entrypoint arguments.

## Making it yours

- Site copy (name, tagline, about paragraphs):
  `web/site/src/i18n/locales/en.json` and `zh.json`.
- Posts, projects, and author contacts: the `<dynBlogData/>` section of
  `serverConfig.xml`, served live under `/api/dyn/`.
- Short links: the `<shortlink/>` entries of that same section, served live
  under `/links/`.
- The home page's live stream: the `<homeLiveWHEPURL/>` element of
  `serverConfig.xml`, served by `GET /api/homeLiveWHEPURL`; like the login
  options it is read at startup, so a server restart applies an edit.
- The hard-coded visitor identity: `pkg/session`
  (`StaticVisitorSessionManager`).
- The favicons: `web/site/public/logo-light.png` and `logo-dark.png`.

## Todos

1. Icons (for dark and bright variant)
2. Math rendering (example MathML post)
3. Support listing updates from MCP, RSS

### WebRTC sub-system todos

1. Audio-inline message
2. Message unreads display
3. LLM chat bot integration
