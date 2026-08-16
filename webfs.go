// Package personalsite embeds and serves the static web assets shipped with
// the binary.
package personalsite

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// web holds the contents of the Next.js static export (web/site/out) at
// compile time. The "all:" prefix is required so that directories such as
// _next/ and _not-found/ are included; without it, embed skips any file or
// directory whose name begins with "_" or ".".
//
//go:embed all:web/site/out
var web embed.FS

// WebFS returns the embedded Next.js export as a read-only filesystem rooted
// at web/site/out (so paths carry no "web/site/out/" prefix).
func WebFS() fs.FS {
	fsys, err := fs.Sub(web, "web/site/out")
	if err != nil {
		// fs.Sub only fails for an invalid or absent name; the path above is
		// embedded, so this is unreachable.
		panic(err)
	}
	return fsys
}

// Handler serves the embedded Next.js static export. It maps clean URLs to
// their pre-rendered files (/foo -> /foo.html or /foo/index.html) and falls
// back to 404.html (with a 404 status) for unknown routes.
func Handler() http.Handler {
	fsys := WebFS()
	fileServer := http.FileServerFS(fsys)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target, ok := lookup(fsys, r.URL.Path)
		if !ok {
			serveNotFound(w, r, fsys)
			return
		}
		// Serve the resolved target without changing the URL the client sees.
		if target != r.URL.Path {
			r = r.Clone(r.Context())
			r.URL.Path = target
		}
		fileServer.ServeHTTP(w, r)
	})
}

// lookup resolves a request path to an existing file or directory in fsys,
// applying Next.js clean-URL conventions. It returns the path to hand to a
// file server (unchanged when no rewrite is needed).
func lookup(fsys fs.FS, urlPath string) (string, bool) {
	name := strings.TrimPrefix(urlPath, "/")

	if name == "" {
		return urlPath, fileExists(fsys, "index.html")
	}

	// Exact file or directory.
	if info, err := fs.Stat(fsys, name); err == nil {
		if !info.IsDir() {
			return urlPath, true
		}
		// Directory: servable only when it contains an index.html.
		if fileExists(fsys, name+"/index.html") {
			if strings.HasSuffix(urlPath, "/") {
				return urlPath, true
			}
			return urlPath + "/", true
		}
		// A directory without an index.html is not servable — Next.js emits
		// per-route RSC payload files into <route>/ alongside <route>.html —
		// so fall through to the clean-URL checks below.
	}

	// Clean URL: /foo -> /foo.html
	if fileExists(fsys, name+".html") {
		return urlPath + ".html", true
	}

	// Clean URL: /foo -> /foo/index.html
	if fileExists(fsys, name+"/index.html") {
		return urlPath + "/", true
	}

	return "", false
}

// fileExists reports whether name is a regular (non-directory) file in fsys.
func fileExists(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	return err == nil && !info.IsDir()
}

// serveNotFound writes the Next.js 404 page (404.html) with a 404 status,
// falling back to the standard not-found response when it is unavailable.
func serveNotFound(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	if b, err := fs.ReadFile(fsys, "404.html"); err == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write(b)
		return
	}
	http.NotFound(w, r)
}
