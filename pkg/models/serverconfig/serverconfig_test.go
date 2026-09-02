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
