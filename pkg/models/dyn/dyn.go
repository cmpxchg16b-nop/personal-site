// Package dyn models the site's dynamic blog data — the project and
// author-contact lists the frontend renders — and how that data is provided
// to the API layer (see pkg/api/dyn, which serves it under /api/dyn/).
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

// DynBlogData is the whole of the site's dynamic blog data: everything the
// <dynBlogData/> section of the server configuration document carries.
type DynBlogData struct {
	Posts          []PostMetadata  `json:"posts"`
	Projects       []Project       `json:"projects"`
	AuthorContacts []AuthorContact `json:"authorContacts"`
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

func (x dynBlogDataXML) toDynBlogData() *DynBlogData {
	data := &DynBlogData{
		Posts:          make([]PostMetadata, 0, len(x.Posts)),
		Projects:       make([]Project, 0, len(x.Projects)),
		AuthorContacts: make([]AuthorContact, 0, len(x.AuthorContacts)),
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
	return data
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
