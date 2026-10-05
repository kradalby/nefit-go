package xmpp

import (
	"bytes"
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

type chunks struct {
	io.Reader
	n int
}

func (r chunks) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(len(p), r.n)]) }
func TestTypedCapturedCorpus(t *testing.T) {
	raw, err := os.ReadFile("../protocol/testdata/xmpp/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest []struct{ File, Kind string }
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range manifest {
		t.Run(fixture.File, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../protocol/testdata/xmpp", fixture.File))
			if err != nil {
				t.Fatal(err)
			}
			var baseline Element
			if err := xml.Unmarshal(raw, &baseline); err != nil {
				t.Fatal(err)
			}
			for _, size := range []int{1, 7, 4096} {
				f, err := NewReader(chunks{bytes.NewReader(raw), size}).Next()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(&baseline, f.Element) {
					t.Fatalf("stream decode changed data at read size %d", size)
				}
			}
			generic, err := xml.Marshal(baseline)
			if err != nil {
				t.Fatal(err)
			}
			var preserved Element
			if err := xml.Unmarshal(generic, &preserved); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(baseline, preserved) {
				t.Fatal("generic XML round trip changed captured semantics")
			}
			typed, err := baseline.Typed()
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := xml.Marshal(typed)
			if err != nil {
				t.Fatal(err)
			}
			var element Element
			if err := xml.Unmarshal(encoded, &element); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(canonicalElement(baseline), canonicalElement(element)) {
				t.Fatalf("typed encoding lost captured XML semantics: %T\noriginal: %#v\nencoded: %#v", typed, baseline, element)
			}
			again, err := element.Typed()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(typed, again) {
				t.Fatalf("typed round trip changed stanza: %T", typed)
			}
		})
	}
}

func canonicalElement(e Element) Element {
	e.Attr = slices.Clone(e.Attr)
	e.Children = slices.Clone(e.Children)
	slices.SortFunc(e.Attr, func(a, b xml.Attr) int {
		return strings.Compare(a.Name.Space+":"+a.Name.Local, b.Name.Space+":"+b.Name.Local)
	})
	// Whitespace between children in element-only protocol containers is not
	// stanza content. HTTP body text and mixed extension content remain exact.
	hasElement, hasText := false, false
	for _, n := range e.Children {
		hasElement = hasElement || n.Element != nil
		hasText = hasText || strings.TrimSpace(n.Text) != ""
	}
	if hasElement && !hasText {
		e.Children = slices.DeleteFunc(e.Children, func(n Node) bool { return n.Element == nil })
	}
	for i, n := range e.Children {
		if n.Element != nil {
			child := canonicalElement(*n.Element)
			e.Children[i].Element = &child
		}
	}
	// XMPP features advertise a set of capabilities; their order has no meaning.
	if e.Name == (xml.Name{Space: StreamNS, Local: "features"}) {
		slices.SortFunc(e.Children, func(a, b Node) int {
			return strings.Compare(a.Element.Name.Space+":"+a.Element.Name.Local, b.Element.Name.Space+":"+b.Element.Name.Local)
		})
	}
	return e
}

func TestStreamsRestartWithoutLosingBufferedData(t *testing.T) {
	var data bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := WriteFrame(&data, Frame{Stream: &Stream{From: "gateway", To: "domain", Version: "1.0"}}); err != nil {
			t.Fatal(err)
		}
		e := E(StreamNS, "features", E(BindNS, "bind"))
		if err := WriteFrame(&data, Frame{Element: &e}); err != nil {
			t.Fatal(err)
		}
	}
	r := NewReader(&data)
	for i := 0; i < 2; i++ {
		header, err := r.Next()
		if err != nil || header.Stream == nil {
			t.Fatal("missing restart", err)
		}
		frame, err := r.Next()
		if err != nil || frame.Element.Name != (xml.Name{Space: StreamNS, Local: "features"}) {
			t.Fatal("lost buffered stanza", err)
		}
	}
}

func TestUnknownExtensionsAndEscaping(t *testing.T) {
	source := []byte(`<message xmlns="jabber:client" from="a&amp;b" to="c" custom="x"><body>GET /x?a=1&amp;b=2 HTTP/1.1&#13;&#10;&#13;&#10;</body><origin-id xmlns="urn:xmpp:sid:0" id="1"/><extra xmlns="urn:example" flag="&quot;">before<child/>after</extra></message>`)
	var e Element
	if err := xml.Unmarshal(source, &e); err != nil {
		t.Fatal(err)
	}
	v, err := e.Typed()
	if err != nil {
		t.Fatal(err)
	}
	m := v.(*Message)
	if len(m.Extensions) != 2 || len(m.Attr) != 1 || m.From != "a&b" {
		t.Fatal("extensions or escaping lost")
	}
	m.Body.Text += "<&]]>"
	raw, err := xml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var again Message
	if err := xml.Unmarshal(raw, &again); err != nil {
		t.Fatal(err)
	}
	// Standard character references preserve the typed body line endings.
	if !reflect.DeepEqual(m, &again) {
		t.Fatal("CDATA or extension round trip changed data")
	}
}

func TestMalformedStreamRejected(t *testing.T) {
	for _, text := range []string{`<message xmlns="jabber:client"><body>x</message>`, `<undeclared:message/>`, `<!DOCTYPE x><message/>`, `<message><body>x`, `<message><?other x?></message>`} {
		t.Run(text, func(t *testing.T) {
			if _, err := NewReader(strings.NewReader(text)).Next(); err == nil {
				t.Fatal("malformed XML accepted")
			}
		})
	}
}

func FuzzReader(f *testing.F) {
	f.Add([]byte(`<message xmlns="jabber:client"><body>x</body></message>`))
	f.Add([]byte(`<stream:stream xmlns:stream="http://etherx.jabber.org/streams" xmlns="jabber:client">`))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > MaxStanzaBytes {
			return
		}
		r := NewReader(bytes.NewReader(raw))
		for i := 0; i < 8; i++ {
			frame, err := r.Next()
			if err != nil {
				return
			}
			if frame.Element != nil {
				if _, err := xml.Marshal(frame.Element); err != nil {
					t.Fatal(err)
				}
			}
		}
	})
}

func TestDeviceLexicalCompatibilityPreservesText(t *testing.T) {
	input := []byte(`<message xmlns="jabber:client" custom="line&#xA;break"><body>GET /a?x=1&amp;y=2 HTTP/1.1&#xD;&#xA;&#xD;&#xA;&lt;iq&gt;&lt;/iq&gt;</body><origin-id xmlns="urn:xmpp:sid:0"></origin-id></message>`)
	output, err := DeviceXML(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte("HTTP/1.1\r\n\r\n")) || !bytes.Contains(output, []byte(`custom="line&#xA;break"`)) || !bytes.Contains(output, []byte(`&lt;iq&gt;&lt;/iq&gt;`)) {
		t.Fatal("lexical compatibility changed non-newline content")
	}
	var message Message
	if err := xml.Unmarshal(output, &message); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(message.Body.Text, "<iq></iq>") || len(message.Extensions) != 1 {
		t.Fatal("text or extensions changed")
	}
	raw := []byte(`<message><body><![CDATA[<iq></iq>]]></body><iq></iq></message>`)
	compact, err := CompactEmpty(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(compact, []byte(`<![CDATA[<iq></iq>]]>`)) || !bytes.Contains(compact, []byte(`<iq/>`)) {
		t.Fatal("empty-element compaction changed CDATA")
	}
}

func TestStreamCloseFrame(t *testing.T) {
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, Frame{Stream: &Stream{Version: "1.0"}}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buffer, Frame{End: true}); err != nil {
		t.Fatal(err)
	}
	reader := NewReader(&buffer)
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	end, err := reader.Next()
	if err != nil || !end.End {
		t.Fatal("stream close was not marshalled correctly", err)
	}
}
