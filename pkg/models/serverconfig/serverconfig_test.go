package serverconfig

// The parser's tests for the music bot's songbook: the <musicBot/>
// element's attributes (flattened from the embedded bot client section)
// and its audioSource children — the base64 inline data, the XSD
// defaults encoding/xml cannot apply, the relative-url resolution, and
// the validation of the converted model.

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// parseMusicBot parses a configuration document consisting of the given
// <musicBot/> body.
func parseMusicBot(t *testing.T, body string) *MusicBotXML {
	t.Helper()
	doc := "<serverConfig>" + body + "</serverConfig>"
	path := filepath.Join(t.TempDir(), "serverConfig.xml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatalf("LoadServerConfig: %v", err)
	}
	if cfg.MusicBot == nil {
		t.Fatal("the document has no musicBot element")
	}
	return cfg.MusicBot
}

func TestMusicBotParsesBotClientAttributes(t *testing.T) {
	mb := parseMusicBot(t, `<musicBot url="ws://localhost:3000/api/ss/ws" jwt="t"
    subscriberId="musicbot" keepAliveInterval="5s"/>`)
	// The attributes live on the embedded bot client section; the
	// flattening must surface them on the music bot type.
	if mb.URL != "ws://localhost:3000/api/ss/ws" || mb.JWT != "t" ||
		mb.SubscriberId != "musicbot" || mb.KeepAliveInterval != "5s" {
		t.Fatalf("the parsed music bot = %+v", mb.BotClientXML)
	}
	if len(mb.AudioSources) != 0 {
		t.Fatalf("the music bot carries %d audio sources, want none", len(mb.AudioSources))
	}
}

func TestAudioSourceInlineData(t *testing.T) {
	data := []byte{0x00, 0x7F, 0x80, 0xFF, 0x2A}
	// The base64 wraps across lines — the lexical space of the schema's
	// base64Binary allows embedded whitespace.
	encoded := base64.StdEncoding.EncodeToString(data)
	wrapped := encoded[:4] + "\n    " + encoded[4:]
	mb := parseMusicBot(t, `<musicBot url="wss://x/api/ss/ws" jwt="t">
  <audioSource id="s1" name="chiptune" sampleFormatType="mu_law" bitDepth="8"
    numericType="unsigned_integer" numChannels="1" sampleRate="8000"
    numTotalSamples="5">`+wrapped+`</audioSource>
</musicBot>`)

	src, err := mb.AudioSources[0].AudioSourceData("/cfg")
	if err != nil {
		t.Fatalf("AudioSourceData: %v", err)
	}
	if string(src.InlineData) != string(data) {
		t.Fatalf("the inline data = %v, want %v", src.InlineData, data)
	}
	// The XSD's defaults, applied by the conversion: compression none,
	// interleaved true.
	if src.Compression != "none" || !src.Interleaved {
		t.Fatalf("the defaults = %q/%v", src.Compression, src.Interleaved)
	}
}

func TestAudioSourceURLResolution(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"a relative path joins the config dir", "assets/chiptune.ulaw", "/cfg/assets/chiptune.ulaw"},
		{"an absolute path stands", "/srv/songs/chiptune.ulaw", "/srv/songs/chiptune.ulaw"},
		{"an http url stands", "http://example.com/song.flac", "http://example.com/song.flac"},
		{"an https url stands", "https://example.com/song.flac", "https://example.com/song.flac"},
	} {
		mb := parseMusicBot(t, `<musicBot url="wss://x/api/ss/ws" jwt="t">
  <audioSource id="s" name="s" url="`+tc.url+`" sampleFormatType="mu_law" bitDepth="8"
    numericType="unsigned_integer" numChannels="1" sampleRate="8000" numTotalSamples="1"/>
</musicBot>`)
		src, err := mb.AudioSources[0].AudioSourceData("/cfg")
		if err != nil {
			t.Fatalf("%s: AudioSourceData: %v", tc.name, err)
		}
		if src.URL != tc.want {
			t.Errorf("%s: the url = %q, want %q", tc.name, src.URL, tc.want)
		}
	}
}

func TestAudioSourceConversionValidates(t *testing.T) {
	// An entry the model rejects (an unsupported combination) is an
	// error naming the entry.
	mb := parseMusicBot(t, `<musicBot url="wss://x/api/ss/ws" jwt="t">
  <audioSource id="s" name="s" url="song.ulaw" sampleFormatType="mu_law" bitDepth="8"
    numericType="unsigned_integer" numChannels="2" sampleRate="8000" numTotalSamples="1"/>
</musicBot>`)
	_, err := mb.AudioSources[0].AudioSourceData("/cfg")
	if err == nil || !strings.Contains(err.Error(), "audioSource s:") {
		t.Fatalf("AudioSourceData error = %v, want one naming the entry and its cause", err)
	}
}

// parseSipBot parses a configuration document consisting of the given
// <sipBot/> body.
func parseSipBot(t *testing.T, body string) *SipBotXML {
	t.Helper()
	doc := "<serverConfig>" + body + "</serverConfig>"
	path := filepath.Join(t.TempDir(), "serverConfig.xml")
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	cfg, err := LoadServerConfig(path)
	if err != nil {
		t.Fatalf("LoadServerConfig: %v", err)
	}
	if cfg.SipBot == nil {
		t.Fatal("the document has no sipBot element")
	}
	return cfg.SipBot
}

// TestSipBotParsesYellowPage covers the <sipBot/> element's
// <yellowPage/> child: the sections and their contacts — the optional
// description defaulting to empty — and the absent page.
func TestSipBotParsesYellowPage(t *testing.T) {
	sb := parseSipBot(t, `<sipBot url="wss://x/api/ss/ws" jwt="t">
  <yellowPage>
    <section id="local" name="local">
      <contact id="echo" name="echo test" aor="9196"/>
      <contact id="echo-delayed" name="delayed echo test" aor="9195" description="echoes back after 250ms"/>
    </section>
    <section id="pbx" name="the PBX"/>
  </yellowPage>
</sipBot>`)
	if sb.YellowPage == nil {
		t.Fatal("the sip bot carries no yellow page")
	}
	sections := sb.YellowPage.Sections
	if len(sections) != 2 {
		t.Fatalf("the yellow page has %d sections, want 2", len(sections))
	}
	if sections[0].ID != "local" || sections[0].Name != "local" {
		t.Fatalf("section[0] = %+v", sections[0])
	}
	contacts := sections[0].Contacts
	if len(contacts) != 2 {
		t.Fatalf("section[0] has %d contacts, want 2", len(contacts))
	}
	if contacts[0].ID != "echo" || contacts[0].Name != "echo test" ||
		contacts[0].AOR != "9196" || contacts[0].Description != "" {
		t.Fatalf("contact[0] = %+v", contacts[0])
	}
	if contacts[1].Description != "echoes back after 250ms" {
		t.Fatalf("contact[1] = %+v", contacts[1])
	}
	if sections[1].Name != "the PBX" || len(sections[1].Contacts) != 0 {
		t.Fatalf("section[1] = %+v", sections[1])
	}

	// No <yellowPage/> child: the page stays nil.
	if sb := parseSipBot(t, `<sipBot url="wss://x/api/ss/ws" jwt="t"/>`); sb.YellowPage != nil {
		t.Fatalf("no yellowPage element, parsed = %+v", sb.YellowPage)
	}
}
