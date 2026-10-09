package client

import (
	"errors"
	"iter"
	"strings"
	"unicode"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
)

const updatePrefix = "/gateway/update"

// ErrUpdateBlocked reports a write refused by UpdatesBlock before it was sent.
var ErrUpdateBlocked = errors.New("firmware update write blocked by policy")

// Update blocking must hold for any spelling a lenient device HTTP parser
// might accept. Rather than decode every spelling, block mode refuses write
// lines that are not plainly canonical, then matches the canonical path.

// bareJID drops the resource, and the trailing dot by which a fully
// qualified domain names the same host.
func bareJID(jid string) string {
	bare, _, _ := strings.Cut(strings.TrimSpace(jid), "/")
	return strings.TrimSuffix(bare, ".")
}

// blockedService matches against UpdateServices, which NewLocalClient
// stores as bare JIDs.
func (t *localTransport) blockedService(from, to string) bool {
	for _, jid := range []string{from, to} {
		bare := bareJID(jid)
		// Servers route bare JIDs that RFC 7622 folds together, such as
		// fullwidth letters, as one address. Bosch's are plain ASCII, so any
		// other spelling is refused rather than folded here.
		if strings.ContainsFunc(bare, func(r rune) bool { return r >= 0x80 }) {
			return true
		}
		local, _, _ := strings.Cut(bare, "@")
		if strings.EqualFold(local, "gservice_update") || strings.EqualFold(local, "gservice_firmware") {
			return true
		}
		for _, blocked := range t.options.UpdateServices {
			if strings.EqualFold(bare, blocked) || strings.EqualFold(local, blocked) {
				return true
			}
		}
	}
	return false
}

func (t *localTransport) blocked(m wire.Message) bool {
	if t.options.UpdatePolicy != UpdatesBlock {
		return false
	}
	return t.blockedService(m.From, m.To) || updateWrite(m.Body.Text)
}

// blockedFrame inspects all text in the preserved tree: relaying keeps every
// element, whatever its namespace or nesting.
func (t *localTransport) blockedFrame(frame wire.Frame) bool {
	e := frame.Element
	if t.options.UpdatePolicy != UpdatesBlock || e == nil {
		return false
	}
	return t.blockedService(e.Get("from"), e.Get("to")) || blockedTree(e)
}

// carriesRequest reports a request line in e's text, read joined and node by
// node like blockedTree, so markup cannot hide one.
func carriesRequest(root *wire.Element) bool {
	if hasRequestLine(fullText(root)) {
		return true
	}
	var walk func(*wire.Element) bool
	walk = func(e *wire.Element) bool {
		for _, n := range e.Children {
			if n.Element == nil && hasRequestLine(n.Text) || n.Element != nil && walk(n.Element) {
				return true
			}
		}
		return false
	}
	return walk(root)
}

// blockedTree reads the stanza's text two ways, each in one linear pass:
// joined, so markup cannot split a request line the policy reads, and node by
// node, so markup a lenient parser takes for a line break cannot join one.
// Any element counts, not just a body: such a parser might read a request
// from a subject, an IQ or a differently cased body.
func blockedTree(root *wire.Element) bool {
	if updateWrite(fullText(root)) {
		return true
	}
	var walk func(*wire.Element) bool
	walk = func(e *wire.Element) bool {
		body := strings.EqualFold(e.Name.Local, "body")
		for _, n := range e.Children {
			switch {
			case n.Element == nil:
				if updateWrite(n.Text) {
					return true
				}
			case body:
				// Markup inside a body could split a path the policy reads.
				return true
			case walk(n.Element):
				return true
			}
		}
		return false
	}
	return walk(root)
}

// fullText joins all text inside e in document order.
func fullText(e *wire.Element) string {
	var b strings.Builder
	var walk func(*wire.Element)
	walk = func(e *wire.Element) {
		for _, n := range e.Children {
			if n.Element != nil {
				walk(n.Element)
			} else {
				b.WriteString(n.Text)
			}
		}
	}
	walk(e)
	return b.String()
}

var httpMethods = []string{"GET", "HEAD", "PUT", "POST", "DELETE", "PATCH", "OPTIONS", "CONNECT", "TRACE"}

// updateWrite reports whether text could carry a write the update policy
// refuses. Every line is checked: a body could hold a request after the
// first. A request line names a known method or ends in an HTTP version;
// only a well-formed GET or HEAD is a read.
func updateWrite(text string) bool {
	for line := range lines(text) {
		if !looksLikeRequest(line) {
			continue
		}
		fields := strings.Fields(line)
		method := strings.ToUpper(fields[0])
		versioned := strings.HasPrefix(fields[len(fields)-1], "HTTP/")
		wellFormed := len(fields) == 3 && versioned
		if wellFormed && (method == "GET" || method == "HEAD") {
			continue
		}
		if !wellFormed || !canonicalTarget(fields[1]) {
			return true
		}
		path, _, _ := strings.Cut(strings.ToLower(fields[1]), "?")
		if path == updatePrefix || strings.HasPrefix(path, updatePrefix+"/") {
			return true
		}
	}
	return false
}

// lines also splits at Unicode line breaks, which a lenient parser might honour.
func lines(text string) iter.Seq[string] {
	return strings.FieldsFuncSeq(text, func(r rune) bool {
		return r == '\r' || r == '\n' || r == '\u0085' || r == '\u2028' || r == '\u2029'
	})
}

// hasRequestLine reports whether any line of text could be an HTTP request.
func hasRequestLine(text string) bool {
	for line := range lines(text) {
		if looksLikeRequest(line) {
			return true
		}
	}
	return false
}

// looksLikeRequest reports whether line could be an HTTP request line: one
// naming a known method or ending in an HTTP version. It allocates nothing,
// since hostile text may hold many lines.
func looksLikeRequest(line string) bool {
	line = strings.TrimFunc(line, unicode.IsSpace)
	first := line
	if i := strings.IndexFunc(line, unicode.IsSpace); i >= 0 {
		first = line[:i]
	}
	for _, m := range httpMethods {
		if strings.EqualFold(first, m) {
			return true
		}
	}
	// The separator may be a multi-byte space, so trim it whole.
	last := strings.TrimLeftFunc(line[max(strings.LastIndexFunc(line, unicode.IsSpace), 0):], unicode.IsSpace)
	return len(last) < len(line) && strings.HasPrefix(last, "HTTP/")
}

// canonicalTarget accepts an origin-form target that needs no decoding or
// normalisation to compare. The whole target is checked: a parser that
// resolves dot segments or decodes across the query would otherwise reach
// a path the policy never saw.
func canonicalTarget(target string) bool {
	if !strings.HasPrefix(target, "/") || strings.ContainsAny(target, `%\;#`) || strings.Contains(target, "//") {
		return false
	}
	for _, segment := range strings.FieldsFunc(target, func(r rune) bool { return r == '/' || r == '?' || r == '&' || r == '=' }) {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}
