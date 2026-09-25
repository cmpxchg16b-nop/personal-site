module personal-site

go 1.25.0

// Pin a patched 1.25.x toolchain: auto-switched toolchains older than the
// golang/go#75031 fix break "go test -cover" with 'no such tool "covdata"'.
toolchain go1.25.12

require github.com/alecthomas/kong v1.16.0

require (
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/emiago/diago v0.32.2
	github.com/emiago/sipgo v1.6.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.3
	github.com/hraban/opus v0.0.0-20260708213942-bde8e4304501
	github.com/joho/godotenv v1.5.1
	github.com/mewkiz/flac v1.0.14
	github.com/miekg/dns v1.1.73
	github.com/pion/opus v0.1.0
	github.com/pion/rtp v1.10.5
	github.com/pion/webrtc/v4 v4.2.20
	github.com/zaf/g711 v1.4.0
	golang.org/x/oauth2 v0.36.0
)

// diago is vendored with local patches (see third_party/diago/PATCHES.md):
// the SDP generator honors a codec's Fmtp line, which upstream (checked
// through v0.40.0) hardcodes — the sip bot needs it to announce stereo
// opus (a=fmtp:96 useinbandfec=1;stereo=1) toward the SIP network.
replace github.com/emiago/diago => ./third_party/diago

require (
	github.com/emiago/dtls/v3 v3.0.0-20260122183559-8b8d23e359c0 // indirect
	github.com/go-audio/riff v1.0.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/gobwas/pool v0.2.1 // indirect
	github.com/gobwas/ws v1.4.0 // indirect
	github.com/icholy/digest v1.1.0 // indirect
	github.com/icza/bitio v1.1.0 // indirect
	github.com/mewkiz/pkg v0.0.0-20250417130911-3f050ff8c56d // indirect
	github.com/mewpkg/term v0.0.0-20241026122259-37a80af23985 // indirect
	github.com/pion/datachannel v1.6.2 // indirect
	github.com/pion/dtls/v3 v3.1.8 // indirect
	github.com/pion/ice/v4 v4.4.2 // indirect
	github.com/pion/interceptor v0.1.48 // indirect
	github.com/pion/logging v0.2.4 // indirect
	github.com/pion/mdns/v2 v2.2.0 // indirect
	github.com/pion/randutil v0.1.0 // indirect
	github.com/pion/rtcp v1.2.17 // indirect
	github.com/pion/sctp v1.11.1 // indirect
	github.com/pion/sdp/v3 v3.0.19 // indirect
	github.com/pion/srtp/v3 v3.0.13 // indirect
	github.com/pion/stun/v4 v4.0.0 // indirect
	github.com/pion/transport/v4 v4.1.0 // indirect
	github.com/pion/turn/v5 v5.1.0 // indirect
	github.com/wlynxg/anet v0.0.5 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/time v0.14.0 // indirect
	gopkg.in/hraban/opus.v2 v2.0.0-20230925203106-0188a62cb302 // indirect
)
