// Package xmpp implements typed, namespace-aware XML stream framing.
package xmpp

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	ClientNS       = "jabber:client"
	StreamNS       = "http://etherx.jabber.org/streams"
	SASLNS         = "urn:ietf:params:xml:ns:xmpp-sasl"
	BindNS         = "urn:ietf:params:xml:ns:xmpp-bind"
	SessionNS      = "urn:ietf:params:xml:ns:xmpp-session"
	PingNS         = "urn:xmpp:ping"
	TLSNS          = "urn:ietf:params:xml:ns:xmpp-tls"
	XMLNS          = "http://www.w3.org/XML/1998/namespace"
	MaxStanzaBytes = 1 << 20
)

// Node preserves ordered text and element children, including unknown extensions.
type (
	Node struct {
		Text    string
		Element *Element
	}
	Element struct {
		Name     xml.Name
		Attr     []xml.Attr
		Children []Node
	}
)

func E(space, name string, children ...Element) Element {
	e := Element{Name: xml.Name{Space: space, Local: name}}
	for _, c := range children {
		e.Children = append(e.Children, Node{Element: &c})
	}
	return e
}

func Text(space, name, text string) Element {
	e := E(space, name)
	e.Children = []Node{{Text: text}}
	return e
}

func (e *Element) Set(name, value string) {
	for i := range e.Attr {
		if e.Attr[i].Name == (xml.Name{Local: name}) {
			e.Attr[i].Value = value
			return
		}
	}
	e.Attr = append(e.Attr, xml.Attr{Name: xml.Name{Local: name}, Value: value})
}

func (e Element) Get(name string) string {
	for _, a := range e.Attr {
		if a.Name == (xml.Name{Local: name}) {
			return a.Value
		}
	}
	return ""
}

func (e Element) Child(space, name string) *Element {
	for _, n := range e.Children {
		if n.Element != nil && n.Element.Name == (xml.Name{Space: space, Local: name}) {
			return n.Element
		}
	}
	return nil
}

func (e Element) Text() string {
	var b strings.Builder
	for _, n := range e.Children {
		if n.Element == nil {
			b.WriteString(n.Text)
		}
	}
	return b.String()
}

func (e Element) MarshalXML(enc *xml.Encoder, _ xml.StartElement) error {
	return e.encode(enc, "", false, xml.Name{})
}

// EncodeInStream inherits the default jabber:client namespace from the stream.
func (e Element) EncodeInStream(enc *xml.Encoder) error {
	return e.encode(enc, ClientNS, true, xml.Name{})
}

func (e Element) encode(enc *xml.Encoder, parent string, device bool, parentName xml.Name) error {
	start := xml.StartElement{Name: xml.Name{Local: e.Name.Local}, Attr: append([]xml.Attr(nil), e.Attr...)}
	childNamespace := e.Name.Space
	if e.Name.Space == StreamNS {
		start.Name.Local = "stream:" + e.Name.Local
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:stream"}, Value: StreamNS})
		childNamespace = parent
	} else if e.Name.Space != parent {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: e.Name.Space})
	}
	if device && parentName == (xml.Name{Space: ClientNS, Local: "message"}) && e.Name == (xml.Name{Space: ClientNS, Local: "body"}) && len(e.Children) == 1 && e.Children[0].Element == nil && isHTTPBody(e.Children[0].Text) {
		text := strings.ReplaceAll(e.Children[0].Text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\n", "\r\n")
		return enc.EncodeElement(Body{Text: text}, start)
	}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	for _, n := range e.Children {
		if n.Element != nil {
			if err := n.Element.encode(enc, childNamespace, device, e.Name); err != nil {
				return err
			}
		} else {
			if err := enc.EncodeToken(xml.CharData(n.Text)); err != nil {
				return err
			}
		}
	}
	return enc.EncodeToken(start.End())
}

func isHTTPBody(text string) bool {
	for _, prefix := range []string{"GET ", "PUT ", "POST ", "DELETE ", "PATCH ", "HEAD ", "OPTIONS ", "HTTP/"} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func (e *Element) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	return e.decode(d, start, 0)
}

func (e *Element) decode(d *xml.Decoder, start xml.StartElement, depth int) error {
	if depth > 64 {
		return errors.New("XML nesting limit exceeded")
	}
	e.Name = start.Name
	e.Attr = nil
	e.Children = nil
	if err := validateAttributes(start.Attr); err != nil {
		return err
	}
	for _, a := range start.Attr {
		if !namespaceAttribute(a.Name) {
			e.Attr = append(e.Attr, a)
		}
	}
	var text strings.Builder
	textSeen := false
	flushText := func() {
		if textSeen {
			e.Children = append(e.Children, Node{Text: text.String()})
			text.Reset()
			textSeen = false
		}
	}
	for {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch v := token.(type) {
		case xml.StartElement:
			flushText()
			child := new(Element)
			if err := child.decode(d, v, depth+1); err != nil {
				return err
			}
			e.Children = append(e.Children, Node{Element: child})
		case xml.CharData:
			textSeen = true
			_, _ = text.Write(v)
		case xml.EndElement:
			flushText()
			if v.Name != start.Name {
				return errors.New("mismatched XML element")
			}
			return nil
		case xml.Directive:
			return errors.New("XML directives are not permitted")
		}
	}
}

// Stream is an opening header, not a complete XML document.
type Stream struct{ From, To, ID, Version, Lang string }

func (s Stream) start() xml.StartElement {
	start := xml.StartElement{Name: xml.Name{Local: "stream:stream"}, Attr: []xml.Attr{{Name: xml.Name{Local: "xmlns"}, Value: ClientNS}, {Name: xml.Name{Local: "xmlns:stream"}, Value: StreamNS}}}
	for _, a := range []struct{ name, value string }{{"from", s.From}, {"to", s.To}, {"id", s.ID}, {"version", s.Version}, {"xml:lang", s.Lang}} {
		if a.value != "" {
			start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: a.name}, Value: a.value})
		}
	}
	return start
}

type Frame struct {
	Stream  *Stream
	Element *Element
	End     bool
}

// Reader uses raw tokens so an authenticated stream restart resets namespace
// scope without dropping decoder-buffered bytes or nesting the previous stream.
type Reader struct {
	d          *xml.Decoder
	input      *frameInput
	namespaces map[string]string
}

func NewReader(r io.Reader) *Reader {
	input := &frameInput{reader: bufio.NewReader(r)}
	return &Reader{d: xml.NewDecoder(input), input: input, namespaces: map[string]string{"xml": XMLNS}}
}

// Implement ByteReader so the XML decoder cannot buffer an unbounded token
// before checking the quota. Read-ahead in the underlying reader stays bounded.
type frameInput struct {
	reader    *bufio.Reader
	remaining int
}

func (r *frameInput) ReadByte() (byte, error) {
	if r.remaining == 0 {
		return 0, errors.New("XMPP stanza exceeds size limit")
	}
	b, err := r.reader.ReadByte()
	if err == nil {
		r.remaining--
	}
	return b, err
}

func (r *frameInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b, err := r.ReadByte()
	if err != nil {
		return 0, err
	}
	p[0] = b
	return 1, nil
}

func namespaceAttribute(name xml.Name) bool {
	return name.Space == "xmlns" || name == (xml.Name{Local: "xmlns"})
}

func validateAttributes(attrs []xml.Attr) error {
	seen := make(map[xml.Name]bool, len(attrs))
	for _, a := range attrs {
		if seen[a.Name] {
			return fmt.Errorf("duplicate XML attribute %v", a.Name)
		}
		seen[a.Name] = true
	}
	return nil
}

func resolvedAttributes(attrs []xml.Attr, ns map[string]string) ([]xml.Attr, error) {
	if err := validateAttributes(attrs); err != nil {
		return nil, err
	}
	var result []xml.Attr
	for _, a := range attrs {
		if namespaceAttribute(a.Name) {
			continue
		}
		var err error
		a.Name, err = resolve(a.Name, ns, true)
		if err != nil {
			return nil, err
		}
		result = append(result, a)
	}
	return result, validateAttributes(result)
}

func scope(parent map[string]string, attrs []xml.Attr) map[string]string {
	m := map[string]string{}
	for k, v := range parent {
		m[k] = v
	}
	for _, a := range attrs {
		if a.Name.Space == "xmlns" {
			m[a.Name.Local] = a.Value
		} else if a.Name.Local == "xmlns" && a.Name.Space == "" {
			m[""] = a.Value
		}
	}
	return m
}

func resolve(name xml.Name, ns map[string]string, attr bool) (xml.Name, error) {
	if attr && name.Space == "" {
		return name, nil
	}
	if name.Space != "" {
		v, ok := ns[name.Space]
		if !ok {
			return xml.Name{}, fmt.Errorf("undeclared XML prefix %q", name.Space)
		}
		name.Space = v
	} else {
		name.Space = ns[""]
	}
	return name, nil
}

func (r *Reader) Next() (Frame, error) {
	r.input.remaining = MaxStanzaBytes
	for {
		token, err := r.d.RawToken()
		if err != nil {
			return Frame{}, err
		}
		switch v := token.(type) {
		case xml.ProcInst:
			if v.Target != "xml" {
				return Frame{}, errors.New("unsupported XML processing instruction")
			}
		case xml.Directive:
			return Frame{}, errors.New("XML directives are not permitted")
		case xml.CharData:
			if strings.TrimSpace(string(v)) != "" {
				return Frame{}, errors.New("text outside stanza")
			}
		case xml.EndElement:
			name, err := resolve(v.Name, r.namespaces, false)
			if err != nil {
				return Frame{}, err
			}
			if name != (xml.Name{Space: StreamNS, Local: "stream"}) {
				return Frame{}, errors.New("unexpected closing element")
			}
			return Frame{End: true}, nil
		case xml.StartElement:
			ns := scope(r.namespaces, v.Attr)
			name, err := resolve(v.Name, ns, false)
			if err != nil {
				return Frame{}, err
			}
			if name == (xml.Name{Space: StreamNS, Local: "stream"}) {
				ns = scope(map[string]string{"xml": XMLNS}, v.Attr)
				attrs, err := resolvedAttributes(v.Attr, ns)
				if err != nil {
					return Frame{}, err
				}
				r.namespaces = ns
				s := new(Stream)
				for _, a := range attrs {
					switch a.Name {
					case xml.Name{Local: "from"}:
						s.From = a.Value
					case xml.Name{Local: "to"}:
						s.To = a.Value
					case xml.Name{Local: "id"}:
						s.ID = a.Value
					case xml.Name{Local: "version"}:
						s.Version = a.Value
					case xml.Name{Space: XMLNS, Local: "lang"}:
						s.Lang = a.Value
					}
				}
				return Frame{Stream: s}, nil
			}
			element, err := r.element(v, ns, 0)
			return Frame{Element: element}, err
		}
	}
}

func (r *Reader) element(start xml.StartElement, ns map[string]string, depth int) (*Element, error) {
	if depth > 64 {
		return nil, errors.New("XML nesting limit exceeded")
	}
	name, err := resolve(start.Name, ns, false)
	if err != nil {
		return nil, err
	}
	attrs, err := resolvedAttributes(start.Attr, ns)
	if err != nil {
		return nil, err
	}
	e := &Element{Name: name, Attr: attrs}
	var text strings.Builder
	textSeen := false
	flushText := func() {
		if textSeen {
			e.Children = append(e.Children, Node{Text: text.String()})
			text.Reset()
			textSeen = false
		}
	}
	for {
		token, err := r.d.RawToken()
		if err != nil {
			return nil, err
		}
		switch v := token.(type) {
		case xml.StartElement:
			flushText()
			child, err := r.element(v, scope(ns, v.Attr), depth+1)
			if err != nil {
				return nil, err
			}
			e.Children = append(e.Children, Node{Element: child})
		case xml.CharData:
			textSeen = true
			_, _ = text.Write(v)
		case xml.EndElement:
			flushText()
			if v.Name != start.Name {
				return nil, errors.New("mismatched closing element")
			}
			return e, nil
		case xml.Directive, xml.ProcInst:
			return nil, errors.New("unsupported XML directive in stanza")
		}
	}
}

// WriteFrame marshals stream headers and complete typed stanzas without raw XML.
func WriteFrame(w io.Writer, f Frame) error {
	enc := xml.NewEncoder(w)
	switch {
	case f.Stream != nil:
		if err := enc.EncodeToken(f.Stream.start()); err != nil {
			return err
		}
	case f.Element != nil:
		if err := enc.Encode(f.Element); err != nil {
			return err
		}
	case f.End:
		// Marshal the matching start/end token pair and emit only the close. The
		// opening header was already written by an earlier frame encoder.
		var buffer bytes.Buffer
		closing := xml.NewEncoder(&buffer)
		start := Stream{}.start()
		if err := closing.EncodeToken(start); err != nil {
			return err
		}
		if err := closing.Flush(); err != nil {
			return err
		}
		offset := buffer.Len()
		if err := closing.EncodeToken(start.End()); err != nil {
			return err
		}
		if err := closing.Flush(); err != nil {
			return err
		}
		n, err := w.Write(buffer.Bytes()[offset:])
		if err == nil && n != buffer.Len()-offset {
			return io.ErrShortWrite
		}
		return err
	default:
		return errors.New("empty XMPP frame")
	}
	return enc.Flush()
}

// CompactEmpty converts only adjacent empty start/end token pairs in marshalled
// XML to the equivalent self-closing form required by the thermostat's parser.
// RawToken offsets keep CDATA, escaped text and attributes completely untouched.
func CompactEmpty(raw []byte) ([]byte, error) {
	d := xml.NewDecoder(strings.NewReader(string(raw)))
	type edit struct{ from, to int }
	var edits []edit
	var previous xml.StartElement
	var previousEnd int
	wasStart := false
	for {
		token, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if end, ok := token.(xml.EndElement); ok && wasStart && end.Name == previous.Name && int(d.InputOffset()) > previousEnd {
			edits = append(edits, edit{previousEnd - 1, int(d.InputOffset())})
		}
		previous, wasStart = token.(xml.StartElement)
		previousEnd = int(d.InputOffset())
	}
	var result []byte
	offset := 0
	for _, e := range edits {
		result = append(result, raw[offset:e.from]...)
		result = append(result, '/', '>')
		offset = e.to
	}
	return append(result, raw[offset:]...), nil
}

// DeviceXML preserves firmware-compatible HTTP text newlines. Character
// references are changed only within HTTP body text tokens, never attributes or
// other payloads. All message structure and escaping comes from encoding/xml.
func DeviceXML(raw []byte) ([]byte, error) {
	raw, err := CompactEmpty(raw)
	if err != nil {
		return nil, err
	}
	d := xml.NewDecoder(strings.NewReader(string(raw)))
	var path []xml.Name
	namespaces := []map[string]string{{"": ClientNS, "xml": XMLNS}}
	var result []byte
	offset := 0
	for {
		begin := int(d.InputOffset())
		token, err := d.RawToken()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		end := int(d.InputOffset())
		switch v := token.(type) {
		case xml.StartElement:
			ns := scope(namespaces[len(namespaces)-1], v.Attr)
			name, err := resolve(v.Name, ns, false)
			if err != nil {
				return nil, err
			}
			namespaces = append(namespaces, ns)
			path = append(path, name)
		case xml.EndElement:
			if len(path) > 0 {
				path = path[:len(path)-1]
				namespaces = namespaces[:len(namespaces)-1]
			}
		case xml.CharData:
			if len(path) >= 2 && path[len(path)-1] == (xml.Name{Space: ClientNS, Local: "body"}) && path[len(path)-2] == (xml.Name{Space: ClientNS, Local: "message"}) && isHTTPBody(string(v)) {
				result = append(result, raw[offset:begin]...)
				text := strings.NewReplacer("&#xD;", "\r", "&#xA;", "\n", "&#13;", "\r", "&#10;", "\n").Replace(string(raw[begin:end]))
				result = append(result, text...)
				offset = end
			}
		}
	}
	return append(result, raw[offset:]...), nil
}
