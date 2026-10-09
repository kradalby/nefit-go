package client

import (
	"strings"
	"testing"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
)

func TestUpdatePolicyRefusesWrites(t *testing.T) {
	t.Parallel()
	// NewLocalClient stores services as bare JIDs.
	blocking := &localTransport{options: LocalOptions{UpdatePolicy: UpdatesBlock, UpdateServices: []string{"custom_update", "other_update@host"}}}
	allowing := &localTransport{options: LocalOptions{UpdatePolicy: UpdatesAllow}}
	writes := []string{
		"PUT /gateway/update HTTP/1.1\r\n\r\n",
		"PUT /gateway/update/strategy HTTP/1.1\r\n\r\n",
		"put /Gateway/Update/strategy HTTP/1.1\r\n\r\n",
		"POST /gateway/update HTTP/1.1\r\n\r\n",
		"DELETE /gateway/update HTTP/1.1\r\n\r\n",
		"PATCH /gateway/update HTTP/1.1\r\n\r\n",
		"FOO /gateway/update HTTP/1.1\r\n\r\n",
		"PUT /gateway/update?x=1 HTTP/1.1\r\n\r\n",
		// Spellings a lenient parser might normalise are refused unread.
		"PUT //gateway//update HTTP/1.1\r\n\r\n",
		"PUT /gateway/./update HTTP/1.1\r\n\r\n",
		"PUT /x/../gateway/update HTTP/1.1\r\n\r\n",
		"PUT /gateway/%75pdate HTTP/1.1\r\n\r\n",
		"PUT /gateway/%75pdate?%zz HTTP/1.1\r\n\r\n",
		"PUT /%67ateway/update?% HTTP/1.1\r\n\r\n",
		"PUT /%3F/../gateway/update HTTP/1.1\r\n\r\n",
		"PUT /%23/../gateway/update HTTP/1.1\r\n\r\n",
		"PUT /gateway/update;v=1 HTTP/1.1\r\n\r\n",
		`PUT \gateway\update HTTP/1.1` + "\r\n\r\n",
		"PUT gateway/update HTTP/1.1\r\n\r\n",
		"PUT http://host/gateway/update HTTP/1.1\r\n\r\n",
		"PUT /a#frag HTTP/1.1\r\n\r\n",
		// The query is checked too: a parser may resolve or decode across it.
		"PUT /x?/../gateway/update/strategy HTTP/1.1\r\n\r\n",
		"PUT /x?y=%20 HTTP/1.1\r\n\r\n",
		// Malformed write lines could mean anything to the device.
		"PUT /gateway/update x HTTP/1.1\r\n\r\n",
		"PUT x /gateway/update HTTP/1.1\r\n\r\n",
		"PUT /a /gateway/update HTTP/1.1\r\n\r\n",
		"PUT /a HTTP/1.1 /gateway/update\r\n\r\n",
		"PUT /gateway/update\r\n\r\n",
		"GET /gateway/update XTTP/1.1\r\n\r\n",
		"GET /gateway/update HTTP/1.1 extra\r\n\r\n",
		// Requests after blank lines or a first request, and bare CR framing.
		"\r\n\r\nPUT /gateway/update HTTP/1.1\r\n\r\n",
		"GET /x HTTP/1.1\r\n\r\nPUT /gateway/update HTTP/1.1\r\n\r\n",
		"PUT /gateway/update HTTP/1.1\rContent-Length: 0\r\r",
		// Method case, backslashes and dot segments inside the query.
		"put /gateway/update\r\n\r\n",
		"POST /gateway/update\r\n\r\n",
		"DELETE /gateway/update\r\n\r\n",
		"PATCH /gateway/update\r\n\r\n",
		// A blank line before the write, and dot segments around ? and &.
		" \r\nPUT /gateway/update HTTP/1.1\r\n\r\n",
		"\tPUT /gateway/update\r\n\r\n",
		"PUT\t/gateway/update\r\n\r\n",
		"PUT\u00a0/gateway/update\r\n\r\n",
		// A multi-byte space before the version, and Unicode line breaks.
		"FOO /gateway/update\u00a0HTTP/1.1\r\n\r\n",
		"\ufeffPUT /gateway/update\u00a0HTTP/1.1\r\n\r\n",
		"hello\u0085PUT /gateway/update\r\n\r\n",
		"hello\u2028PUT /gateway/update\r\n\r\n",
		"x\rPUT /gateway/update\r\r",
		"PUT /x/..?/gateway/update HTTP/1.1\r\n\r\n",
		"PUT /x?a&../gateway/update HTTP/1.1\r\n\r\n",
		`PUT /gateway\update HTTP/1.1` + "\r\n\r\n",
		"PUT /x?a=../gateway/update HTTP/1.1\r\n\r\n",
	}
	for _, body := range writes {
		m := wire.Message{Body: wire.Body{Text: body}}
		if !blocking.blocked(m) {
			t.Errorf("update write passed block policy: %q", body)
		}
		if allowing.blocked(m) {
			t.Errorf("allow policy blocked %q", body)
		}
	}
	reads := []string{
		"GET /gateway/update/strategy HTTP/1.1\r\n\r\n",
		"get /gateway/versionFirmware HTTP/1.1\r\n\r\n",
		"HEAD /gateway/update HTTP/1.1\r\n\r\n",
		"GET /gateway/%75pdate HTTP/1.1\r\n\r\n",
		"PUT /gateway/updates HTTP/1.1\r\n\r\n",
		"PUT /heatingCircuits/hc1/temperatureRoomManual?x=1 HTTP/1.1\r\nContent-Type: application/json\r\nContent-Length: 24\r\n\r\nAAAAAAAAAAAAAAAAAAAAAA==",
		"HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\nAAAA",
		"HTTP/1.1 204\r\n\r\n",
		"opaque service payload",
	}
	for _, body := range reads {
		if blocking.blocked(wire.Message{Body: wire.Body{Text: body}}) {
			t.Errorf("ordinary traffic blocked: %q", body)
		}
	}
	for _, jid := range []string{"gservice_update@host", "GService_Update@host", "\uff47service_update@host", "gservice_upd\u00adate@host", "other_update@host.", "GSERVICE_FIRMWARE@host/r", " gservice_update@host", "Custom_Update@host", "other_update@host/another", "Other_Update@HOST"} {
		if !blocking.blocked(wire.Message{From: jid}) || !blocking.blocked(wire.Message{To: jid}) {
			t.Errorf("update service %q passed", jid)
		}
		if allowing.blocked(wire.Message{From: jid}) {
			t.Errorf("allow policy blocked service %q", jid)
		}
	}
}

func TestUpdatePolicyInspectsWholeFrame(t *testing.T) {
	t.Parallel()
	blocking := &localTransport{options: LocalOptions{UpdatePolicy: UpdatesBlock}}
	write := "PUT /gateway/update HTTP/1.1\r\n\r\n"
	split := wire.Text(wire.ClientNS, "body", "PUT /gateway/")
	split.Children = append(split.Children, wire.Node{Element: ptrElement(wire.Text("", "span", "update"))}, wire.Node{Text: "/strategy HTTP/1.1"})
	for name, e := range map[string]wire.Element{
		"server namespace":    wire.E("jabber:server", "message", wire.Text("jabber:server", "body", write)),
		"nested body":         wire.E(wire.ClientNS, "message", wire.E("urn:example", "x", wire.Text(wire.ClientNS, "body", write))),
		"second body":         wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "hello"), wire.Text(wire.ClientNS, "body", write)),
		"markup in body":      wire.E(wire.ClientNS, "message", split),
		"capital body":        wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "BODY", write)),
		"subject":             wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "subject", write)),
		"iq text":             wire.E(wire.ClientNS, "iq", wire.Text(wire.ClientNS, "BODY", write)),
		"capital body markup": wire.E(wire.ClientNS, "message", splitIn("BODY", wire.ClientNS)),
		// Markup in a body of any case is refused, even around a read.
		"capital body with markup": wire.E(wire.ClientNS, "message", wire.E(wire.ClientNS, "BODY", wire.Text("", "x", "GET /x HTTP/1.1"))),
		"subject markup":           wire.E(wire.ClientNS, "message", splitIn("subject", wire.ClientNS)),
		"extension markup":         wire.E(wire.ClientNS, "iq", splitIn("q", "urn:x")),
		// No fragment holds a request line on its own.
		"fragments": wire.E(wire.ClientNS, "message", wire.E(wire.ClientNS, "subject",
			wire.Text("", "a", "P"), wire.Text("", "b", "UT /gateway/update HT"), wire.Text("", "c", "TP/1.1"))),
		// Joined, the stanza reads "xPUT …", no request line; the second
		// element's own text is one.
		"later element": wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "subject", "x"), wire.Text(wire.ClientNS, "subject", "PUT /gateway/update")),
	} {
		if !blocking.blockedFrame(wire.Frame{Element: &e}) {
			t.Errorf("%s passed block policy", name)
		}
	}
	read := wire.E(wire.ClientNS, "message", wire.Text(wire.ClientNS, "body", "GET /gateway/update HTTP/1.1\r\n\r\n"))
	if blocking.blockedFrame(wire.Frame{Element: &read}) {
		t.Error("update read blocked")
	}
}

// splitIn wraps an update write in name, with markup inside its path.
func splitIn(name, space string) wire.Element {
	e := wire.Text(space, name, "PUT /gateway/")
	e.Children = append(e.Children, wire.Node{Element: ptrElement(wire.Text("", "x", "update"))}, wire.Node{Text: " HTTP/1.1"})
	return e
}

func TestUpdatePolicyCostIsLinear(t *testing.T) {
	// Deep nesting must not make the policy rescan the text at every level:
	// one hostile stanza within the reader's limits could pin the bridge.
	inner := wire.Text(wire.ClientNS, "x", strings.Repeat("a\n", 30000))
	e := inner
	for range 60 {
		e = wire.E(wire.ClientNS, "x", e)
	}
	frame := wire.Frame{Element: &e}
	blocking := &localTransport{options: LocalOptions{UpdatePolicy: UpdatesBlock}}
	if allocs := testing.AllocsPerRun(5, func() { blocking.blockedFrame(frame) }); allocs > 16 {
		t.Fatalf("%v allocations per check", allocs)
	}
}
