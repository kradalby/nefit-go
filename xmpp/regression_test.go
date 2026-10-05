package xmpp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

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
		first, err := CompactEmpty([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		second, err := CompactEmpty(first)
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
		device, err := DeviceXML(second)
		if err != nil {
			t.Fatal(err)
		}
		if string(device) != string(second) {
			t.Fatalf("device adapter changed empty elements: %s", device)
		}
	}
}

func TestDeviceXMLPreservesForeignBodyNewlines(t *testing.T) {
	text := "HTTP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n"
	for _, space := range []string{"urn:extension", ""} {
		original := E(ClientNS, "message", Text(space, "body", text))
		raw, err := xml.Marshal(original)
		if err != nil {
			t.Fatal(err)
		}
		raw, err = DeviceXML(raw)
		if err != nil {
			t.Fatal(err)
		}
		var got Element
		if err := xml.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		if got.Child(space, "body").Text() != text {
			t.Fatalf("foreign body changed: %s", raw)
		}
	}
}

func TestFragmentedTextAllocationBound(t *testing.T) {
	raw := `<message xmlns="jabber:client"><body>` + strings.Repeat(`<![CDATA[x]]>`, 80000) + `</body></message>`
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
			if text := element.Child(ClientNS, "body").Text(); text != strings.Repeat("x", 80000) {
				t.Fatal("fragmented text changed")
			}
		})
	}
}

func TestNestedExtensionBodyPreservesText(t *testing.T) {
	for _, text := range []string{"HTTP/1.0 200 OK\n\n", "HTTP/1.0 200 OK\r\n\r\n"} {
		original := E(ClientNS, "message", E("urn:extension", "payload", Text(ClientNS, "body", text)))
		var output bytes.Buffer
		enc := xml.NewEncoder(&output)
		if err := original.EncodeInStream(enc); err != nil {
			t.Fatal(err)
		}
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
		raw, err := DeviceXML(output.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		var result Element
		decoder := xml.NewDecoder(bytes.NewReader(raw))
		decoder.DefaultSpace = ClientNS
		if err := decoder.Decode(&result); err != nil {
			t.Fatal(err)
		}
		if got := result.Child("urn:extension", "payload").Child(ClientNS, "body").Text(); got != text {
			t.Fatalf("nested extension changed from %q to %q", text, got)
		}
	}
}
