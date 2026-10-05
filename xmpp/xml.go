// Package xmpp implements typed, namespace-aware XML stream framing.
package xmpp

import (
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
	return e.encode(enc, "", false)
}

// EncodeInStream inherits the default jabber:client namespace from the stream.
func (e Element) EncodeInStream(enc *xml.Encoder) error { return e.encode(enc, ClientNS, true) }

func (e Element) encode(enc *xml.Encoder, parent string, device bool) error {
	start := xml.StartElement{Name: xml.Name{Local: e.Name.Local}, Attr: append([]xml.Attr(nil), e.Attr...)}
	childNamespace := e.Name.Space
	if e.Name.Space == StreamNS {
		start.Name.Local = "stream:" + e.Name.Local
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:stream"}, Value: StreamNS})
		childNamespace = parent
	} else if e.Name.Space != parent {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: e.Name.Space})
	}
	if device && e.Name == (xml.Name{Space: ClientNS, Local: "body"}) && len(e.Children) == 1 && e.Children[0].Element == nil && isHTTPBody(e.Children[0].Text) {
		text := strings.ReplaceAll(e.Children[0].Text, "\r\n", "\n")
		text = strings.ReplaceAll(text, "\n", "\r\n")
		return enc.EncodeElement(Body{Text: text}, start)
	}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	for _, n := range e.Children {
		if n.Element != nil {
			if err := n.Element.encode(enc, childNamespace, device); err != nil {
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
	for _, a := range start.Attr {
		if a.Name.Space != "xmlns" && a.Name.Local != "xmlns" {
			e.Attr = append(e.Attr, a)
		}
	}
	for {
		token, err := d.Token()
		if err != nil {
			return err
		}
		switch v := token.(type) {
		case xml.StartElement:
			child := new(Element)
			if err := child.decode(d, v, depth+1); err != nil {
				return err
			}
			e.Children = append(e.Children, Node{Element: child})
		case xml.CharData:
			if len(e.Children) > 0 && e.Children[len(e.Children)-1].Element == nil {
				e.Children[len(e.Children)-1].Text += string(v)
			} else {
				e.Children = append(e.Children, Node{Text: string(v)})
			}
		case xml.EndElement:
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
	namespaces map[string]string
}

func NewReader(r io.Reader) *Reader {
	return &Reader{d: xml.NewDecoder(r), namespaces: map[string]string{"xml": XMLNS}}
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
				r.namespaces = ns
				s := new(Stream)
				for _, a := range v.Attr {
					switch a.Name.Local {
					case "from":
						s.From = a.Value
					case "to":
						s.To = a.Value
					case "id":
						s.ID = a.Value
					case "version":
						s.Version = a.Value
					case "lang":
						if a.Name.Space == "xml" {
							s.Lang = a.Value
						}
					}
				}
				return Frame{Stream: s}, nil
			}
			offset := r.d.InputOffset()
			element, err := r.element(v, ns, 0, offset)
			return Frame{Element: element}, err
		}
	}
}

func (r *Reader) element(start xml.StartElement, ns map[string]string, depth int, offset int64) (*Element, error) {
	if depth > 64 {
		return nil, errors.New("XML nesting limit exceeded")
	}
	name, err := resolve(start.Name, ns, false)
	if err != nil {
		return nil, err
	}
	e := &Element{Name: name}
	for _, a := range start.Attr {
		if a.Name.Space == "xmlns" || a.Name.Local == "xmlns" {
			continue
		}
		a.Name, err = resolve(a.Name, ns, true)
		if err != nil {
			return nil, err
		}
		e.Attr = append(e.Attr, a)
	}
	for {
		token, err := r.d.RawToken()
		if err != nil {
			return nil, err
		}
		if r.d.InputOffset()-offset > MaxStanzaBytes {
			return nil, errors.New("XMPP stanza exceeds size limit")
		}
		switch v := token.(type) {
		case xml.StartElement:
			child, err := r.element(v, scope(ns, v.Attr), depth+1, offset)
			if err != nil {
				return nil, err
			}
			e.Children = append(e.Children, Node{Element: child})
		case xml.CharData:
			if len(e.Children) > 0 && e.Children[len(e.Children)-1].Element == nil {
				e.Children[len(e.Children)-1].Text += string(v)
			} else {
				e.Children = append(e.Children, Node{Text: string(v)})
			}
		case xml.EndElement:
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
		if end, ok := token.(xml.EndElement); ok && wasStart && end.Name == previous.Name {
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
			path = append(path, v.Name)
		case xml.EndElement:
			if len(path) > 0 {
				path = path[:len(path)-1]
			}
		case xml.CharData:
			if len(path) > 0 && path[len(path)-1].Local == "body" && isHTTPBody(string(v)) {
				result = append(result, raw[offset:begin]...)
				text := strings.NewReplacer("&#xD;", "\r", "&#xA;", "\n", "&#13;", "\r", "&#10;", "\n").Replace(string(raw[begin:end]))
				result = append(result, text...)
				offset = end
			}
		}
	}
	return append(result, raw[offset:]...), nil
}
