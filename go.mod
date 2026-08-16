module personal-site

go 1.25.0

// Pin a patched 1.25.x toolchain: auto-switched toolchains older than the
// golang/go#75031 fix break "go test -cover" with 'no such tool "covdata"'.
toolchain go1.25.12

require github.com/google/uuid v1.6.0

require (
	github.com/alecthomas/kong v1.16.0
	github.com/beevik/etree v1.7.0
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/emersion/go-sasl v0.0.0-20241020182733-b788ff22d5a6
	github.com/emersion/go-smtp v0.24.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/joho/godotenv v1.5.1
	github.com/russellhaering/goxmldsig v1.6.1
	github.com/spf13/afero v1.15.0
	golang.org/x/oauth2 v0.36.0
)

require (
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	golang.org/x/text v0.28.0 // indirect
)
