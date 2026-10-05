package server

import (
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/kradalby/nefit-go/client"
	"github.com/kradalby/nefit-go/crypto"
	wire "github.com/kradalby/nefit-go/xmpp"
)

func TestDNSSRVMatchesListener(t *testing.T) {
	for _, override := range []uint16{0, 15222} {
		t.Run(fmt.Sprint(override), func(t *testing.T) {
			dnsConfig := &DNSConfig{ListenAddress: "127.0.0.1:0", Hostname: "original.example", Address: netip.MustParseAddr("127.0.0.1"), DeviceIP: netip.MustParseAddr("127.0.0.1"), XMPPPort: override}
			s, err := New(Config{Device: client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret"}, DeviceIP: net.ParseIP("127.0.0.1"), ListenAddress: "127.0.0.1:0", DNS: dnsConfig})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			name, err := dnsmessage.NewName("_xmpp-client._tcp.original.example.")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := (&dnsmessage.Message{Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeSRV, Class: dnsmessage.ClassINET}}}).Pack()
			if err != nil {
				t.Fatal(err)
			}
			answer, err := s.dns.answer(raw)
			if err != nil {
				t.Fatal(err)
			}
			var msg dnsmessage.Message
			if err := msg.Unpack(answer); err != nil {
				t.Fatal(err)
			}
			expected := override
			if expected == 0 {
				expected = uint16(s.LocalAddress().(*net.TCPAddr).Port)
			}
			if len(msg.Answers) != 1 || msg.Answers[0].Body.(*dnsmessage.SRVResource).Port != expected {
				t.Fatalf("wrong SRV port, expected %d: %+v", expected, msg)
			}
			if dnsConfig.XMPPPort != override {
				t.Fatal("caller DNS configuration mutated")
			}
		})
	}
}

func TestRawHTTPPreservesJSONString(t *testing.T) {
	cfg := client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret", RetryTimeout: time.Second, ConnectTimeout: time.Second, PingInterval: time.Hour}.WithDefaults()
	c, err := client.NewLocalClient(cfg, client.LocalOptions{DeviceIP: net.ParseIP("127.0.0.1"), ListenAddress: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	conn, err := net.Dial("tcp", c.LocalAddress().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	r := wire.NewReader(conn)
	send := func(text string) {
		t.Helper()
		if _, err := io.WriteString(conn, text); err != nil {
			t.Fatal(err)
		}
	}
	next := func() wire.Frame {
		t.Helper()
		f, err := r.Next()
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	header := fmt.Sprintf(`<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" from="rrcgateway_%s" to="%s">`, cfg.SerialNumber, cfg.Host)
	send(header)
	next()
	next()
	send(`<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="DIGEST-MD5"/>`)
	next()
	send(`<response xmlns="urn:ietf:params:xml:ns:xmpp-sasl">eA==</response>`)
	next()
	send(header)
	next()
	next()
	send(`<iq type="set" id="b"><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"><resource>RRC-RestApi</resource></bind></iq>`)
	next()
	send(`<iq type="set" id="s"><session xmlns="urn:ietf:params:xml:ns:xmpp-session"/></iq>`)
	next()
	send(`<presence/>`)
	if err := <-connected; err != nil {
		t.Fatal(err)
	}

	h := NewHandler(c, time.Second)
	for _, methodPath := range []struct{ method, path string }{
		{"PUT", "/test"}, {"PUT", "/bridge/test"}, {"POST", "/bridge/test"}, {"PUT", "/api/temperature"}, {"PUT", "/api/user-mode"}, {"PUT", "/api/hot-water"},
	} {
		for _, contentType := range []string{"application/json", "text/plain"} {
			request := httptest.NewRequest(methodPath.method, "http://127.0.0.1:8088"+methodPath.path, strings.NewReader(`{"value":30}`))
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Origin", "https://other.example")
			request.Header.Set("Sec-Fetch-Site", "cross-site")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, request)
			if rec.Code != 403 {
				t.Fatalf("cross-origin %s %s returned %d", methodPath.method, methodPath.path, rec.Code)
			}
		}
	}
	for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		request := httptest.NewRequest("POST", "/bridge/test", strings.NewReader(`{"value":30}`))
		request.Header.Set("Content-Type", contentType)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, request)
		if rec.Code != 415 {
			t.Fatalf("unsupported content type %q returned %d", contentType, rec.Code)
		}
	}
	for _, test := range []struct{ method, path, body, origin string }{
		{"PUT", "/test", `"hello"`, ""},
		{"POST", "/bridge/test", `"hello"`, "http://127.0.0.1:8088"},
		{"PUT", "/bridge/test", `9007199254740993`, ""},
		{"PUT", "/test", `{"value":9007199254740993}`, ""},
		{"PUT", "/test", `null`, ""},
		{"PUT", "/test", `true`, ""},
	} {
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		request := httptest.NewRequest(test.method, "http://127.0.0.1:8088"+test.path, strings.NewReader(test.body))
		request.Header.Set("Content-Type", "application/json; charset=utf-8")
		if test.origin != "" {
			request.Header.Set("Origin", test.origin)
			request.Header.Set("Sec-Fetch-Site", "same-origin")
		}
		go func() { h.ServeHTTP(rec, request); close(done) }()
		requestFrame := next()
		body := requestFrame.Element.Child(wire.ClientNS, "body").Text()
		if !strings.HasPrefix(body, "PUT /test ") {
			t.Fatal("rejected browser request reached device", body)
		}
		_, cipher, ok := strings.Cut(body, "\n\n")
		if !ok {
			t.Fatal("invalid request body", body)
		}
		enc, err := crypto.NewEncryptor(cfg.SerialNumber, cfg.AccessKey, cfg.Password)
		if err != nil {
			t.Fatal(err)
		}
		plain, err := enc.DecryptAndStrip(cipher)
		if err != nil {
			t.Fatal(err)
		}
		send(fmt.Sprintf(`<message to="%s/localprobe" type="chat"><body>HTTP/1.0 204 No Content&#13;&#10;&#13;&#10;</body></message>`, cfg.JID()))
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("handler stalled")
		}
		if rec.Code != 200 || plain != test.body {
			t.Fatalf("JSON %s became %s, status %d", test.body, plain, rec.Code)
		}
	}
}
