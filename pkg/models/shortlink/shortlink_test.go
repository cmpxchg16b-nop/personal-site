package shortlink

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeConfig writes a minimal server configuration document carrying the
// given <dynBlogData/> inner XML, returning its path.
func writeConfig(t *testing.T, inner string) string {
	t.Helper()
	doc := `<?xml version="1.0" encoding="UTF-8" ?>
<serverConfig>
  <dynBlogData>` + inner + `</dynBlogData>
</serverConfig>
`
	path := filepath.Join(t.TempDir(), "serverConfig.xml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFsShortLinkDataProvider_GetShortLinkById(t *testing.T) {
	path := writeConfig(t, `
    <shortlink id="gh" href="https://github.com/your-handle"/>
    <shortlink id="first-post" href="/posts/your-first-post"/>
  `)
	p := NewFsShortLinkDataProvider(path)

	for _, tc := range []struct {
		id   string
		want string
	}{
		{"gh", "https://github.com/your-handle"},
		{"first-post", "/posts/your-first-post"},
	} {
		href, err := p.GetShortLinkById(context.Background(), tc.id)
		if err != nil {
			t.Fatalf("GetShortLinkById(%q): %v", tc.id, err)
		}
		if href != tc.want {
			t.Fatalf("GetShortLinkById(%q) = %q, want %q", tc.id, href, tc.want)
		}
	}
}

func TestFsShortLinkDataProvider_UnknownId(t *testing.T) {
	path := writeConfig(t, `
    <shortlink id="gh" href="https://github.com/your-handle"/>
  `)
	p := NewFsShortLinkDataProvider(path)

	_, err := p.GetShortLinkById(context.Background(), "nope")
	if !errors.Is(err, ErrShortLinkNotFound) {
		t.Fatalf("GetShortLinkById of unknown id: got %v, want ErrShortLinkNotFound", err)
	}
}

func TestFsShortLinkDataProvider_RereadsOnEveryCall(t *testing.T) {
	path := writeConfig(t, `
    <shortlink id="gh" href="https://github.com/your-handle"/>
  `)
	p := NewFsShortLinkDataProvider(path)

	if _, err := p.GetShortLinkById(context.Background(), "new"); !errors.Is(err, ErrShortLinkNotFound) {
		t.Fatalf("first read: got %v, want ErrShortLinkNotFound", err)
	}

	// The provider keeps only the path: rewriting the document is picked up
	// by the next call on the same provider, without a restart.
	doc := `<?xml version="1.0" encoding="UTF-8" ?>
<serverConfig>
  <dynBlogData>
    <shortlink id="gh" href="https://github.com/your-handle"/>
    <shortlink id="new" href="/posts/a-third-post"/>
  </dynBlogData>
</serverConfig>
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	href, err := p.GetShortLinkById(context.Background(), "new")
	if err != nil {
		t.Fatalf("second read: GetShortLinkById: %v", err)
	}
	if href != "/posts/a-third-post" {
		t.Fatalf("second read: href = %q, want /posts/a-third-post", href)
	}
}

func TestFsShortLinkDataProvider_MissingFile(t *testing.T) {
	p := NewFsShortLinkDataProvider(filepath.Join(t.TempDir(), "does-not-exist.xml"))
	if _, err := p.GetShortLinkById(context.Background(), "gh"); err == nil {
		t.Fatal("expected an error for a missing file, got nil")
	}
}
