// Package xmpp implements typed, namespace-aware XML stream framing.
package xmpp

import (
	"bufio"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

const (
	ClientNS  = "jabber:client"
	StreamNS  = "http://etherx.jabber.org/streams"
	SASLNS    = "urn:ietf:params:xml:ns:xmpp-sasl"
	BindNS    = "urn:ietf:params:xml:ns:xmpp-bind"
	SessionNS = "urn:ietf:params:xml:ns:xmpp-session"
	PingNS    = "urn:xmpp:ping"
	TLSNS     = "urn:ietf:params:xml:ns:xmpp-tls"
	XMLNS     = "http://www.w3.org/XML/1998/namespace"
	// Captured stanzas stay far below this; the bound keeps a hostile peer's
	// parsed and re-encoded trees small.
	MaxStanzaBytes = 1 << 16
	// Bound namespace work independently of stanza length and nesting depth.
	MaxNamespaceBindings = 128
	// MaxStanzaNodes bounds a stanza's elements plus attributes, so tree size
	// does not grow with byte count.
	MaxStanzaNodes = 256
	maxNesting     = 64
	xmlnsNS        = "http://www.w3.org/2000/xmlns/"
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
		// Scopes are immutable and shared; QName values need their original
		// bindings even when no element or attribute name uses those prefixes.
		namespaces *namespaceScope
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
	// The enclosing element's default namespace is unknown here, so the
	// placeholder makes encode always declare one, xmlns="" included.
	return e.encode(enc, nil, "\x00", xml.Name{}, true)
}

// Marshal encodes a frame, element or typed stanza as it goes on the wire,
// inheriting jabber:client from the stream. The device form spells HTTP body
// newlines as literal CRLF and self-closes empty elements, as the firmware
// expects; the cloud form keeps character references, since a conforming
// server would normalise literal CR away. Re-encoding repeats namespace
// declarations per element, so the output is bounded rather than the input.
func Marshal(v any, device bool) ([]byte, error) {
	buffer := limitedBuffer{limit: 2 * MaxStanzaBytes}
	element, ok := v.(*Element)
	if f, isFrame := v.(Frame); isFrame {
		if f.Element == nil {
			err := WriteFrame(&buffer, f)
			return buffer.Bytes(), err
		}
		element, ok = f.Element, true
	}
	if !ok {
		raw, err := xml.Marshal(v)
		if err != nil {
			return nil, err
		}
		element = new(Element)
		d := xml.NewDecoder(strings.NewReader(string(raw)))
		d.DefaultSpace = ClientNS
		if err := d.Decode(element); err != nil {
			return nil, err
		}
	}
	enc := xml.NewEncoder(&buffer)
	var w io.Writer
	if device {
		w = &buffer
	}
	if err := element.encode(enc, w, ClientNS, xml.Name{}, false); err != nil {
		return nil, err
	}
	if err := enc.Flush(); err != nil {
		return nil, err
	}
	if !device {
		return buffer.Bytes(), nil
	}
	return compactEmpty(buffer.Bytes())
}

var literalNewlines = strings.NewReplacer("&#xD;", "\r", "&#xA;", "\n")

// encode writes device HTTP bodies to raw, when set, with literal newlines.
func (e Element) encode(enc *xml.Encoder, raw io.Writer, parent string, parentName xml.Name, standalone bool) error {
	start := xml.StartElement{Name: xml.Name{Local: e.Name.Local}}
	bindings := map[string]string{"xml": XMLNS}
	for ns := e.namespaces; ns != nil; ns = ns.parent {
		for prefix, space := range ns.bindings {
			if _, exists := bindings[prefix]; !exists {
				bindings[prefix] = space
			}
		}
	}
	var prefixes []string
	for prefix := range bindings {
		if prefix != "" && prefix != "xml" {
			prefixes = append(prefixes, prefix)
		}
	}
	slices.Sort(prefixes)
	declarations := make(map[string]string)
	// Declare a value's bindings on its own element, so decoding a typed
	// extension in isolation also keeps its QName context. Looking for the
	// prefix is conservative: extension schemas are not known here.
	text := e.Text()
	for _, prefix := range prefixes {
		used := strings.Contains(text, prefix+":")
		for _, a := range e.Attr {
			used = used || strings.Contains(a.Value, prefix+":")
		}
		if used {
			declarations[prefix] = bindings[prefix]
		}
	}
	nextPrefix := 0
	prefixFor := func(space, preferred string) string {
		for _, prefix := range prefixes {
			if bindings[prefix] == space {
				declarations[prefix] = space
				return prefix
			}
		}
		prefix := preferred
		for {
			_, reserved := bindings[prefix]
			if prefix != "" && !reserved {
				break
			}
			prefix = "a" + strconv.Itoa(nextPrefix)
			nextPrefix++
		}
		bindings[prefix] = space
		prefixes = append(prefixes, prefix)
		declarations[prefix] = space
		return prefix
	}
	for _, a := range e.Attr {
		if a.Name.Space == "" || a.Name.Space == XMLNS {
			start.Attr = append(start.Attr, a)
			continue
		}
		prefix := prefixFor(a.Name.Space, "")
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: prefix + ":" + a.Name.Local}, Value: a.Value})
	}
	childNamespace := parent
	if e.namespaces != nil {
		childNamespace, _ = e.namespaces.lookup("")
	} else if e.Name.Space != StreamNS {
		childNamespace = e.Name.Space
	}
	if e.Name.Space != childNamespace {
		preferred := ""
		if e.Name.Space == StreamNS {
			preferred = "stream"
		}
		start.Name.Local = prefixFor(e.Name.Space, preferred) + ":" + e.Name.Local
	}
	var declared []string
	for prefix := range declarations {
		declared = append(declared, prefix)
	}
	slices.Sort(declared)
	for _, prefix := range declared {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns:" + prefix}, Value: declarations[prefix]})
	}
	// Typed decoding starts Element.UnmarshalXML afresh for extensions,
	// without the enclosing stanza's default namespace.
	typedParent := parentName.Space == SASLNS || parentName.Space == ClientNS && (parentName.Local == "message" || parentName.Local == "iq" || parentName.Local == "presence")
	if childNamespace != parent || standalone && typedParent {
		start.Attr = append(start.Attr, xml.Attr{Name: xml.Name{Local: "xmlns"}, Value: childNamespace})
	}
	if err := enc.EncodeToken(start); err != nil {
		return err
	}
	if raw != nil && parentName == (xml.Name{Space: ClientNS, Local: "message"}) && e.Name == (xml.Name{Space: ClientNS, Local: "body"}) && len(e.Children) == 1 && e.Children[0].Element == nil && isHTTPBody(e.Children[0].Text) {
		var text strings.Builder
		_ = xml.EscapeText(&text, []byte(strings.ReplaceAll(e.Children[0].Text, "\r\n", "\n")))
		if err := enc.Flush(); err != nil {
			return err
		}
		if _, err := io.WriteString(raw, strings.ReplaceAll(literalNewlines.Replace(text.String()), "\n", "\r\n")); err != nil {
			return err
		}
		return enc.EncodeToken(start.End())
	}
	if parentName != (xml.Name{}) {
		// Only the stanza's own body is HTTP; a message nested in another
		// stanza or an extension keeps its text exact.
		raw = nil
	}
	for _, n := range e.Children {
		if n.Element != nil {
			if err := n.Element.encode(enc, raw, childNamespace, e.Name, standalone); err != nil {
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
	budget := MaxStanzaNodes
	parent := baseNamespaces
	if d.DefaultSpace != "" {
		parent = &namespaceScope{parent: parent, bindings: map[string]string{"": d.DefaultSpace}, count: parent.count + 1}
	}
	return e.decode(d, start, 0, parent, &budget)
}

func (e *Element) decode(d *xml.Decoder, start xml.StartElement, depth int, parent *namespaceScope, budget *int) error {
	if depth >= maxNesting {
		return errors.New("XML nesting limit exceeded")
	}
	if *budget -= 1 + len(start.Attr); *budget < 0 {
		return errors.New("XML element limit exceeded")
	}
	e.Name = start.Name
	e.Attr = nil
	e.Children = nil
	if err := validateAttributes(start.Attr); err != nil {
		return err
	}
	ns, err := scope(parent, start.Attr)
	if err != nil {
		return err
	}
	e.namespaces = ns
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
			if err := child.decode(d, v, depth+1, ns, budget); err != nil {
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
		case xml.Directive, xml.Comment, xml.ProcInst:
			return errors.New("XML directives, comments and processing instructions are not permitted in stanzas")
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
	// Offset is the start of this frame's token in the input stream. Stream
	// restarts retain the decoder, so offsets never restart with the login.
	Offset int64
}

// Reader uses raw tokens so an authenticated stream restart resets namespace
// scope without dropping decoder-buffered bytes or nesting the previous stream.
type Reader struct {
	d          *xml.Decoder
	input      *frameInput
	namespaces *namespaceScope
	elements   int
}

func NewReader(r io.Reader) *Reader {
	input := &frameInput{reader: bufio.NewReader(r)}
	return &Reader{d: xml.NewDecoder(input), input: input, namespaces: baseNamespaces}
}

// Implement ByteReader so the XML decoder cannot buffer an unbounded token
// before checking the quota. Read-ahead in the underlying reader stays bounded.
type frameInput struct {
	reader    *bufio.Reader
	remaining int
	eof       bool
}

func (r *frameInput) ReadByte() (byte, error) {
	if r.remaining == 0 {
		return 0, errors.New("XMPP stanza exceeds size limit")
	}
	b, err := r.reader.ReadByte()
	if err == nil {
		r.remaining--
	}
	r.eof = r.eof || err == io.EOF
	return b, err
}

// truncated reports a decoder error caused by the stream ending mid-token as
// io.ErrUnexpectedEOF, so callers can tell it from a clean close.
func (r *Reader) truncated(err error) error {
	if err != io.EOF && r.input.eof && !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: %v", io.ErrUnexpectedEOF, err)
	}
	return err
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

// isNCName checks the XML Namespaces NCName production. encoding/xml only
// checks the full name, so "p:0" would re-encode as an invalid "<0>".
func isNCName(s string) bool {
	if s == "" {
		return false
	}
	ascii := true
	for i, r := range s {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_'
		switch {
		case r >= 0x80:
			ascii = false
		case letter || i > 0 && (r >= '0' && r <= '9' || r == '-' || r == '.'):
		default:
			return false
		}
	}
	if ascii {
		return true
	}
	// Beyond ASCII, defer to encoding/xml's name tables, which peers share,
	// so a name it reads is one it writes back.
	_, err := xml.NewDecoder(strings.NewReader("<" + s + "/>")).RawToken()
	return err == nil
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

func resolvedAttributes(attrs []xml.Attr, ns *namespaceScope) ([]xml.Attr, error) {
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
		a.Value = attrValue(a.Value)
		result = append(result, a)
	}
	return result, validateAttributes(result)
}

type namespaceScope struct {
	parent   *namespaceScope
	bindings map[string]string
	count    int
}

var baseNamespaces = &namespaceScope{bindings: map[string]string{"xml": XMLNS}, count: 1}

func (s *namespaceScope) lookup(prefix string) (string, bool) {
	for ; s != nil; s = s.parent {
		if value, ok := s.bindings[prefix]; ok {
			return value, true
		}
	}
	return "", false
}

// attrValue applies XML 1.0 3.3.3, which turns literal whitespace into spaces
// and which encoding/xml skips: a relayed IQ id or namespace would otherwise
// read differently to the peer. Character references to whitespace, rarer,
// lose out.
func attrValue(v string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		return r
	}, v)
}

func scope(parent *namespaceScope, attrs []xml.Attr) (*namespaceScope, error) {
	var child *namespaceScope
	for _, a := range attrs {
		if !namespaceAttribute(a.Name) {
			continue
		}
		a.Value = attrValue(a.Value)
		prefix := ""
		if a.Name.Space == "xmlns" {
			prefix = a.Name.Local
			switch {
			case !isNCName(prefix) || prefix == "xmlns" || a.Value == "" || a.Value == xmlnsNS:
				return nil, fmt.Errorf("invalid XML namespace declaration for prefix %q", prefix)
			case (prefix == "xml") != (a.Value == XMLNS):
				return nil, errors.New("the xml prefix and namespace are reserved")
			}
		} else if a.Value == XMLNS || a.Value == xmlnsNS {
			return nil, errors.New("reserved XML namespace as default namespace")
		}
		current := parent
		if child != nil {
			current = child
		}
		value, exists := current.lookup(prefix)
		if value == a.Value && (exists || prefix == "") {
			continue
		}
		if child == nil {
			child = &namespaceScope{parent: parent, bindings: make(map[string]string), count: parent.count}
		}
		if !exists {
			child.count++
		}
		if child.count > MaxNamespaceBindings {
			return nil, errors.New("XML namespace limit exceeded")
		}
		child.bindings[prefix] = a.Value
	}
	if child == nil {
		return parent, nil
	}
	return child, nil
}

func resolve(name xml.Name, ns *namespaceScope, attr bool) (xml.Name, error) {
	if !isNCName(name.Local) || name.Space != "" && !isNCName(name.Space) {
		return xml.Name{}, fmt.Errorf("invalid XML name %q", name.Space+":"+name.Local)
	}
	if attr && name.Space == "" {
		return name, nil
	}
	if !attr && name.Space == "xml" {
		// The XML namespace cannot be declared as a default, so such an
		// element could not be written back.
		return xml.Name{}, fmt.Errorf("element %q uses the reserved xml prefix", name.Local)
	}
	if name.Space != "" {
		v, ok := ns.lookup(name.Space)
		if !ok {
			return xml.Name{}, fmt.Errorf("undeclared XML prefix %q", name.Space)
		}
		name.Space = v
	} else {
		name.Space, _ = ns.lookup("")
	}
	return name, nil
}

func (r *Reader) Next() (Frame, error) {
	r.input.remaining = MaxStanzaBytes
	r.elements = MaxStanzaNodes
	for {
		offset := r.d.InputOffset()
		token, err := r.d.RawToken()
		if err != nil {
			return Frame{}, r.truncated(err)
		}
		switch v := token.(type) {
		case xml.ProcInst:
			if v.Target != "xml" {
				return Frame{}, errors.New("unsupported XML processing instruction")
			}
		case xml.Directive, xml.Comment:
			// RFC 6120 restricts streams to elements, attributes and text.
			return Frame{}, errors.New("XML directives and comments are not permitted")
		case xml.CharData:
			// XML whitespace only: Unicode spaces are text.
			if strings.Trim(string(v), " \t\r\n") != "" {
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
			return Frame{End: true, Offset: offset}, nil
		case xml.StartElement:
			if v.Name.Local == "stream" {
				// A new stream resets scope, so it must declare its own namespaces.
				ns, err := scope(baseNamespaces, v.Attr)
				if err != nil {
					return Frame{}, err
				}
				if name, _ := resolve(v.Name, ns, false); name == (xml.Name{Space: StreamNS, Local: "stream"}) {
					frame, err := r.restart(v, ns)
					frame.Offset = offset
					return frame, err
				}
			}
			ns, err := scope(r.namespaces, v.Attr)
			if err != nil {
				return Frame{}, err
			}
			name, err := resolve(v.Name, ns, false)
			if err != nil {
				return Frame{}, err
			}
			if name == (xml.Name{Space: StreamNS, Local: "stream"}) {
				return Frame{}, errors.New("stream header relies on inherited namespace declarations")
			}
			element, err := r.element(v, ns, 0)
			return Frame{Element: element, Offset: offset}, err
		}
	}
}

func (r *Reader) restart(v xml.StartElement, ns *namespaceScope) (Frame, error) {
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

func (r *Reader) element(start xml.StartElement, ns *namespaceScope, depth int) (*Element, error) {
	if depth >= maxNesting {
		return nil, errors.New("XML nesting limit exceeded")
	}
	if r.elements -= 1 + len(start.Attr); r.elements < 0 {
		return nil, errors.New("XML element limit exceeded")
	}
	name, err := resolve(start.Name, ns, false)
	if err != nil {
		return nil, err
	}
	attrs, err := resolvedAttributes(start.Attr, ns)
	if err != nil {
		return nil, err
	}
	e := &Element{Name: name, Attr: attrs, namespaces: ns}
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
		if err == io.EOF {
			// A clean close only happens between stanzas.
			return nil, io.ErrUnexpectedEOF
		}
		if err != nil {
			return nil, r.truncated(err)
		}
		switch v := token.(type) {
		case xml.StartElement:
			flushText()
			childScope, err := scope(ns, v.Attr)
			if err != nil {
				return nil, err
			}
			child, err := r.element(v, childScope, depth+1)
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
		case xml.Directive, xml.ProcInst, xml.Comment:
			return nil, errors.New("XML directives, comments and processing instructions are not permitted in stanzas")
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
		_, err := io.WriteString(w, "</stream:stream>")
		return err
	default:
		return errors.New("empty XMPP frame")
	}
	return enc.Flush()
}

// compactEmpty converts only adjacent empty start/end token pairs in marshalled
// XML to the equivalent self-closing form, matching what the thermostat
// accepted in captures.
// RawToken offsets keep CDATA, escaped text and attributes completely untouched.
func compactEmpty(raw []byte) ([]byte, error) {
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
