package xmpp

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
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
	raw, err := os.ReadFile("../../protocol/testdata/xmpp/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest []struct{ File, Kind string }
	if err = json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range manifest {
		t.Run(fixture.File, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("../../protocol/testdata/xmpp", fixture.File))
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
				if !reflect.DeepEqual(canonicalElement(baseline), canonicalElement(*f.Element)) {
					t.Fatalf("stream decode changed data at read size %d", size)
				}
			}
			if _, err := NewReader(bytes.NewReader(raw[:len(raw)-1])).Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("truncated fixture = %v, want unexpected EOF", err)
			}
			generic, err := xml.Marshal(baseline)
			if err != nil {
				t.Fatal(err)
			}
			var preserved Element
			if err := xml.Unmarshal(generic, &preserved); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(canonicalElement(baseline), canonicalElement(preserved)) {
				t.Fatal("generic XML round trip changed captured semantics")
			}
			for _, cloud := range []bool{false, true} {
				encoded, err := Marshal(Frame{Element: &baseline}, !cloud)
				if err != nil {
					t.Fatal(err)
				}
				var stream bytes.Buffer
				if err = WriteFrame(&stream, Frame{Stream: &Stream{}}); err != nil {
					t.Fatal(err)
				}
				stream.Write(encoded)
				r := NewReader(&stream)
				if _, err = r.Next(); err != nil {
					t.Fatal(err)
				}
				frame, err := r.Next()
				if err != nil {
					t.Fatal(err)
				}
				expected := canonicalElement(baseline)
				if !cloud {
					for i := range expected.Children {
						body := expected.Children[i].Element
						if expected.Name == (xml.Name{Space: ClientNS, Local: "message"}) && body != nil && body.Name == (xml.Name{Space: ClientNS, Local: "body"}) && isHTTPBody(body.Text()) {
							body.Children = []Node{{Text: strings.ReplaceAll(strings.ReplaceAll(body.Text(), "\r\n", "\n"), "\r", "\n")}}
						}
					}
				}
				if !reflect.DeepEqual(expected, canonicalElement(*frame.Element)) {
					t.Fatalf("stream encoder changed captured semantics (cloud=%t)", cloud)
				}
				// The device reads HTTP line ends literally; other text keeps CR
				// as a character reference. Decided from the capture, not the encoder.
				if body := baseline.Child(ClientNS, "body"); !cloud && baseline.Name.Local == "message" && body != nil && strings.ContainsAny(body.Text(), "\r\n") {
					text := body.Text()
					literal := strings.HasPrefix(text, "HTTP/") || strings.HasPrefix(text, "GET ") || strings.HasPrefix(text, "PUT ")
					if literal != bytes.Contains(encoded, []byte("\r\n")) || !literal && bytes.Contains(encoded, []byte("\r")) {
						t.Fatalf("device form of %q kept the wrong line ends:\n%s", text[:min(len(text), 20)], encoded)
					}
				}
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
	// Namespace scope is decoder metadata; QName meanings are checked by
	// TestQNameContext rather than the shape of the shared scope chain.
	e.namespaces = nil
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
		key := func(n Node) string {
			if n.Element == nil {
				return "text:" + n.Text
			}
			return n.Element.Name.Space + ":" + n.Element.Name.Local
		}
		slices.SortFunc(e.Children, func(a, b Node) int { return strings.Compare(key(a), key(b)) })
	}
	return e
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
	for _, text := range []string{
		`<message xmlns="jabber:client"><body>x</message>`,
		`<message xmlns="jabber:client"><body>x</message></body>`,
		`<undeclared:message/>`,
		`<!DOCTYPE x><message/>`,
		`<message><body>x`,
		`<message><?other x?></message>`,
		`<?other x?><message/>`,
		`text<message/>`,
		"\u00a0<message/>",
		`<!-- comment --><message xmlns="jabber:client"/>`,
		`<message xmlns="jabber:client"><!-- comment --></message>`,
		// Names encoding/xml accepts but no namespace-aware peer could re-read.
		`<message xmlns="jabber:client"><p:0 xmlns:p="urn:x"/></message>`,
		`<p:-x xmlns:p="urn:x"/>`,
		`<message xmlns="jabber:client" xmlns:a=""/>`,
		`<message xmlns="jabber:client" xmlns:xml="urn:bad"/>`,
		`<message xmlns="jabber:client" xmlns:xmlns="urn:x"/>`,
		`<message xmlns="jabber:client" xmlns:p="http://www.w3.org/XML/1998/namespace"/>`,
		`<message xmlns="http://www.w3.org/2000/xmlns/"/>`,
		`<message xmlns="jabber:client" xmlns:p="http://www.w3.org/2000/xmlns/"/>`,
		`<message xmlns="jabber:client"><a:b:c xmlns:a="urn:x"/></message>`,
		`<presence xmlns="jabber:client"><xml:x/></presence>`,
		`<message xmlns="jabber:client"><!DOCTYPE x></message>`,
		`<message xmlns="http://www.w3.org/XML/1998/namespace"/>`,
	} {
		t.Run(text, func(t *testing.T) {
			if _, err := NewReader(strings.NewReader(text)).Next(); err == nil {
				t.Fatal("malformed XML accepted")
			}
		})
	}
}

func TestAttributeWhitespaceNormalised(t *testing.T) {
	f, err := NewReader(strings.NewReader("<iq xmlns=\"jabber:client\" type=\"get\" id=\"a\tb\nc\"/>")).Next()
	if err != nil {
		t.Fatal(err)
	}
	// A conforming peer reads the id with spaces; a reply must echo that.
	if id := f.Element.Get("id"); id != "a b c" {
		t.Fatalf("id = %q", id)
	}
	f, err = NewReader(strings.NewReader("<message xmlns=\"jabber:client\" xmlns:p=\"urn:\tx\" p:a=\"1\"/>")).Next()
	if err != nil {
		t.Fatal(err)
	}
	if f.Element.Attr[0].Name.Space != "urn: x" {
		t.Fatalf("namespace = %q", f.Element.Attr[0].Name.Space)
	}
	// Two declarations equal once normalised make a duplicate attribute.
	if _, err := NewReader(strings.NewReader("<message xmlns=\"jabber:client\" xmlns:a=\"urn:x\n\" xmlns:b=\"urn:x \" a:to=\"1\" b:to=\"2\"/>")).Next(); err == nil {
		t.Fatal("duplicate attribute accepted")
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
	var input Element
	if err := xml.Unmarshal([]byte(`<message xmlns="jabber:client" custom="line&#xA;break"><body>GET /a?x=1&amp;y=2 HTTP/1.1&#xD;&#xA;&#xD;&#xA;&lt;iq&gt;&lt;/iq&gt;</body><origin-id xmlns="urn:xmpp:sid:0"></origin-id></message>`), &input); err != nil {
		t.Fatal(err)
	}
	output, err := Marshal(&input, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(output, []byte("HTTP/1.1\r\n\r\n")) || !bytes.Contains(output, []byte(`custom="line&#xA;break"`)) || !bytes.Contains(output, []byte(`&lt;iq&gt;&lt;/iq&gt;`)) {
		t.Fatal("lexical compatibility changed non-newline content")
	}
	var message Message
	decoder := xml.NewDecoder(bytes.NewReader(output))
	decoder.DefaultSpace = ClientNS
	if err := decoder.Decode(&message); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(message.Body.Text, "<iq></iq>") || len(message.Extensions) != 1 {
		t.Fatal("text or extensions changed")
	}
	raw := []byte(`<message><body><![CDATA[<iq></iq>]]></body><iq></iq></message>`)
	compact, err := compactEmpty(raw)
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
	if err := WriteFrame(&buffer, Frame{End: true}); err != nil || !strings.HasSuffix(buffer.String(), "</stream:stream>") {
		t.Fatalf("stream close = %q, %v", buffer.String(), err)
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

func TestTruncationIsNotAClose(t *testing.T) {
	stanza := `<message xmlns="jabber:client" to="a"><body>x</body></message>`
	for cut := 1; cut < len(stanza); cut++ {
		if _, err := NewReader(strings.NewReader(stanza[:cut])).Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("stanza cut at %d = %v, want unexpected EOF", cut, err)
		}
	}
	r := NewReader(strings.NewReader(`<message xmlns="jabber:client"/>`))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("close between stanzas = %v, want EOF", err)
	}
}

func TestStanzaElementLimit(t *testing.T) {
	stanza := func(children int) string {
		return `<message xmlns="jabber:client">` + strings.Repeat(`<x/>`, children) + `</message>`
	}
	// The root and its xmlns attribute take two nodes.
	if _, err := NewReader(strings.NewReader(stanza(MaxStanzaNodes - 2))).Next(); err != nil {
		t.Fatalf("stanza at the node limit rejected: %v", err)
	}
	if _, err := NewReader(strings.NewReader(stanza(MaxStanzaNodes - 1))).Next(); err == nil {
		t.Fatal("stanza over the node limit accepted")
	}
	var e Element
	if err := xml.Unmarshal([]byte(stanza(MaxStanzaNodes-1)), &e); err == nil {
		t.Fatal("document over the node limit accepted")
	}
	var attrs strings.Builder
	for i := range MaxStanzaNodes {
		fmt.Fprintf(&attrs, ` a%d=""`, i)
	}
	if _, err := NewReader(strings.NewReader(`<message xmlns="jabber:client"` + attrs.String() + `/>`)).Next(); err == nil {
		t.Fatal("attribute-heavy stanza accepted")
	}
}

func TestReencodingIsBounded(t *testing.T) {
	// One long namespace is repeated on every element when re-encoded.
	space := "urn:" + strings.Repeat("n", 1000)
	e := E(ClientNS, "message")
	for range MaxStanzaNodes - 1 {
		e.Children = append(e.Children, Node{Element: &Element{Name: xml.Name{Space: space, Local: "x"}}})
	}
	if _, err := e.Typed(); err == nil {
		t.Fatal("amplified re-encoding accepted")
	}
}

func TestNamespacedAttributesSurviveReencoding(t *testing.T) {
	var deep strings.Builder
	for i := range 8 {
		deep.WriteString("<x")
		for j := range 10 {
			fmt.Fprintf(&deep, ` xmlns:p%d="http://x/%d/%d" p%d:a="1"`, j, i, j, j)
		}
		deep.WriteString(">")
	}
	deep.WriteString(strings.Repeat("</x>", 8))
	for name, raw := range map[string]string{
		"ancestor stream prefix": `<message xmlns="jabber:client" xmlns:a="http://x/stream" a:foo="1"><body>x</body><stream:error xmlns:stream="http://etherx.jabber.org/streams"><c a:bar="2"/></stream:error></message>`,
		"stream element":         `<stream:features xmlns:stream="http://etherx.jabber.org/streams" xmlns:a="http://example.com/stream" a:foo="1"/>`,
		"rebinding per element":  `<message xmlns="jabber:client">` + deep.String() + `</message>`,
		"prefix from URI":        `<message xmlns="jabber:client" xmlns:p="http://x/a·b" p:c="1"/>`,
	} {
		t.Run(name, func(t *testing.T) {
			frame, err := NewReader(strings.NewReader(raw)).Next()
			if err != nil {
				t.Fatal(err)
			}
			for _, device := range []bool{true, false} {
				encoded, err := Marshal(frame, device)
				if err != nil {
					t.Fatalf("device=%v: %v", device, err)
				}
				r := NewReader(strings.NewReader(`<stream:stream xmlns="jabber:client" xmlns:stream="` + StreamNS + `">` + string(encoded)))
				if _, err := r.Next(); err != nil {
					t.Fatal(err)
				}
				got, err := r.Next()
				if err != nil {
					t.Fatalf("device=%v: re-read failed: %v\n%s", device, err, encoded)
				}
				if !reflect.DeepEqual(canonicalElement(*got.Element), canonicalElement(*frame.Element)) {
					t.Fatalf("device=%v: namespaces changed\n%s", device, encoded)
				}
			}
		})
	}
}

func TestRestartMustDeclareStreamNamespace(t *testing.T) {
	r := NewReader(strings.NewReader(`<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams"><stream:stream xmlns="jabber:client">`))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); err == nil {
		t.Fatal("restart relying on the previous stream's declarations accepted")
	}
}

func TestCapturedStreamManifest(t *testing.T) {
	const directory = "../../protocol/testdata/xmpp"
	raw, err := os.ReadFile(filepath.Join(directory, "streams.json"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		File, Recording string
		Offset          int
	}
	if err = json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(directory, "*.open"))
	if err != nil || len(paths) != len(entries) || len(entries) == 0 {
		t.Fatal("stream manifest must cover every captured opening header", err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if filepath.Base(entry.File) != entry.File || seen[entry.File] || entry.Recording == "" || entry.Offset < 0 {
			t.Fatal("invalid stream provenance entry")
		}
		seen[entry.File] = true
		t.Run(entry.File, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(directory, entry.File))
			if err != nil {
				t.Fatal(err)
			}
			for _, size := range []int{1, 7, 4096} {
				// Consecutive open headers exercise authentication restarts while
				// leaving the same decoder and its buffered bytes in place.
				r := NewReader(chunks{bytes.NewReader(bytes.Repeat(raw, 2)), size})
				first, err := r.Next()
				if err != nil || first.Stream == nil {
					t.Fatalf("captured stream failed at chunk size %d: %v", size, err)
				}
				second, err := r.Next()
				if err != nil || !reflect.DeepEqual(first.Stream, second.Stream) {
					t.Fatalf("restart changed captured stream at chunk size %d: %v", size, err)
				}
				var out bytes.Buffer
				if err = WriteFrame(&out, first); err != nil {
					t.Fatal(err)
				}
				again, err := NewReader(&out).Next()
				if err != nil || !reflect.DeepEqual(first.Stream, again.Stream) {
					t.Fatalf("captured stream roundtrip changed header: %v", err)
				}
			}
		})
	}
}

// Wire framing and routing must stay correct for untrusted extension metadata.
func TestAttributeNamespaces(t *testing.T) {
	for _, name := range []string{"message", "iq", "presence", "auth"} {
		ns := ClientNS
		if name == "auth" {
			ns = SASLNS
		}
		for _, attrs := range []string{
			`to="device" from="phone" type="chat" id="real" mechanism="PLAIN" meta:to="other" meta:from="other" meta:type="other" meta:id="other" meta:mechanism="other"`,
			`meta:to="other" meta:from="other" meta:type="other" meta:id="other" meta:mechanism="other" to="device" from="phone" type="chat" id="real" mechanism="PLAIN"`,
			`meta:to="other" meta:from="other" meta:type="other" meta:id="other" meta:mechanism="other"`,
		} {
			raw := `<` + name + ` xmlns="` + ns + `" xmlns:meta="urn:metadata" xml:lang="nl" ` + attrs + `/>`
			frame, err := NewReader(strings.NewReader(raw)).Next()
			if err != nil {
				t.Fatal(err)
			}
			typed, err := frame.Element.Typed()
			if err != nil {
				t.Fatal(err)
			}
			var direct any
			switch name {
			case "message":
				direct = new(Message)
			case "iq":
				direct = new(IQ)
			case "presence":
				direct = new(Presence)
			case "auth":
				direct = new(SASL)
			}
			if err := xml.Unmarshal([]byte(raw), direct); err != nil {
				t.Fatal(err)
			}
			for _, v := range []any{typed, direct} {
				expected := frame.Element.Get("to")
				var to string
				var extra []xml.Attr
				switch x := v.(type) {
				case *Message:
					to, extra = x.To, x.Attr
					if x.Lang != "nl" {
						t.Fatal(x.Lang)
					}
				case *IQ:
					to, extra = x.To, x.Attr
				case *Presence:
					to, extra = x.To, x.Attr
				case *SASL:
					if x.Mechanism != frame.Element.Get("mechanism") {
						t.Fatalf("mechanism overridden: %+v", x)
					}
					extra = x.Attr
				}
				if to != expected && name != "auth" {
					t.Fatalf("routing overridden: %+v", v)
				}
				for _, key := range []string{"to", "from", "type", "id", "mechanism"} {
					found := false
					for _, a := range extra {
						if a.Name == (xml.Name{Space: "urn:metadata", Local: key}) {
							found = true
						}
					}
					if !found {
						t.Fatalf("lost %s extension attribute: %+v", key, v)
					}
				}
			}
		}
	}
	raw := `<stream:stream xmlns:stream="` + StreamNS + `" xmlns:meta="urn:metadata" xmlns:to="namespace-value" from="device" to="domain" meta:from="other" meta:to="other" meta:id="other" meta:version="other" meta:lang="other" xml:lang="nl">`
	frame, err := NewReader(strings.NewReader(raw)).Next()
	if err != nil {
		t.Fatal(err)
	}
	if frame.Stream.From != "device" || frame.Stream.To != "domain" || frame.Stream.ID != "" || frame.Stream.Version != "" || frame.Stream.Lang != "nl" {
		t.Fatalf("stream routing overridden: %+v", frame.Stream)
	}
}

func TestDuplicateAttributes(t *testing.T) {
	for _, raw := range []string{
		`<message xmlns="jabber:client" to="one" to="two"/>`,
		`<message xmlns="jabber:client" xmlns:a="urn:x" xmlns:b="urn:x" a:to="one" b:to="two"/>`,
		`<stream:stream xmlns:stream="` + StreamNS + `" from="one" from="two">`,
	} {
		if _, err := NewReader(strings.NewReader(raw)).Next(); err == nil {
			t.Fatal("duplicate accepted", raw)
		}
		if !strings.HasPrefix(raw, "<stream:") {
			var m Message
			if err := xml.Unmarshal([]byte(raw), &m); err == nil {
				t.Fatal("typed duplicate accepted", raw)
			}
		}
	}
}

type countingInput struct {
	reader *strings.Reader
	bytes  int
}

func (r *countingInput) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytes += n
	return n, err
}

func TestReaderBoundsTokensBeforeAllocation(t *testing.T) {
	large := strings.Repeat("x", 2*MaxStanzaBytes)
	for _, raw := range []string{
		`<message padding="` + large + `"/>`,
		`<stream:stream xmlns:stream="` + StreamNS + `" padding="` + large + `">`,
		strings.Repeat(" ", 2*MaxStanzaBytes) + `<presence/>`,
		`<message><body>` + large + `</body></message>`,
		`<message><![CDATA[` + large + `]]></message>`,
		`<!--` + large + `--><presence/>`,
		`<message padding="` + large,
		`<?xml ` + large,
	} {
		input := &countingInput{reader: strings.NewReader(raw)}
		if _, err := NewReader(input).Next(); err == nil {
			t.Fatal("oversized token accepted")
		}
		if input.bytes > MaxStanzaBytes+4096 {
			t.Fatalf("read %d before rejection", input.bytes)
		}
	}
	// Each frame has its own quota, including inter-stanza whitespace.
	reader := NewReader(strings.NewReader(strings.Repeat("\n<presence/>", MaxStanzaBytes/10)))
	for i := 0; i < MaxStanzaBytes/10; i++ {
		if _, err := reader.Next(); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
}

func TestCompactEmptyIdempotent(t *testing.T) {
	for _, raw := range []string{`<iq/>`, `<iq><x/></iq>`, `<iq a="b"></iq>`, `<message><body></body><x/></message>`} {
		first, err := compactEmpty([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		second, err := compactEmpty(first)
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Fatalf("not idempotent: %s, %s", first, second)
		}
		var element Element
		if err := xml.Unmarshal(second, &element); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFragmentedTextAllocationBound(t *testing.T) {
	fragments := MaxStanzaBytes / 16
	raw := `<message xmlns="jabber:client"><body>` + strings.Repeat(`<![CDATA[x]]>`, fragments) + `</body></message>`
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var element *Element
			if stream {
				frame, err := NewReader(strings.NewReader(raw)).Next()
				if err != nil {
					t.Fatal(err)
				}
				element = frame.Element
			} else {
				element = new(Element)
				if err := xml.Unmarshal([]byte(raw), element); err != nil {
					t.Fatal(err)
				}
			}
			runtime.ReadMemStats(&after)
			if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 64*MaxStanzaBytes {
				t.Fatalf("sub-quota stanza allocated %d bytes", allocated)
			}
			if text := element.Child(ClientNS, "body").Text(); text != strings.Repeat("x", fragments) {
				t.Fatal("fragmented text changed")
			}
		})
	}
}

func TestStanzaByteLimitBoundary(t *testing.T) {
	head, tail := `<message xmlns="jabber:client"><body>`, `</body></message>`
	for size, ok := range map[int]bool{MaxStanzaBytes: true, MaxStanzaBytes + 1: false} {
		raw := head + strings.Repeat("x", size-len(head)-len(tail)) + tail
		if _, err := NewReader(strings.NewReader(raw)).Next(); (err == nil) != ok {
			t.Errorf("%d-byte stanza: %v", size, err)
		}
	}
}

func namespaceDeclarations(count int, prefix string) string {
	var b strings.Builder
	for i := range count {
		fmt.Fprintf(&b, ` xmlns:%s%d="urn:%s%d"`, prefix, i, prefix, i)
	}
	return b.String()
}

func TestNamespaceLimit(t *testing.T) {
	for _, raw := range []string{
		`<message xmlns="jabber:client"` + namespaceDeclarations(MaxNamespaceBindings, "p") + `/>`,
		`<message xmlns="jabber:client"` + namespaceDeclarations(MaxNamespaceBindings-3, "p") + `><body` + namespaceDeclarations(3, "q") + `/></message>`,
	} {
		for name, decode := range map[string]func(string) error{
			"stream":   func(raw string) error { _, err := NewReader(strings.NewReader(raw)).Next(); return err },
			"document": func(raw string) error { var e Element; return xml.Unmarshal([]byte(raw), &e) },
		} {
			t.Run(name, func(t *testing.T) {
				if err := decode(raw); err == nil || !strings.Contains(err.Error(), "namespace limit") {
					t.Fatalf("expected bounded namespace rejection, got %v", err)
				}
			})
		}
	}
	// The xml prefix and the default namespace count, so this is the limit.
	if _, err := NewReader(strings.NewReader(`<message xmlns="jabber:client"` + namespaceDeclarations(MaxNamespaceBindings-2, "p") + `/>`)).Next(); err != nil {
		t.Fatal("bindings at the limit refused:", err)
	}
	stream := `<stream:stream xmlns="jabber:client" xmlns:stream="` + StreamNS + `"` + namespaceDeclarations(MaxNamespaceBindings, "p") + `>`
	if _, err := NewReader(strings.NewReader(stream)).Next(); err == nil {
		t.Fatal("accepted excessive namespaces on stream header")
	}
}

func TestNamespaceInheritanceDoesNotAllocate(t *testing.T) {
	parent := &namespaceScope{bindings: map[string]string{"xml": XMLNS, "": ClientNS}, count: MaxNamespaceBindings}
	for i := range MaxNamespaceBindings - len(parent.bindings) {
		parent.bindings[fmt.Sprintf("p%d", i)] = fmt.Sprintf("urn:p%d", i)
	}
	attrs := []xml.Attr{{Name: xml.Name{Local: "id"}, Value: "1"}}
	if allocations := testing.AllocsPerRun(100, func() {
		ns, err := scope(parent, attrs)
		value, ok := ns.lookup("p100")
		if err != nil || !ok || value != "urn:p100" {
			panic("lost inherited namespace")
		}
	}); allocations != 0 {
		t.Fatalf("inheriting unchanged namespaces allocates %v times", allocations)
	}
}

func TestNamespaceRebindingAllocationIsIndependentOfInheritedBindings(t *testing.T) {
	attrs := []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: "urn:inner"}}
	allocations := func(count int) float64 {
		parent := &namespaceScope{bindings: map[string]string{"": ClientNS}, count: count}
		for i := range count - 1 {
			parent.bindings[fmt.Sprintf("p%d", i)] = "urn:extension"
		}
		return testing.AllocsPerRun(100, func() {
			ns, err := scope(parent, attrs)
			value, ok := ns.lookup("")
			if err != nil || !ok || value != "urn:inner" {
				panic("lost namespace rebinding")
			}
		})
	}
	if small, large := allocations(1), allocations(MaxNamespaceBindings); large != small {
		t.Fatalf("namespace rebinding allocates %v times with full scope, %v with one binding", large, small)
	}
}

func TestNamespaceSiblingWorkBound(t *testing.T) {
	// The root and its declarations, then children of one node plus one attribute.
	siblings := (MaxStanzaNodes - 1 - (MaxNamespaceBindings - 1)) / 2
	raw := `<message xmlns="jabber:client"` + namespaceDeclarations(MaxNamespaceBindings-2, "p") + `>` +
		strings.Repeat(`<x xmlns=""/>`, siblings) + `</message>`
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	frame, err := NewReader(strings.NewReader(raw)).Next()
	runtime.ReadMemStats(&after)
	if err != nil || len(frame.Element.Children) != siblings {
		t.Fatalf("valid broad stanza rejected: %v", err)
	}
	// XML nodes cost memory; inherited namespace maps must not multiply it.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(len(raw))*64+1<<20 {
		t.Fatalf("namespace rebinding amplified %d input bytes into %d allocated bytes", len(raw), allocated)
	}
}

func TestNamespaceRebindingAndRestart(t *testing.T) {
	raw := `<stream:stream xmlns="jabber:client" xmlns:stream="` + StreamNS + `" xmlns:p="urn:outer"` + namespaceDeclarations(MaxNamespaceBindings-4, "a") + `>` +
		`<message><p:x xmlns:p="urn:inner"/><p:x/></message>` +
		`<stream:stream xmlns="jabber:client" xmlns:stream="` + StreamNS + `" xmlns:p="urn:new"` + namespaceDeclarations(MaxNamespaceBindings-4, "b") + `>` +
		`<message><p:x/></message>`
	r := NewReader(strings.NewReader(raw))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	f, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f.Element.Children[0].Element.Name.Space != "urn:inner" || f.Element.Children[1].Element.Name.Space != "urn:outer" {
		t.Fatal("namespace rebinding leaked into sibling")
	}
	if _, err = r.Next(); err != nil {
		t.Fatalf("fresh stream inherited old namespace limit: %v", err)
	}
	f, err = r.Next()
	if err != nil || f.Element.Children[0].Element.Name.Space != "urn:new" {
		t.Fatalf("stream restart did not reset namespace: %v", err)
	}
}

func TestXMLNestingLimit(t *testing.T) {
	nested := func(depth int) string { return strings.Repeat(`<x>`, depth) + strings.Repeat(`</x>`, depth) }
	for name, decode := range map[string]func(string) error{
		"stream":   func(raw string) error { _, err := NewReader(strings.NewReader(raw)).Next(); return err },
		"document": func(raw string) error { var e Element; return xml.Unmarshal([]byte(raw), &e) },
	} {
		t.Run(name, func(t *testing.T) {
			if err := decode(nested(maxNesting)); err != nil {
				t.Fatalf("depth at the limit rejected: %v", err)
			}
			if err := decode(nested(maxNesting + 1)); err == nil || !strings.Contains(err.Error(), "nesting limit") {
				t.Fatalf("expected nesting rejection, got %v", err)
			}
		})
	}
}

func TestCloudEncodingPreservesHTTPText(t *testing.T) {
	text := "PUT /test HTTP/1.1\r\nHeader: value\r\n\r\npayload"
	e := E(ClientNS, "message", Text(ClientNS, "body", text), Text("urn:extension", "body", "unknown\r\ntext"))
	out, err := Marshal(e, false)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("\r")) {
		t.Fatal("cloud encoder emitted a literal carriage return")
	}
	r := NewReader(strings.NewReader(`<stream:stream xmlns="jabber:client" xmlns:stream="` + StreamNS + `">` + string(out)))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	f, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if f.Element.Child(ClientNS, "body").Text() != text || f.Element.Child("urn:extension", "body").Text() != "unknown\r\ntext" {
		t.Fatal("cloud roundtrip changed HTTP or extension text")
	}
}

func TestCodecEdgeNames(t *testing.T) {
	// Names encoding/xml reads must be accepted, and those it cannot write
	// back refused.
	if _, err := NewReader(strings.NewReader("<message xmlns=\"jabber:client\" to=\"a\"><\u3007/></message>")).Next(); err != nil {
		t.Fatal("valid name refused:", err)
	}
	if _, err := NewReader(strings.NewReader("<p:\u30fc xmlns:p=\"urn:x\"/>")).Next(); err == nil {
		t.Fatal("name a peer cannot read accepted")
	}
}

func TestTypedExtensionKeepsEmptyNamespace(t *testing.T) {
	var m Message
	if err := xml.Unmarshal([]byte(`<message xmlns="jabber:client" to="a"><body>x</body><foo xmlns=""/></message>`), &m); err != nil {
		t.Fatal(err)
	}
	raw, err := xml.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// Without it, foo would move into jabber:client.
	if !bytes.Contains(raw, []byte(`xmlns=""`)) {
		t.Fatalf("extension changed namespace: %s", raw)
	}
}

// Only a stanza's own jabber:client body holding HTTP gets literal CRLF for
// the device; any other text keeps its exact value.
func TestDeviceFormLiteralNewlines(t *testing.T) {
	http := "HTTP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n"
	for name, tc := range map[string]struct {
		e       Element
		literal bool
	}{
		"stanza body":       {E(ClientNS, "message", Text(ClientNS, "body", http)), true},
		"non-HTTP body":     {E(ClientNS, "message", Text(ClientNS, "body", "line\r\nbreak")), false},
		"foreign body":      {E(ClientNS, "message", Text("urn:extension", "body", http)), false},
		"unqualified body":  {E(ClientNS, "message", Text("", "body", http)), false},
		"extension LF body": {E(ClientNS, "message", E("urn:extension", "payload", Text(ClientNS, "body", "HTTP/1.0 200 OK\n\n"))), false},
		"extension body":    {E(ClientNS, "message", E("urn:extension", "payload", Text(ClientNS, "body", http))), false},
		"nested message":    {E(ClientNS, "presence", E(ClientNS, "message", Text(ClientNS, "body", http))), false},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := Marshal(&tc.e, true)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte("\r\n")) != tc.literal {
				t.Fatalf("literal CRLF = %t: %q", !tc.literal, raw)
			}
			if tc.literal {
				return
			}
			var got Element
			decoder := xml.NewDecoder(bytes.NewReader(raw))
			decoder.DefaultSpace = ClientNS
			if err := decoder.Decode(&got); err != nil || !reflect.DeepEqual(canonicalElement(got), canonicalElement(tc.e)) {
				t.Fatalf("text changed: %q, %v", raw, err)
			}
		})
	}
}

func TestStreamHeaderRoundTrip(t *testing.T) {
	// In both mode headers are relayed: every field must survive.
	want := Stream{From: "a@example", To: "example", ID: "abc", Version: "1.0", Lang: "en"}
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, Frame{Stream: &want}); err != nil {
		t.Fatal(err)
	}
	f, err := NewReader(&buffer).Next()
	if err != nil || f.Stream == nil || *f.Stream != want {
		t.Fatalf("read %+v, %v", f.Stream, err)
	}
}

func TestFrameOffsets(t *testing.T) {
	header := `<stream:stream xmlns="jabber:client" xmlns:stream="` + StreamNS + `">`
	stanza := `<message><body>plain</body></message>`
	parts := []string{" \n", header, "\r\n ", stanza, "\t", header, "\n", stanza, " ", "</stream:stream>"}
	raw := strings.Join(parts, "")
	var offsets []int64
	var offset int64
	for _, part := range parts {
		if strings.HasPrefix(part, "<") {
			offsets = append(offsets, offset)
		}
		offset += int64(len(part))
	}
	for _, size := range []int{1, 7, 4096} {
		r := NewReader(chunks{strings.NewReader(raw), size})
		for i, want := range offsets {
			frame, err := r.Next()
			if err != nil || frame.Offset != want {
				t.Fatalf("chunk=%d frame=%d: offset=%d, want %d, error=%v", size, i, frame.Offset, want, err)
			}
		}
	}
}

func TestQNameContext(t *testing.T) {
	const xsi = "http://www.w3.org/2001/XMLSchema-instance"
	const types = "urn:fixture-types"
	header := `<stream:stream xmlns="jabber:client" xmlns:stream="` + StreamNS + `" xmlns:t="` + types + `">`
	for name, tc := range map[string]struct {
		raw        string
		prefix     string
		value      string
		namespace  string
		target     xml.Name
		attribute  xml.Name
		addForeign bool
	}{
		"stream inherited": {
			raw:       `<message><body>plain</body><query xmlns="urn:extension" xmlns:xsi="` + xsi + `" xsi:type="t:Data">t:Data</query></message>`,
			prefix:    "t",
			value:     "t:Data",
			target:    xml.Name{Space: "urn:extension", Local: "query"},
			attribute: xml.Name{Space: xsi, Local: "type"},
		},
		"locally declared": {
			raw:       `<message><query xmlns="urn:extension" xmlns:local="` + types + `" xmlns:xsi="` + xsi + `" xsi:type="local:Data"><![CDATA[local:]]>Data</query></message>`,
			prefix:    "local",
			value:     "local:Data",
			target:    xml.Name{Space: "urn:extension", Local: "query"},
			attribute: xml.Name{Space: xsi, Local: "type"},
		},
		"default QName namespace": {
			raw:       `<message><ext:query xmlns:ext="urn:extension" xmlns="` + types + `" xmlns:xsi="` + xsi + `" xsi:type="Data">Data</ext:query></message>`,
			value:     "Data",
			target:    xml.Name{Space: "urn:extension", Local: "query"},
			attribute: xml.Name{Space: xsi, Local: "type"},
		},
		"inherited default QName namespace": {
			raw:       `<message><ext:item xmlns:ext="urn:extension" xmlns:xsi="` + xsi + `" xsi:type="Data">Data</ext:item></message>`,
			value:     "Data",
			namespace: ClientNS,
			target:    xml.Name{Space: "urn:extension", Local: "item"},
			attribute: xml.Name{Space: xsi, Local: "type"},
		},
		"unprefixed extension and QName": {
			raw:       `<message><extra xmlns:xsi="` + xsi + `" xsi:type="Data">Data</extra></message>`,
			value:     "Data",
			namespace: ClientNS,
			target:    xml.Name{Space: ClientNS, Local: "extra"},
			attribute: xml.Name{Space: xsi, Local: "type"},
		},
		"generated prefix must not shadow": {
			raw:        `<message><query xmlns="urn:extension" xmlns:a0="` + types + `" xmlns:xsi="` + xsi + `" xsi:type="a0:Data">a0:Data</query></message>`,
			prefix:     "a0",
			value:      "a0:Data",
			target:     xml.Name{Space: "urn:extension", Local: "query"},
			attribute:  xml.Name{Space: xsi, Local: "type"},
			addForeign: true,
		},
		"stream prefix must not shadow": {
			raw:       `<s:features xmlns:s="` + StreamNS + `" xmlns:stream="` + types + `" kind="stream:Data">stream:Data</s:features>`,
			prefix:    "stream",
			value:     "stream:Data",
			target:    xml.Name{Space: StreamNS, Local: "features"},
			attribute: xml.Name{Local: "kind"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := NewReader(strings.NewReader(header + tc.raw))
			if _, err := r.Next(); err != nil {
				t.Fatal(err)
			}
			frame, err := r.Next()
			if err != nil {
				t.Fatal(err)
			}
			target := func(e *Element) *Element {
				if e.Name == tc.target {
					return e
				}
				return e.Child(tc.target.Space, tc.target.Local)
			}
			if tc.addForeign {
				target(frame.Element).Attr = append(target(frame.Element).Attr, xml.Attr{Name: xml.Name{Space: "urn:generated", Local: "marker"}, Value: "present"})
			}
			check := func(raw []byte) {
				t.Helper()
				reader := NewReader(io.MultiReader(strings.NewReader(`<stream:stream xmlns="jabber:client" xmlns:stream="`+StreamNS+`">`), bytes.NewReader(raw)))
				if _, err := reader.Next(); err != nil {
					t.Fatal(err)
				}
				got, err := reader.Next()
				if err != nil || got.Element == nil || got.Element.Name != frame.Element.Name {
					t.Fatalf("re-read: %v", err)
				}
				query := target(got.Element)
				if query == nil {
					t.Fatal("changed extension namespace")
				}
				space, _ := query.namespaces.lookup(tc.prefix)
				want := tc.namespace
				if want == "" {
					want = types
				}
				if space != want || query.Text() != tc.value {
					t.Fatalf("QName %q namespace=%q, text=%q", tc.value, space, query.Text())
				}
				found, foreign := false, !tc.addForeign
				for _, a := range query.Attr {
					found = found || a.Name == tc.attribute && a.Value == tc.value
					foreign = foreign || a.Name == (xml.Name{Space: "urn:generated", Local: "marker"}) && a.Value == "present"
				}
				if !found || !foreign {
					t.Fatal("changed attribute namespace or QName value")
				}
			}
			for _, device := range []bool{false, true} {
				raw, err := Marshal(frame, device)
				if err != nil {
					t.Fatal(err)
				}
				// Read on a stream without the source's bindings: the stanza
				// must supply the context it needs after being relayed.
				check(raw)
				for range 3 {
					again, err := Marshal(frame, device)
					if err != nil || !bytes.Equal(raw, again) {
						t.Fatal("non-deterministic namespace encoding")
					}
				}
			}
			generic, err := xml.Marshal(frame.Element)
			if err != nil {
				t.Fatal(err)
			}
			check(generic)
			typed, err := frame.Element.Typed()
			if err != nil {
				t.Fatal(err)
			}
			generic, err = xml.Marshal(typed)
			if err != nil {
				t.Fatal(err)
			}
			check(generic)
		})
	}
}

func TestQNameRebinding(t *testing.T) {
	header := `<stream:stream xmlns="jabber:client" xmlns:stream="` + StreamNS + `" xmlns:t="urn:outer">`
	r := NewReader(strings.NewReader(header + `<message><a xmlns:t="urn:inner">t:A</a><b>t:B</b></message>` + strings.Replace(header, "urn:outer", "urn:new", 1) + `<message><b>t:B</b></message>`))
	var frames []Frame
	for range 2 {
		if _, err := r.Next(); err != nil {
			t.Fatal(err)
		}
		frame, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, frame)
	}
	for i, frame := range frames {
		for _, device := range []bool{false, true} {
			raw, err := Marshal(frame, device)
			if err != nil {
				t.Fatal(err)
			}
			var got Element
			if err := xml.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			for _, node := range got.Children {
				want := "urn:outer"
				if i == 1 {
					want = "urn:new"
				} else if node.Element.Name.Local == "a" {
					want = "urn:inner"
				}
				if space, _ := node.Element.namespaces.lookup("t"); space != want {
					t.Fatalf("rebound QName namespace=%q, want %q", space, want)
				}
			}
		}
	}
}

func TestQNameReencodingBound(t *testing.T) {
	space := "urn:" + strings.Repeat("n", 1000)
	raw := `<message xmlns="jabber:client" xmlns:t="` + space + `">` + strings.Repeat(`<x>t:Data</x>`, MaxStanzaNodes-3) + `</message>`
	frame, err := NewReader(strings.NewReader(raw)).Next()
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range []bool{false, true} {
		if _, err := Marshal(frame, device); err == nil {
			t.Fatal("QName namespace amplification escaped output bound")
		}
	}
	if _, err := frame.Element.Typed(); err == nil {
		t.Fatal("typed QName namespace amplification escaped output bound")
	}
}
