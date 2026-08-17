module personal-site

go 1.25.0

// Pin a patched 1.25.x toolchain: auto-switched toolchains older than the
// golang/go#75031 fix break "go test -cover" with 'no such tool "covdata"'.
toolchain go1.25.12

require github.com/alecthomas/kong v1.16.0
