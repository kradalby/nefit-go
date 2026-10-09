package server

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/kradalby/nefit-go/client"
	"github.com/kradalby/nefit-go/crypto"
	wire "github.com/kradalby/nefit-go/internal/xmpp"
	"github.com/kradalby/nefit-go/protocol"
)

var testDevice = client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret", RetryTimeout: time.Second, ConnectTimeout: time.Second, PingInterval: time.Hour}.WithDefaults()

// offlineClient sees a device only once loginFakeDevice logs one in; until
// then a request that reaches it times out with 504 instead of the status a
// boundary check should produce.
func offlineClient(t *testing.T, options client.LocalOptions) *client.Client {
	t.Helper()
	options.ListenAddress, options.DeviceIP = "127.0.0.1:0", netip.MustParseAddr("127.0.0.1")
	c, err := client.NewLocalClient(testDevice, options)
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func newHandler(t *testing.T, c *client.Client, timeout time.Duration, hosts ...string) http.Handler {
	t.Helper()
	h, err := NewHandler(c, timeout, hosts...)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHTTPRequestBoundaries(t *testing.T) {
	h := newHandler(t, offlineClient(t, client.LocalOptions{}), 50*time.Millisecond)
	large := `"` + strings.Repeat("a", maxBodyBytes) + `"`
	for _, test := range []struct {
		method, target, body, contentType string
		headers                           map[string]string
		code                              int
		allow                             string
	}{
		{method: "GET", target: "/healthz", code: 503},
		{method: "GET", target: "/x", code: 504},
		{method: "PUT", target: "/api/status", code: 405, allow: "GET, HEAD"},
		{method: "DELETE", target: "/api/status", code: 405, allow: "GET, HEAD"},
		{method: "POST", target: "/api/hot-water", code: 405, allow: "GET, HEAD, PUT"},
		{method: "GET", target: "/api/temperature", code: 405, allow: "PUT"},
		{method: "PUT", target: "/healthz", code: 405, allow: "GET, HEAD"},
		{method: "PUT", target: "/api/unknown", body: "{}", code: 404},
		{method: "GET", target: "/api", code: 404},
		{method: "PUT", target: "/api%2fstatus", body: "{}", code: 404},
		{method: "DELETE", target: "/test", code: 405, allow: "GET, HEAD, PUT"},
		{method: "DELETE", target: "/bridge/ecus/rrc/uiStatus", code: 405, allow: "GET, HEAD, POST, PUT"},
		{method: "GET", target: "/x?\xff", code: 400},
		{method: "PUT", target: "/x?\xff", body: "1", code: 400},
		{method: "PUT", target: "/heatingCircuits/hc1/temperatureRoomManual", body: "{", code: 400},
		{method: "POST", target: "/bridge/ecus/rrc/usermode", body: `{"value":"manual"} {"value":"clock"}`, code: 400},
		{method: "PUT", target: "/api/temperature", body: `{}`, code: 400},
		{method: "PUT", target: "/api/temperature", body: `{"value":null}`, code: 400},
		{method: "PUT", target: "/api/temperature", body: `{"value":"warm"}`, code: 400},
		{method: "PUT", target: "/api/temperature", body: `{"value":40}`, code: 400},
		{method: "PUT", target: "/api/user-mode", body: `{"value":"off"}`, code: 400},
		{method: "PUT", target: "/api/user-mode", body: `{"value":"manual"} 1`, code: 400},
		{method: "PUT", target: "/api/user-mode", body: `{"value":"manual"}x`, code: 400},
		{method: "PUT", target: "/api/hot-water", body: `{"value":1}`, code: 400},
		// null would otherwise decode as false and switch hot water off.
		{method: "PUT", target: "/api/hot-water", body: `{"value":null}`, code: 400},
		{method: "PUT", target: "/x", body: large, code: 413},
		{method: "PUT", target: "/x", body: "1", contentType: "text/json", code: 415},
		{method: "POST", target: "/bridge/x", body: "1", contentType: "text/plain", code: 415},
		{method: "POST", target: "/bridge/x", body: "1", contentType: "application/x-www-form-urlencoded", code: 415},
		{method: "POST", target: "/bridge/x", body: "1", contentType: "-", code: 415},
		{method: "GET", target: "/healthz", headers: map[string]string{"Sec-Fetch-Site": "same-origin"}, code: 503},
		{method: "GET", target: "/healthz", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, code: 403},
		{method: "HEAD", target: "/x", headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, code: 403},
		{method: "PUT", target: "/api/hot-water", body: `{"value":true}`, headers: map[string]string{"Sec-Fetch-Site": "cross-site"}, code: 403},
		{method: "PUT", target: "/x", body: "1", headers: map[string]string{"Sec-Fetch-Site": "same-site"}, code: 403},
		{method: "POST", target: "/bridge/x", body: "1", headers: map[string]string{"Origin": "https://other.example"}, code: 403},
	} {
		t.Run(test.method+" "+test.target+" "+fmt.Sprint(test.headers)+test.contentType, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "http://localhost"+test.target, strings.NewReader(test.body))
			contentType := "application/json"
			if test.contentType != "" {
				contentType = strings.TrimPrefix(test.contentType, "-")
			}
			r.Header.Set("Content-Type", contentType)
			for key, value := range test.headers {
				r.Header.Set(key, value)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != test.code || w.Header().Get("Allow") != test.allow {
				t.Fatalf("status %d Allow=%q, want %d %q: %s", w.Code, w.Header().Get("Allow"), test.code, test.allow, w.Body.String())
			}
			// Routing errors come from ServeMux as text; everything else is JSON.
			if test.code == 404 || test.code == 405 || test.method == "HEAD" {
				return
			}
			var body map[string]any
			if w.Header().Get("Content-Type") != "application/json" || json.Unmarshal(w.Body.Bytes(), &body) != nil {
				t.Fatalf("response is not JSON: %q", w.Body.String())
			}
			if _, ok := body["error"]; ok != (test.code != 503) {
				t.Fatalf("error field mismatch: %s", w.Body.String())
			}
		})
	}
}

func TestHTTPErrorStatus(t *testing.T) {
	for _, test := range []struct {
		err  error
		code int
	}{
		{fmt.Errorf("PUT failed: %w", client.ErrUpdateBlocked), 403},
		{fmt.Errorf("GET failed: %w", context.DeadlineExceeded), 504},
		{fmt.Errorf("x: %w", protocol.ErrInvalidURI), 400},
		{inputError("bad"), 400},
		{fmt.Errorf("PUT failed: %w", client.ErrInvalidValue), 400},
		{errors.New("HTTP error 500"), 502},
	} {
		if code := errorStatus(test.err); code != test.code {
			t.Errorf("%v: status %d, want %d", test.err, code, test.code)
		}
	}
}

func TestHTTPHostBoundary(t *testing.T) {
	h := newHandler(t, offlineClient(t, client.LocalOptions{}), time.Second, "proxy.example", "specific.example:8443", "NEFIT.local.")
	for _, test := range []struct {
		host, local, forwarded string
		allowed                bool
	}{
		{host: "localhost:8088", allowed: true},
		{host: "LOCALHOST.", allowed: true},
		{host: "127.0.0.1:8088", allowed: true},
		{host: "[::1]:8088", allowed: true},
		{host: "[::ffff:127.0.0.1]:8088", allowed: true},
		{host: "192.0.2.27:8088", local: "192.0.2.27:8088", allowed: true},
		{host: "192.0.2.27:8088", local: "[::ffff:192.0.2.27]:8088", allowed: true},
		{host: "[::ffff:192.0.2.27]:8088", local: "192.0.2.27:8088", allowed: true},
		{host: "192.0.2.28:8088", local: "192.0.2.27:8088"},
		{host: "0.0.0.0:8088", local: "0.0.0.0:8088"},
		{host: "[::]:8088", local: "[::]:8088"},
		{host: "[::ffff:0.0.0.0]:8088"},
		{host: "[fe80::1%25eth0]:8088", local: "[fe80::1%eth0]:8088"},
		{host: "proxy.example:8443", allowed: true},
		{host: "specific.example:8443", allowed: true},
		{host: "specific.example:08443", allowed: true},
		{host: "specific.example:8088"},
		{host: "specific.example:70000"},
		{host: "specific.example:"},
		{host: "nefit.local:8088", allowed: true},
		{host: "néfit.local:8088"},
		{host: "attacker.example:8088"},
		{host: "attacker.example:8088", forwarded: "localhost:8088"},
		{host: "localhost.attacker.example"},
		{host: "localhost@attacker.example"},
		{host: "localhost:bad"},
		{host: "localhost\r\nX-Test: injected"},
		{host: ""},
	} {
		t.Run(test.host+test.local+test.forwarded, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/healthz", nil)
			r.Host = test.host
			r.Header.Set("X-Forwarded-Host", test.forwarded)
			if test.local != "" {
				addr, err := net.ResolveTCPAddr("tcp", test.local)
				if err != nil {
					t.Fatal(err)
				}
				r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, addr))
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := http.StatusForbidden
			if test.allowed {
				want = http.StatusServiceUnavailable
			}
			if w.Code != want {
				t.Fatalf("host boundary returned %d, want %d", w.Code, want)
			}
		})
	}
}

func TestNewHandlerRejectsInvalidAllowedHosts(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "[::]", "nefit.example:70000", "néfit.example", "nefit.example/path", "user@nefit.example", "[fe80::1%eth0]"} {
		if _, err := NewHandler(nil, time.Second, "nefit.example", host); err == nil {
			t.Errorf("allowed host %q accepted", host)
		}
	}
	if _, err := NewHandler(nil, time.Second, "nefit.local.", "proxy.example:8443", "[::1]", "192.0.2.20"); err != nil {
		t.Fatal(err)
	}
}

type delayedHTTPBody struct {
	io.Reader
	delay time.Duration
}

func (r *delayedHTTPBody) Read(p []byte) (int, error) {
	if r.delay != 0 {
		time.Sleep(r.delay)
		r.delay = 0
	}
	return r.Reader.Read(p)
}

func TestHTTPDeadlineIncludesBodyDecoding(t *testing.T) {
	// A nil client panics on any device call, so a late body reaching it
	// fails loudly instead of receiving a fresh deadline.
	h := newHandler(t, nil, 20*time.Millisecond)
	for _, path := range []string{"/api/user-mode", "/api/temperature", "/api/hot-water", "/bridge/test", "/test"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest("PUT", "http://localhost"+path, &delayedHTTPBody{Reader: strings.NewReader(`{"value":"manual"}`), delay: 60 * time.Millisecond})
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusRequestTimeout {
				t.Fatalf("expired body reached backend: status %d", w.Code)
			}
		})
	}
}

// rawExchange writes request text and reads one response on a fresh connection.
func rawExchange(t *testing.T, address, method, request string) *http.Response {
	t.Helper()
	socket, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	_ = socket.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(socket, request); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(socket), &http.Request{Method: method})
	if err != nil {
		t.Fatal("no prompt response", err)
	}
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal("incomplete response", err)
	}
	return response
}

func TestHTTPRejectionsDoNotWaitForBody(t *testing.T) {
	t.Parallel()
	// No ReadTimeout: only an immediate answer passes the socket deadline.
	s := httptest.NewServer(newHandler(t, nil, time.Second))
	t.Cleanup(s.Close)
	address := s.Listener.Addr().String()
	for _, test := range []struct {
		name, method, path, host, headers, contentType, body string
		status                                               int
	}{
		{"host", "PUT", "/api/user-mode", "untrusted.example", "", "application/json", "{", 403},
		{"cross-site", "PUT", "/api/user-mode", "localhost", "Sec-Fetch-Site: cross-site\r\n", "application/json", "{", 403},
		{"unknown API", "PUT", "/api/unknown", "localhost", "", "application/json", "{", 404},
		{"API method", "POST", "/api/status", "localhost", "", "application/json", "{", 405},
		{"raw method", "DELETE", "/test", "localhost", "", "application/json", "{", 405},
		{"malformed JSON", "PUT", "/api/user-mode", "localhost", "", "application/json", "!", 400},
		{"extra JSON", "PUT", "/api/user-mode", "localhost", "", "application/json", "{} {}", 400},
		{"media type", "PUT", "/api/user-mode", "localhost", "", "text/plain", "{", 415},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			response := rawExchange(t, address, test.method, fmt.Sprintf("%s %s HTTP/1.1\r\nHost: %s\r\n%sContent-Type: %s\r\nContent-Length: 100\r\n\r\n%s", test.method, test.path, test.host, test.headers, test.contentType, test.body))
			if response.StatusCode != test.status {
				t.Fatalf("status %d, want %d", response.StatusCode, test.status)
			}
		})
	}
}

func TestHTTPReadTimeoutBoundsSlowBody(t *testing.T) {
	t.Parallel()
	s := httptest.NewUnstartedServer(newHandler(t, nil, time.Minute))
	s.Config.ReadTimeout = 50 * time.Millisecond
	s.Start()
	t.Cleanup(s.Close)
	address := s.Listener.Addr().String()
	response := rawExchange(t, address, "PUT", fmt.Sprintf("PUT /api/user-mode HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", address))
	if response.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("slow body returned %d", response.StatusCode)
	}
}

func TestHTTPCompleteBodyPreservesKeepAlive(t *testing.T) {
	s := httptest.NewServer(newHandler(t, nil, time.Second))
	t.Cleanup(s.Close)
	address := s.Listener.Addr().String()
	socket, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	_ = socket.SetDeadline(time.Now().Add(time.Second))
	reader := bufio.NewReader(socket)
	for range 2 {
		body := `{"value":"invalid"}`
		if _, err = fmt.Fprintf(socket, "PUT /api/user-mode HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", address, len(body), body); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(reader, &http.Request{Method: "PUT"})
		if err != nil {
			t.Fatal("complete body lost keep-alive", err)
		}
		if response.StatusCode != http.StatusBadRequest || response.Close {
			t.Fatalf("complete body response %d close=%v", response.StatusCode, response.Close)
		}
		if _, err := io.ReadAll(response.Body); err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
	}
}

// fakeDevice is the thermostat end of a local session; its helpers must run
// on the test goroutine.
type fakeDevice struct {
	t      *testing.T
	cfg    client.Config
	conn   net.Conn
	reader *wire.Reader
	enc    *crypto.Encryptor
}

// loginFakeDevice logs a device in to c and waits for the session.
func loginFakeDevice(t *testing.T, c *client.Client, cfg client.Config) *fakeDevice {
	t.Helper()
	enc, err := crypto.NewEncryptor(cfg.SerialNumber, cfg.AccessKey, cfg.Password)
	if err != nil {
		t.Fatal(err)
	}
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	conn, err := net.DialTimeout("tcp", c.LocalAddress().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	d := &fakeDevice{t: t, cfg: cfg, conn: conn, reader: wire.NewReader(conn), enc: enc}
	header := fmt.Sprintf(`<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" from="rrcgateway_%s" to="%s">`, cfg.SerialNumber, cfg.Host)
	for _, step := range []struct {
		send    string
		replies int
	}{
		{header, 2},
		{`<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="DIGEST-MD5"/>`, 1},
		{`<response xmlns="urn:ietf:params:xml:ns:xmpp-sasl">eA==</response>`, 1},
		{header, 2},
		{`<iq type="set" id="b"><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"><resource>RRC-RestApi</resource></bind></iq>`, 1},
		{`<iq type="set" id="s"><session xmlns="urn:ietf:params:xml:ns:xmpp-session"/></iq>`, 1},
		{`<presence/>`, 0},
	} {
		d.send(step.send)
		for range step.replies {
			d.next()
		}
	}
	select {
	case err := <-connected:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("local session did not connect")
	}
	return d
}

func (d *fakeDevice) send(text string) {
	d.t.Helper()
	if _, err := io.WriteString(d.conn, text); err != nil {
		d.t.Fatal(err)
	}
}

func (d *fakeDevice) next() wire.Frame {
	d.t.Helper()
	f, err := d.reader.Next()
	if err != nil {
		d.t.Fatal(err)
	}
	return f
}

// request returns the next device request's line and decrypted body.
func (d *fakeDevice) request() (line, body string) {
	d.t.Helper()
	// XML parsing reads the device encoder's literal CRLF as LF.
	text := d.next().Element.Child(wire.ClientNS, "body").Text()
	line, _, _ = strings.Cut(text, "\n")
	if _, cipher, ok := strings.Cut(text, "\n\n"); ok && cipher != "" {
		plain, err := d.enc.DecryptAndStrip(cipher)
		if err != nil {
			d.t.Fatal(err)
		}
		body = plain
	}
	return line, body
}

func (d *fakeDevice) reply(status, json string) {
	d.t.Helper()
	text := "HTTP/1.0 " + status + "\r\n\r\n"
	if json != "" {
		cipher, err := d.enc.Encrypt(json)
		if err != nil {
			d.t.Fatal(err)
		}
		text = "HTTP/1.0 " + status + "\r\nContent-Type: application/json\r\n\r\n" + cipher
	}
	raw, err := xml.Marshal(wire.Message{To: d.cfg.JID() + "/localprobe", Type: "chat", Body: wire.Body{Text: text}})
	if err != nil {
		d.t.Fatal(err)
	}
	d.send(string(raw))
}

// serveAsync runs the handler while the test goroutine plays the device.
func serveAsync(h http.Handler, r *http.Request) func(t *testing.T) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, r); close(done) }()
	return func(t *testing.T) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case <-done:
			return w
		case <-time.After(2 * time.Second):
			t.Fatal("handler stalled")
			return nil
		}
	}
}

func TestHTTPDeviceRequests(t *testing.T) {
	c := offlineClient(t, client.LocalOptions{UpdatePolicy: client.UpdatesBlock})
	device := loginFakeDevice(t, c, testDevice)
	h := newHandler(t, c, time.Second)
	newRequest := func(method, target, body string) *http.Request {
		r := httptest.NewRequest(method, "http://127.0.0.1:8088"+target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		return r
	}

	t.Run("raw writes", func(t *testing.T) {
		for _, test := range []struct{ method, target, body, sent string }{
			{"PUT", "/test", `"hello"`, `"hello"`},
			{"PUT", "/test", `"<a & b>"`, `"<a & b>"`},
			{"PUT", "/test", `"<"`, `"<"`},
			{"POST", "/bridge/test", ` { "value" : 9007199254740993 } `, `{"value":9007199254740993}`},
			{"PUT", "/bridge/test", `9007199254740993`, `9007199254740993`},
			{"PUT", "/test", `null`, `null`},
			{"PUT", "/test", `true`, `true`},
		} {
			wait := serveAsync(h, newRequest(test.method, test.target, test.body))
			line, sent := device.request()
			device.reply("204 No Content", "")
			w := wait(t)
			if line != "PUT /test HTTP/1.1" || sent != test.sent || w.Code != 200 {
				t.Fatalf("%s %s %s: device got %q %q, status %d", test.method, test.target, test.body, line, sent, w.Code)
			}
		}
	})

	t.Run("raw reads", func(t *testing.T) {
		for _, test := range []struct{ method, target, line string }{
			{"GET", "/x?y=1", "GET /x?y=1 HTTP/1.1"},
			{"GET", "/bridge/a%2Fb", "GET /a%2Fb HTTP/1.1"},
			{"HEAD", "/bridge/test", "GET /test HTTP/1.1"},
		} {
			// A HEAD body must not turn the read into a write.
			wait := serveAsync(h, newRequest(test.method, test.target, `{"value":30}`))
			line, _ := device.request()
			// Replies name their resource; the client matches on it.
			path, _, _ := strings.Cut(strings.Fields(line + " ?")[1], "?")
			device.reply("200 OK", fmt.Sprintf(`{"id":%q,"value":"<b>"}`, path))
			w := wait(t)
			if line != test.line || w.Code != 200 {
				t.Fatalf("%s %s: device got %q, status %d", test.method, test.target, line, w.Code)
			}
			if test.method == "GET" && !strings.Contains(w.Body.String(), `"<b>"`) {
				t.Fatalf("device text escaped: %s", w.Body.String())
			}
		}
	})

	t.Run("api writes", func(t *testing.T) {
		status := `{"id":"/ecus/rrc/uiStatus","value":{"UMD":"clock"}}`
		for _, test := range []struct {
			target, body string
			exchange     []string // device request, then its reply ("" for 204)
		}{
			{"/api/temperature", `{"value":21.5}`, []string{
				`PUT /heatingCircuits/hc1/temperatureRoomManual {"value":21.5}`, "",
				`PUT /heatingCircuits/hc1/manualTempOverride/status {"value":"on"}`, "",
				`PUT /heatingCircuits/hc1/manualTempOverride/temperature {"value":21.5}`, "",
			}},
			{"/api/user-mode", `{"value":"clock"}`, []string{`PUT /heatingCircuits/hc1/usermode {"value":"clock"}`, ""}},
			{"/api/hot-water", `{"value":false}`, []string{
				"GET /ecus/rrc/uiStatus ", status,
				`PUT /dhwCircuits/dhwA/dhwOperationClockMode {"value":"off"}`, "",
			}},
		} {
			wait := serveAsync(h, newRequest("PUT", test.target, test.body))
			for i := 0; i < len(test.exchange); i += 2 {
				line, sent := device.request()
				if got := strings.TrimSuffix(line, "HTTP/1.1") + sent; got != test.exchange[i] {
					t.Fatalf("%s: device got %q, want %q", test.target, got, test.exchange[i])
				}
				if test.exchange[i+1] == "" {
					device.reply("204 No Content", "")
				} else {
					device.reply("200 OK", test.exchange[i+1])
				}
			}
			if w := wait(t); w.Code != 200 {
				t.Fatalf("%s: status %d %s", test.target, w.Code, w.Body.String())
			}
		}
	})

	t.Run("healthz", func(t *testing.T) {
		// Offline: the device is connected, Bosch is not.
		w := serveAsync(h, newRequest("GET", "/healthz", ""))(t)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"connected":true`) || !strings.Contains(w.Body.String(), `"upstreamConnected":false`) {
			t.Fatalf("status %d %s", w.Code, w.Body.String())
		}
	})

	t.Run("api reads", func(t *testing.T) {
		status := `{"id":"/ecus/rrc/uiStatus","value":{"UMD":"clock"}}`
		for _, test := range []struct {
			target, want string
			exchange     []string // device request line, then its reply
		}{
			{"/api/pressure", `"pressure":1.5`, []string{"GET /system/appliance/systemPressure", `{"id":"/system/appliance/systemPressure","value":1.5}`}},
			{"/api/hot-water", "true", []string{
				"GET /ecus/rrc/uiStatus", status,
				"GET /dhwCircuits/dhwA/dhwOperationClockMode", `{"id":"/dhwCircuits/dhwA/dhwOperationClockMode","value":"on"}`,
			}},
			{"/api/status?outdoor=true", `"outdoor_temp":7`, []string{
				"GET /ecus/rrc/uiStatus", status,
				"GET /system/sensors/temperatures/outdoor_t1", `{"id":"/system/sensors/temperatures/outdoor_t1","value":7}`,
			}},
		} {
			wait := serveAsync(h, newRequest("GET", test.target, ""))
			for i := 0; i < len(test.exchange); i += 2 {
				if line, _ := device.request(); line != test.exchange[i]+" HTTP/1.1" {
					t.Fatalf("%s: device got %q, want %q", test.target, line, test.exchange[i])
				}
				device.reply("200 OK", test.exchange[i+1])
			}
			if w := wait(t); w.Code != 200 || !strings.Contains(w.Body.String(), test.want) {
				t.Fatalf("%s: status %d %s", test.target, w.Code, w.Body.String())
			}
		}
	})

	t.Run("blocked update write", func(t *testing.T) {
		w := serveAsync(h, newRequest("PUT", "/gateway/update/strategy", `"auto"`))(t)
		if w.Code != http.StatusForbidden {
			t.Fatalf("blocked write returned %d: %s", w.Code, w.Body.String())
		}
		// The next frame must be the following request, not the blocked write.
		wait := serveAsync(h, newRequest("GET", "/probe", ""))
		if line, _ := device.request(); line != "GET /probe HTTP/1.1" {
			t.Fatalf("blocked write reached device: %q", line)
		}
		device.reply("200 OK", `{"id":"/probe","value":1}`)
		wait(t)
	})
}
