// Package shortlink models the site's short links — stable short paths
// under /links/ that redirect to longer or changing destinations — and how
// they are provided to the API layer (see pkg/api/links, which serves the
// redirects).
//
// The links are authored as <shortlink/> entries of the <dynBlogData/>
// section of the global server configuration document (serverConfig.xml,
// validated against serverConfig.xsd in the project root).
package shortlink

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
)

// ErrShortLinkNotFound is returned (wrapped) by ShortLinkDataProvider
// implementations when no short link with the requested id exists. Callers
// match it with errors.Is, e.g. to answer the request with 404 instead of
// 500.
var ErrShortLinkNotFound = errors.New("short link not found")

// ShortLinkDataProvider supplies short link destinations to the API layer.
// Implementations may re-read the underlying source on every call, so
// callers must not cache the result.
type ShortLinkDataProvider interface {
	// GetShortLinkById returns the destination href of the short link with
	// the given id — a site-relative path ("/posts/hello-world") or an
	// absolute URL. It returns an error wrapping ErrShortLinkNotFound when
	// no such id exists. The context is available for implementations backed
	// by a remote store; filesystem-backed implementations may ignore it.
	GetShortLinkById(ctx context.Context, shortLinkId string) (string, error)
}

// FsShortLinkDataProvider is a ShortLinkDataProvider backed by the server
// configuration document on the filesystem. It keeps only the document's
// path in memory: every GetShortLinkById call re-reads and re-parses the
// file, so edits to the <shortlink/> entries apply without a server
// restart. The document is small, so per-request reads are cheap.
type FsShortLinkDataProvider struct {
	path string
}

// NewFsShortLinkDataProvider constructs a FsShortLinkDataProvider reading
// the server configuration document at path.
func NewFsShortLinkDataProvider(path string) *FsShortLinkDataProvider {
	return &FsShortLinkDataProvider{path: path}
}

func (p *FsShortLinkDataProvider) GetShortLinkById(_ context.Context, shortLinkId string) (string, error) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return "", fmt.Errorf("failed to read server config file %s: %w", p.path, err)
	}
	// Only the <shortlink/> entries of the <dynBlogData/> section are
	// mirrored here; xml.Unmarshal ignores the document's other sections.
	var doc struct {
		DynBlogData struct {
			ShortLinks []shortLinkXML `xml:"shortlink"`
		} `xml:"dynBlogData"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("failed to parse server config file %s: %w", p.path, err)
	}
	for _, sl := range doc.DynBlogData.ShortLinks {
		if sl.Id == shortLinkId {
			return sl.Href, nil
		}
	}
	return "", fmt.Errorf("short link %q: %w", shortLinkId, ErrShortLinkNotFound)
}

// shortLinkXML mirrors a single <shortlink/> entry of the <dynBlogData/>
// section of serverConfig.xml.
type shortLinkXML struct {
	Id   string `xml:"id,attr"`
	Href string `xml:"href,attr"`
}
