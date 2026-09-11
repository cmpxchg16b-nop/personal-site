package dyn

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
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

func TestFSBasedDynBlogData_GetDynBlogData(t *testing.T) {
	path := writeConfig(t, `
    <postMetadata id="post-1" href="/posts/post-1" title="First Post" description="The first post." lastModified="2026-03-08" creation="2026-03-01" tags="meta, example ,"/>
    <postMetadata id="post-2" href="/posts/post-2" title="Second Post" description="The second post." creation="2026-02-14"/>
    <project id="p1" name="Project One" description="First." url="https://example.com/one" tech="Go, Next.js ,"/>
    <project id="p2" name="Project Two" description="Second." url="https://example.com/two"/>
    <authorContact id="c1" kind="email" label="you@example.com" url="mailto:you@example.com"/>
    <entertain>
      <live id="mystream" name="mystream" displayName="My Stream" description="The site's own live stream." thumbnail="https://example.com/thumb.png" href="http://localhost:8889/mystream/whep"/>
      <video id="v1" name="clip-1" displayName="Clip One" description="The first clip." href="/entertain#v1"/>
      <music id="m1" name="track-1" displayName="Track One" description="The first track."/>
    </entertain>
    <playable id="mystream-whep" mediaId="mystream" type="whep" url="http://localhost:8889/mystream/whep"/>
    <playable id="mystream-hls" mediaId="mystream" type="hls" url="http://localhost:8889/mystream/index.m3u8"/>
    <menu>
      <menuEntry id="6ce7ecbd-3ddc-4d58-b2f6-4828a50b7e85" name="home" displayName="Home" description="The site's home page." iconClassName="home">
        <i18nDisplayName key="en" value="Home"/>
        <i18nDisplayName key="zh" value="首页"/>
      </menuEntry>
      <menuEntry id="d7c2a874-622a-4ff0-8515-f5b028555edf" name="entertain" displayName="Entertain" iconClassName="musicNote"/>
    </menu>
  `)

	data, err := NewFSBasedDynBlogData(path).GetDynBlogData()
	if err != nil {
		t.Fatalf("GetDynBlogData: %v", err)
	}

	if len(data.Posts) != 2 {
		t.Fatalf("posts count: got %d, want 2", len(data.Posts))
	}
	post1 := data.Posts[0]
	if post1.Id != "post-1" || post1.Href != "/posts/post-1" || post1.Title != "First Post" || post1.Description != "The first post." {
		t.Fatalf("unexpected post payload: %+v", post1)
	}
	if post1.Creation != "2026-03-01" || post1.LastModified != "2026-03-08" {
		t.Fatalf("dates: got creation %q lastModified %q, want 2026-03-01 and 2026-03-08", post1.Creation, post1.LastModified)
	}
	if !slices.Equal(post1.Tags, []string{"meta", "example"}) {
		t.Fatalf("tags: got %v, want [meta example]", post1.Tags)
	}
	// lastModified is optional: absent means empty; tags absent means an empty list.
	if data.Posts[1].LastModified != "" {
		t.Fatalf("lastModified without attribute: got %q, want empty", data.Posts[1].LastModified)
	}
	if len(data.Posts[1].Tags) != 0 {
		t.Fatalf("tags without attribute: got %v, want empty", data.Posts[1].Tags)
	}

	if len(data.Projects) != 2 {
		t.Fatalf("projects count: got %d, want 2", len(data.Projects))
	}
	p1 := data.Projects[0]
	if p1.Id != "p1" || p1.Name != "Project One" || p1.Description != "First." || p1.URL != "https://example.com/one" {
		t.Fatalf("unexpected project payload: %+v", p1)
	}
	if !slices.Equal(p1.Tech, []string{"Go", "Next.js"}) {
		t.Fatalf("tech: got %v, want [Go Next.js]", p1.Tech)
	}
	// tech is optional: absent means an empty list.
	if len(data.Projects[1].Tech) != 0 {
		t.Fatalf("tech without attribute: got %v, want empty", data.Projects[1].Tech)
	}

	if len(data.AuthorContacts) != 1 {
		t.Fatalf("author contacts count: got %d, want 1", len(data.AuthorContacts))
	}
	c1 := data.AuthorContacts[0]
	if c1.Id != "c1" || c1.Kind != "email" || c1.Label != "you@example.com" || c1.URL != "mailto:you@example.com" {
		t.Fatalf("unexpected author contact payload: %+v", c1)
	}

	if len(data.Entertain.Live) != 1 {
		t.Fatalf("live count: got %d, want 1", len(data.Entertain.Live))
	}
	live1 := data.Entertain.Live[0]
	if live1.Id != "mystream" || live1.Name != "mystream" || live1.DisplayName != "My Stream" || live1.Description != "The site's own live stream." || live1.Href != "http://localhost:8889/mystream/whep" {
		t.Fatalf("unexpected live payload: %+v", live1)
	}
	if live1.Thumbnail != "https://example.com/thumb.png" {
		t.Fatalf("thumbnail: got %q, want https://example.com/thumb.png", live1.Thumbnail)
	}
	if len(data.Entertain.Videos) != 1 || data.Entertain.Videos[0].Id != "v1" {
		t.Fatalf("unexpected videos payload: %+v", data.Entertain.Videos)
	}
	if len(data.Entertain.Music) != 1 || data.Entertain.Music[0].Id != "m1" {
		t.Fatalf("unexpected music payload: %+v", data.Entertain.Music)
	}
	// thumbnail is optional: absent means empty.
	if data.Entertain.Videos[0].Thumbnail != "" {
		t.Fatalf("thumbnail without attribute: got %q, want empty", data.Entertain.Videos[0].Thumbnail)
	}
	// href is optional: absent means empty.
	if data.Entertain.Music[0].Href != "" {
		t.Fatalf("href without attribute: got %q, want empty", data.Entertain.Music[0].Href)
	}

	if len(data.Playables) != 2 {
		t.Fatalf("playables count: got %d, want 2", len(data.Playables))
	}
	whep := data.Playables[0]
	if whep.Id != "mystream-whep" || whep.MediaId != "mystream" || whep.Type != PlayableTypeWHEP || whep.URL != "http://localhost:8889/mystream/whep" {
		t.Fatalf("unexpected playable payload: %+v", whep)
	}
	if data.Playables[1].Id != "mystream-hls" || data.Playables[1].Type != PlayableTypeHLS {
		t.Fatalf("unexpected playable payload: %+v", data.Playables[1])
	}

	if len(data.Menu) != 2 {
		t.Fatalf("menu count: got %d, want 2", len(data.Menu))
	}
	home := data.Menu[0]
	if home.Id != "6ce7ecbd-3ddc-4d58-b2f6-4828a50b7e85" || home.Name != "home" || home.DisplayName != "Home" || home.Description != "The site's home page." || home.IconClassName != "home" {
		t.Fatalf("unexpected menu entry payload: %+v", home)
	}
	if !reflect.DeepEqual(home.I18nDisplayNames, map[string]string{"en": "Home", "zh": "首页"}) {
		t.Fatalf("i18nDisplayNames: got %v, want map[en:Home zh:首页]", home.I18nDisplayNames)
	}
	if data.Menu[1].Name != "entertain" || data.Menu[1].IconClassName != "musicNote" {
		t.Fatalf("unexpected menu entry payload: %+v", data.Menu[1])
	}
	// description is optional: absent means empty.
	if data.Menu[1].Description != "" {
		t.Fatalf("description without attribute: got %q, want empty", data.Menu[1].Description)
	}
	// i18nDisplayName children are optional: absent means a nil map.
	if data.Menu[1].I18nDisplayNames != nil {
		t.Fatalf("i18nDisplayNames without children: got %v, want nil", data.Menu[1].I18nDisplayNames)
	}
}

func TestFSBasedDynBlogData_RereadsOnEveryCall(t *testing.T) {
	path := writeConfig(t, `
    <project id="p1" name="Project One" description="First." url="https://example.com/one" tech="Go"/>
  `)
	p := NewFSBasedDynBlogData(path)

	data, err := p.GetDynBlogData()
	if err != nil {
		t.Fatalf("first GetDynBlogData: %v", err)
	}
	if len(data.Projects) != 1 {
		t.Fatalf("first read: projects count = %d, want 1", len(data.Projects))
	}

	// The provider keeps only the path: rewriting the document is picked up
	// by the next call on the same provider, without a restart.
	doc := `<?xml version="1.0" encoding="UTF-8" ?>
<serverConfig>
  <dynBlogData>
    <project id="p1" name="Project One" description="First." url="https://example.com/one" tech="Go"/>
    <project id="p2" name="Project Two" description="Second." url="https://example.com/two"/>
  </dynBlogData>
</serverConfig>
`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	data, err = p.GetDynBlogData()
	if err != nil {
		t.Fatalf("second GetDynBlogData: %v", err)
	}
	if len(data.Projects) != 2 {
		t.Fatalf("second read: projects count = %d, want 2", len(data.Projects))
	}
}

func TestFSBasedDynBlogData_MissingFile(t *testing.T) {
	p := NewFSBasedDynBlogData(filepath.Join(t.TempDir(), "does-not-exist.xml"))
	if _, err := p.GetDynBlogData(); err == nil {
		t.Fatal("expected an error for a missing file, got nil")
	}
}

func TestParseCommaSeparated(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Go", []string{"Go"}},
		{"Go,Next.js", []string{"Go", "Next.js"}},
		{" Go , Next.js ,", []string{"Go", "Next.js"}},
	} {
		if got := parseCommaSeparated(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("parseCommaSeparated(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
