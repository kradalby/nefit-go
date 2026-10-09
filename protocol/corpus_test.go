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
	"regexp"
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

func TestCapturedXMLCorpus(t *testing.T) {
	manifest, err := os.ReadFile("testdata/xmpp/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []captureFixture
	if err = json.Unmarshal(manifest, &fixtures); err != nil {
		t.Fatal(err)
	}
	counts, indexed := map[string]int{}, map[string]bool{}
	for _, fixture := range fixtures {
		counts[fixture.Kind]++
		if !fixtureName.MatchString(fixture.File) || indexed[fixture.File] {
			t.Errorf("invalid or duplicate manifest file %q", fixture.File)
		}
		indexed[fixture.File] = true
		t.Run(fixture.File, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata/xmpp", fixture.File))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(raw)
			if hex.EncodeToString(sum[:]) != fixture.SHA256 {
				t.Fatal("fixture checksum changed")
			}
			var root struct{ XMLName xml.Name }
			if err := xml.Unmarshal(raw, &root); err != nil {
				t.Fatal(err)
			}
			if root.XMLName.Space != fixture.Expected.Namespace {
				t.Fatal("wrong namespace")
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
	paths, err := filepath.Glob("testdata/xmpp/*.xml")
	if err != nil || len(paths) != len(fixtures) {
		t.Fatalf("%d fixture files, %d indexed: %v", len(paths), len(fixtures), err)
	}
	for _, path := range paths {
		if !indexed[filepath.Base(path)] {
			t.Errorf("%s is not indexed by manifest.json", path)
		}
	}
}

// fixtureName matches what the generator writes; anything else could escape
// testdata or be a stale, unreviewed file.
var fixtureName = regexp.MustCompile(`^[0-9]{4}-[a-z-]+\.xml$`)

// TestCapturedBodiesAreZeroed keeps a ciphertext leak visible to go test alone:
// every body payload must be absent or base64 zero bytes, whatever its length.
func TestCapturedBodiesAreZeroed(t *testing.T) {
	paths, err := filepath.Glob("testdata/xmpp/*.xml")
	if err != nil {
		t.Fatal(err)
	}
	request := regexp.MustCompile(`^(?:HTTP/|[A-Za-z]+\s+\S*\s*HTTP/)`)
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			d := xml.NewDecoder(bytes.NewReader(raw))
			var body *strings.Builder
			for {
				token, err := d.Token()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				switch v := token.(type) {
				case xml.StartElement:
					if v.Name.Local == "body" {
						body = &strings.Builder{}
					}
				case xml.CharData:
					if body != nil {
						body.Write(v)
					}
				case xml.EndElement:
					if v.Name.Local != "body" {
						continue
					}
					payload := strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(body.String())
					if request.MatchString(strings.TrimSpace(payload)) {
						var found bool
						if _, payload, found = strings.Cut(payload, "\n\n"); !found {
							t.Fatal("HTTP body without header separator")
						}
					}
					decoded, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(payload), ""))
					if err != nil || slices.ContainsFunc(decoded, func(b byte) bool { return b != 0 }) {
						t.Fatal("body payload is not base64 zero bytes")
					}
					body = nil
				}
			}
		})
	}
}
