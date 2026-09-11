// Package dyn models the site's dynamic blog data — the post metadata,
// project and author-contact lists, and the entertain page's media shelves
// the frontend renders — and how that data is provided to the API layer (see
// pkg/api/dyn, which serves it under /api/dyn/).
//
// The data is authored in the <dynBlogData/> section of the global server
// configuration document (serverConfig.xml, validated against
// serverConfig.xsd in the project root).
package dyn

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"
)

// PostMetadata is the metadata of one blog post: what its card shows and
// where the card links. Dates are ISO date strings (e.g. "2026-03-01");
// LastModified is empty when the post was never edited after publication.
type PostMetadata struct {
	Id           string   `json:"id"`
	Href         string   `json:"href"`
	Title        string   `json:"title"`
	Description  string   `json:"description"`
	LastModified string   `json:"lastModified,omitempty"`
	Creation     string   `json:"creation"`
	Tags         []string `json:"tags"`
}

// Project is one entry of the site's project list. Tech holds the
// comma-separated technologies of the entry's tech attribute, split and
// trimmed (see parseCommaSeparated).
type Project struct {
	Id          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	Tech        []string `json:"tech"`
}

// AuthorContact is one way to reach the site author: an iconizable kind
// ("email", "github", …), a human-readable label, and the URL to open.
type AuthorContact struct {
	Id    string `json:"id"`
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Media is one media card entry of the entertain page's Live, Video, or
// Music shelf. Id uniquely identifies the entry — the media id; Name is the
// media's slug-like handle (e.g. "mystream"); DisplayName and Description
// are shown on the card. Thumbnail is the card's cover image — a data URL,
// an absolute URL, or a site-relative URL; empty renders a placeholder
// tile. Href, optional, is where clicking the card navigates — a
// site-relative path or an absolute URL; without one the frontend links the
// card to the site's play page (/play?mediaId=<id>), which plays the
// media's Playable sources.
type Media struct {
	Id          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Thumbnail   string `json:"thumbnail,omitempty"`
	Href        string `json:"href,omitempty"`
}

// PlayableType is the streaming protocol a Playable's URL speaks:
// PlayableTypeWHEP (WebRTC HTTP Egress Protocol) or PlayableTypeHLS.
type PlayableType string

const (
	PlayableTypeWHEP PlayableType = "whep"
	PlayableTypeHLS  PlayableType = "hls"
)

// Playable is one playable source of a media entry — a <live/>, <video/>,
// or <music/> entry of the entertain page's shelves. Id uniquely identifies
// the entry; MediaId is the media id of the entry it plays — several
// playables may share a media id (e.g. a WHEP and an HLS variant of the
// same stream); Type is the protocol URL speaks; URL is the endpoint to
// play from.
type Playable struct {
	Id      string       `json:"id"`
	MediaId string       `json:"mediaId"`
	Type    PlayableType `json:"type"`
	URL     string       `json:"url"`
}

// Entertain is the entertain page's dynamic content: the Live, Video, and
// Music media shelves, served by GET /api/dyn/entertain.
type Entertain struct {
	Live   []Media `json:"live"`
	Videos []Media `json:"videos"`
	Music  []Media `json:"music"`
}

// MenuEntry is one entry of the top bar's navigation drawer. Id uniquely
// identifies the entry; Name is the page's route slug — the drawer navigates
// to "/<name>", with the special name "home" mapping to "/"; DisplayName
// is the entry's caption — the fallback when I18nDisplayNames carries no
// caption for the active language. Description, optional, is the entry's
// secondary line (empty when the entry defines none). IconClassName picks
// the entry's icon from the frontend's icon map (e.g. "home",
// "musicNote").
type MenuEntry struct {
	Id          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	// I18nDisplayNames maps an i18n language code ("en", "zh", …) to the
	// entry's caption in that language; nil when the entry defines none.
	I18nDisplayNames map[string]string `json:"i18nDisplayNames,omitempty"`
	Description      string            `json:"description,omitempty"`
	IconClassName    string            `json:"iconClassName"`
}

// DynBlogData is the whole of the site's dynamic blog data: everything the
// <dynBlogData/> section of the server configuration document carries.
type DynBlogData struct {
	Posts          []PostMetadata  `json:"posts"`
	Projects       []Project       `json:"projects"`
	AuthorContacts []AuthorContact `json:"authorContacts"`
	Entertain      Entertain       `json:"entertain"`
	Menu           []MenuEntry     `json:"menu"`
	Playables      []Playable      `json:"playables"`
}

// DynBlogDataProvider supplies the site's dynamic blog data to the API
// layer. Implementations may re-read the underlying source on every call, so
// callers must not cache the result.
type DynBlogDataProvider interface {
	GetDynBlogData() (*DynBlogData, error)
}

// FSBasedDynBlogData is a DynBlogDataProvider backed by the server
// configuration document on the filesystem. It keeps only the document's
// path in memory: every GetDynBlogData call re-reads and re-parses the file,
// so edits to the <dynBlogData/> section apply without a server restart. The
// document is small, so per-request reads are cheap.
type FSBasedDynBlogData struct {
	path string
}

// NewFSBasedDynBlogData constructs a FSBasedDynBlogData reading the server
// configuration document at path.
func NewFSBasedDynBlogData(path string) *FSBasedDynBlogData {
	return &FSBasedDynBlogData{path: path}
}

func (p *FSBasedDynBlogData) GetDynBlogData() (*DynBlogData, error) {
	data, err := os.ReadFile(p.path)
	if err != nil {
		return nil, fmt.Errorf("failed to read server config file %s: %w", p.path, err)
	}
	// Only the <dynBlogData/> section is mirrored here; xml.Unmarshal ignores
	// the document's other sections.
	var doc struct {
		DynBlogData dynBlogDataXML `xml:"dynBlogData"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("failed to parse server config file %s: %w", p.path, err)
	}
	return doc.DynBlogData.toDynBlogData(), nil
}

// dynBlogDataXML mirrors the <dynBlogData/> section of serverConfig.xml.
type dynBlogDataXML struct {
	Posts          []postMetadataXML  `xml:"postMetadata"`
	Projects       []projectXML       `xml:"project"`
	AuthorContacts []authorContactXML `xml:"authorContact"`
	Entertain      entertainXML       `xml:"entertain"`
	Playables      []playableXML      `xml:"playable"`
	Menu           []menuEntryXML     `xml:"menu>menuEntry"`
}

// postMetadataXML mirrors a single <postMetadata/> entry of the
// <dynBlogData/> section of serverConfig.xml.
type postMetadataXML struct {
	Id           string `xml:"id,attr"`
	Href         string `xml:"href,attr"`
	Title        string `xml:"title,attr"`
	Description  string `xml:"description,attr"`
	LastModified string `xml:"lastModified,attr"`
	Creation     string `xml:"creation,attr"`
	// Tags is the raw comma-separated tags attribute; an empty string means
	// no tags.
	Tags string `xml:"tags,attr"`
}

// projectXML mirrors a single <project/> entry of the <dynBlogData/>
// section of serverConfig.xml.
type projectXML struct {
	Id          string `xml:"id,attr"`
	Name        string `xml:"name,attr"`
	Description string `xml:"description,attr"`
	URL         string `xml:"url,attr"`
	// Tech is the raw comma-separated tech attribute; an empty string means
	// no technologies.
	Tech string `xml:"tech,attr"`
}

// authorContactXML mirrors a single <authorContact/> entry of the
// <dynBlogData/> section of serverConfig.xml.
type authorContactXML struct {
	Id    string `xml:"id,attr"`
	Kind  string `xml:"kind,attr"`
	Label string `xml:"label,attr"`
	URL   string `xml:"url,attr"`
}

// entertainXML mirrors the <entertain/> element of the <dynBlogData/>
// section of serverConfig.xml: the entertain page's three media shelves.
type entertainXML struct {
	Live   []mediaXML `xml:"live"`
	Videos []mediaXML `xml:"video"`
	Music  []mediaXML `xml:"music"`
}

// mediaXML mirrors a single <live/>, <video/>, or <music/> entry of the
// <entertain/> element of serverConfig.xml.
type mediaXML struct {
	Id          string `xml:"id,attr"`
	Name        string `xml:"name,attr"`
	DisplayName string `xml:"displayName,attr"`
	Description string `xml:"description,attr"`
	Thumbnail   string `xml:"thumbnail,attr"`
	Href        string `xml:"href,attr"`
}

// playableXML mirrors a single <playable/> entry of the <dynBlogData/>
// section of serverConfig.xml.
type playableXML struct {
	Id      string `xml:"id,attr"`
	MediaId string `xml:"mediaId,attr"`
	Type    string `xml:"type,attr"`
	URL     string `xml:"url,attr"`
}

// menuEntryXML mirrors a single <menuEntry/> entry of the <menu/> element of
// serverConfig.xml.
type menuEntryXML struct {
	Id               string               `xml:"id,attr"`
	Name             string               `xml:"name,attr"`
	DisplayName      string               `xml:"displayName,attr"`
	Description      string               `xml:"description,attr"`
	IconClassName    string               `xml:"iconClassName,attr"`
	I18nDisplayNames []i18nDisplayNameXML `xml:"i18nDisplayName"`
}

// i18nDisplayNameXML mirrors a single <i18nDisplayName/> child of a
// <menuEntry/> element of serverConfig.xml: Key is the i18n language code
// ("en", "zh", …), Value the entry's caption in that language.
type i18nDisplayNameXML struct {
	Key   string `xml:"key,attr"`
	Value string `xml:"value,attr"`
}

func (x dynBlogDataXML) toDynBlogData() *DynBlogData {
	data := &DynBlogData{
		Posts:          make([]PostMetadata, 0, len(x.Posts)),
		Projects:       make([]Project, 0, len(x.Projects)),
		AuthorContacts: make([]AuthorContact, 0, len(x.AuthorContacts)),
		Entertain: Entertain{
			Live:   make([]Media, 0, len(x.Entertain.Live)),
			Videos: make([]Media, 0, len(x.Entertain.Videos)),
			Music:  make([]Media, 0, len(x.Entertain.Music)),
		},
		Menu:      make([]MenuEntry, 0, len(x.Menu)),
		Playables: make([]Playable, 0, len(x.Playables)),
	}
	for _, p := range x.Posts {
		data.Posts = append(data.Posts, PostMetadata{
			Id:           p.Id,
			Href:         p.Href,
			Title:        p.Title,
			Description:  p.Description,
			LastModified: p.LastModified,
			Creation:     p.Creation,
			Tags:         parseCommaSeparated(p.Tags),
		})
	}
	for _, p := range x.Projects {
		data.Projects = append(data.Projects, Project{
			Id:          p.Id,
			Name:        p.Name,
			Description: p.Description,
			URL:         p.URL,
			Tech:        parseCommaSeparated(p.Tech),
		})
	}
	for _, c := range x.AuthorContacts {
		data.AuthorContacts = append(data.AuthorContacts, AuthorContact{
			Id:    c.Id,
			Kind:  c.Kind,
			Label: c.Label,
			URL:   c.URL,
		})
	}
	for _, m := range x.Entertain.Live {
		data.Entertain.Live = append(data.Entertain.Live, m.toMedia())
	}
	for _, m := range x.Entertain.Videos {
		data.Entertain.Videos = append(data.Entertain.Videos, m.toMedia())
	}
	for _, m := range x.Entertain.Music {
		data.Entertain.Music = append(data.Entertain.Music, m.toMedia())
	}
	for _, e := range x.Menu {
		data.Menu = append(data.Menu, e.toMenuEntry())
	}
	for _, p := range x.Playables {
		data.Playables = append(data.Playables, p.toPlayable())
	}
	return data
}

func (m mediaXML) toMedia() Media {
	return Media{
		Id:          m.Id,
		Name:        m.Name,
		DisplayName: m.DisplayName,
		Description: m.Description,
		Thumbnail:   m.Thumbnail,
		Href:        m.Href,
	}
}

func (p playableXML) toPlayable() Playable {
	return Playable{
		Id:      p.Id,
		MediaId: p.MediaId,
		Type:    PlayableType(p.Type),
		URL:     p.URL,
	}
}

func (e menuEntryXML) toMenuEntry() MenuEntry {
	entry := MenuEntry{
		Id:            e.Id,
		Name:          e.Name,
		DisplayName:   e.DisplayName,
		Description:   e.Description,
		IconClassName: e.IconClassName,
	}
	if len(e.I18nDisplayNames) > 0 {
		entry.I18nDisplayNames = make(map[string]string, len(e.I18nDisplayNames))
		for _, d := range e.I18nDisplayNames {
			entry.I18nDisplayNames[d.Key] = d.Value
		}
	}
	return entry
}

// parseCommaSeparated parses a comma-separated attribute (a project's tech,
// a post's tags) into a list. An empty string yields a nil (empty) list;
// surrounding whitespace is ignored.
func parseCommaSeparated(s string) []string {
	var list []string
	for part := range strings.SplitSeq(s, ",") {
		if t := strings.TrimSpace(part); t != "" {
			list = append(list, t)
		}
	}
	return list
}
