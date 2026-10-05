package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type captureFixture struct {
	File     string `json:"file"`
	Kind     string `json:"kind"`
	SHA256   string `json:"sha256"`
	Expected struct {
		Namespace  string `json:"namespace"`
		BodySHA256 string `json:"body_sha256"`
		StatusCode int    `json:"status_code"`
	} `json:"expected"`
}

// chunkReader makes XML tokens cross arbitrary TCP read boundaries.
type chunkReader struct {
	io.Reader
	size int
}

func (r chunkReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(len(p), r.size)]) }

// semanticTokens ignores namespace declaration spelling and coalesces text.
// Attribute order, prefixes and empty-element spelling are not XML semantics.
func semanticTokens(t *testing.T, r io.Reader) []xml.Token {
	t.Helper()
	d := xml.NewDecoder(r)
	var tokens []xml.Token
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		token = xml.CopyToken(token)
		switch v := token.(type) {
		case xml.StartElement:
			v.Attr = slices.DeleteFunc(v.Attr, func(a xml.Attr) bool { return a.Name.Space == "xmlns" || a.Name.Local == "xmlns" })
			slices.SortFunc(v.Attr, func(a, b xml.Attr) int {
				return strings.Compare(a.Name.Space+":"+a.Name.Local, b.Name.Space+":"+b.Name.Local)
			})
			token = v
		case xml.CharData:
			if len(tokens) > 0 {
				if previous, ok := tokens[len(tokens)-1].(xml.CharData); ok {
					tokens[len(tokens)-1] = append(previous, v...)
					continue
				}
			}
		}
		tokens = append(tokens, token)
	}
	return tokens
}

func TestCapturedXMLCorpus(t *testing.T) {
	manifest, err := os.ReadFile("testdata/xmpp/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []captureFixture
	if err = json.Unmarshal(manifest, &fixtures); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, fixture := range fixtures {
		counts[fixture.Kind]++
		t.Run(fixture.File, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata/xmpp", fixture.File))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != fixture.SHA256 {
				t.Fatal("fixture checksum changed")
			}
			original := semanticTokens(t, bytes.NewReader(raw))
			start := original[0].(xml.StartElement)
			if start.Name.Space != fixture.Expected.Namespace {
				t.Fatal("wrong namespace")
			}
			for _, size := range []int{1, 7, 4096} {
				tokens := semanticTokens(t, chunkReader{bytes.NewReader(raw), size})
				if !reflect.DeepEqual(original, tokens) {
					t.Fatalf("decode depends on read size %d", size)
				}
			}
			// Encode typed XML tokens, then decode again. No raw XML or innerxml injection.
			var wire bytes.Buffer
			encoder := xml.NewEncoder(&wire)
			for _, token := range original {
				if err := encoder.EncodeToken(token); err != nil {
					t.Fatal(err)
				}
			}
			if err := encoder.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := semanticTokens(t, &wire); !reflect.DeepEqual(original, got) {
				t.Fatal("XML semantic round trip changed tokens")
			}
			if fixture.Kind == "message" {
				var stanza MessageStanza
				if err := xml.Unmarshal(raw, &stanza); err != nil {
					t.Fatal(err)
				}
				sum := sha256.Sum256([]byte(stanza.Body))
				if hex.EncodeToString(sum[:]) != fixture.Expected.BodySHA256 {
					t.Fatal("body differs from independently extracted fixture")
				}
				encoded, err := xml.Marshal(stanza)
				if err != nil {
					t.Fatal(err)
				}
				var decoded MessageStanza
				if err := xml.Unmarshal(encoded, &decoded); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(stanza, decoded) {
					t.Fatal("typed message round trip changed known fields")
				}
				if fixture.Expected.StatusCode != 0 {
					response, err := ParseHTTPResponse(stanza.Body)
					if err != nil {
						t.Fatal(err)
					}
					if response.StatusCode != fixture.Expected.StatusCode {
						t.Fatal("incorrect HTTP status")
					}
				}
			}
			if fixture.Kind == "response" || fixture.Kind == "challenge" || fixture.Kind == "success" {
				var sasl struct {
					Text string `xml:",chardata"`
				}
				if err := xml.Unmarshal(raw, &sasl); err != nil {
					t.Fatal(err)
				}
				if sasl.Text != "" {
					if _, err := base64.StdEncoding.DecodeString(sasl.Text); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
	for _, kind := range []string{"stream:features", "auth", "challenge", "response", "success", "iq", "presence", "message"} {
		if counts[kind] == 0 {
			t.Errorf("missing capture coverage: %s", kind)
		}
	}
}

func TestCapturedStreamHeaders(t *testing.T) {
	paths, err := filepath.Glob("testdata/xmpp/*.open")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("missing stream headers")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d := xml.NewDecoder(chunkReader{bytes.NewReader(raw), 1})
			token, err := d.Token()
			if err != nil {
				t.Fatal(err)
			}
			start, ok := token.(xml.StartElement)
			if !ok || start.Name != (xml.Name{Space: "http://etherx.jabber.org/streams", Local: "stream"}) {
				t.Fatal("incorrect stream namespace")
			}
			// A stream header deliberately has no closing tag. EOF here is incomplete
			// framing, not a standalone XML document that can be xml.Unmarshal'ed.
			if _, err = d.Token(); err == nil || err == io.EOF {
				t.Fatal("unclosed stream must remain incomplete")
			}
		})
	}
}

func TestCapturedTruncatedStanzas(t *testing.T) {
	paths, err := filepath.Glob("testdata/xmpp/*.xml")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d := xml.NewDecoder(bytes.NewReader(raw[:len(raw)-1]))
			for {
				_, err = d.Token()
				if err != nil {
					break
				}
			}
			if err == io.EOF {
				t.Fatal("truncated stanza accepted as complete")
			}
		})
	}
}
