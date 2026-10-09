package client

import (
	"bufio"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
)

// newLocalTestClient also defaults to a loopback device and an ephemeral listener.
func newLocalTestClient(t *testing.T, cfg Config, options LocalOptions) *Client {
	t.Helper()
	options.ListenAddress = cmp.Or(options.ListenAddress, "127.0.0.1:0")
	if !options.DeviceIP.IsValid() {
		options.DeviceIP = netip.MustParseAddr("127.0.0.1")
	}
	c, err := NewLocalClient(testConfig(cfg), options)
	return testClient(t, c, err)
}

func localTestClient(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	return newLocalTestClient(t, Config{ConnectTimeout: timeout, RetryTimeout: time.Second}, LocalOptions{})
}

type deviceFixture struct {
	socket      net.Conn
	reader      *bufio.Reader
	afterWrite  func(string)
	beforeWrite func(string)
	checkRead   func(string)
}

func (d *deviceFixture) readThrough(t *testing.T, end string) string {
	t.Helper()
	var text strings.Builder
	for !strings.Contains(text.String(), end) {
		part, err := d.reader.ReadString('>')
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(part)
	}
	if d.checkRead != nil {
		d.checkRead(text.String())
	}
	return text.String()
}

func (d *deviceFixture) write(t *testing.T, text string) {
	t.Helper()
	if d.beforeWrite != nil {
		d.beforeWrite(text)
	}
	if _, err := io.WriteString(d.socket, text); err != nil {
		t.Fatal(err)
	}
	if d.afterWrite != nil {
		d.afterWrite(text)
	}
}

// loginSteps is the device side of a successful login: each stanza sent and
// text that ends the server's answer. The final presence gets none.
func loginSteps(c *Client) []struct{ send, end string } {
	header := fmt.Sprintf(`<stream:stream from="rrcgateway_%s" to="%s" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`, c.config.SerialNumber, c.config.Host)
	return []struct{ send, end string }{
		{header, "</stream:features>"},
		{`<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="DIGEST-MD5"/>`, "</challenge>"},
		{`<response xmlns="urn:ietf:params:xml:ns:xmpp-sasl">` + base64.StdEncoding.EncodeToString([]byte("proof")) + `</response>`, "</success>"},
		{header, "</stream:features>"},
		{`<iq type="set" id="bind_1"><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"><resource>RRC-RestApi</resource></bind></iq>`, "</iq>"},
		// The acknowledgement layout the firmware accepted in captures.
		{`<iq type="set" id="sess_1"><session xmlns="urn:ietf:params:xml:ns:xmpp-session"/></iq>`, `<iq type="result" id="sess_1"`},
		{`<presence><status>RRC</status></presence>`, ""},
	}
}

func connectFixture(t *testing.T, c *Client) *deviceFixture {
	t.Helper()
	return connectHandshakeFixture(t, c, nil)
}

func connectHandshakeFixture(t *testing.T, c *Client, hook func(*deviceFixture)) *deviceFixture {
	t.Helper()
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	s, err := net.Dial("tcp", c.LocalAddress().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	d := &deviceFixture{socket: s, reader: bufio.NewReader(s)}
	if hook != nil {
		hook(d)
	}
	for _, step := range loginSteps(c) {
		d.write(t, step.send)
		if step.end != "" {
			d.readThrough(t, step.end)
		}
	}
	if err := wait(t, connected); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *deviceFixture) reply(t *testing.T, c *Client, body string) {
	t.Helper()
	d.write(t, fmt.Sprintf(`<message to="%s/localprobe" type="chat"><body>%s</body></message>`, c.config.JID(), body))
}

func TestLocalClientReadsWritesAndReconnects(t *testing.T) {
	t.Parallel()
	c := localTestClient(t, time.Second)
	d := connectFixture(t, c)
	for cycle := 0; cycle < 2; cycle++ {
		result := make(chan any, 1)
		go func() {
			value, err := c.Get(t.Context(), "/ecus/rrc/uiStatus")
			if err != nil {
				result <- err
			} else {
				result <- value
			}
		}()
		request := d.readThrough(t, "</message>")
		if !strings.Contains(request, "GET /ecus/rrc/uiStatus HTTP/1.1\r\nUser-Agent: NefitEasy\r\n\r\n") {
			t.Fatalf("incorrect device line endings: %q", request)
		}
		// An encrypted cloud-service message must never satisfy the API request.
		d.write(t, `<message to="gservice_time@example.com"><body>not an HTTP response</body></message>`)
		encoded, err := c.encryptor.Encrypt(`{"id":"/ecus/rrc/uiStatus","value":{"TSP":"14.0"}}`)
		if err != nil {
			t.Fatal(err)
		}
		d.reply(t, c, "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n"+encoded)
		value := wait(t, result)
		if err, ok := value.(error); ok {
			t.Fatal(err)
		}
		if value.(map[string]any)["value"].(map[string]any)["TSP"] != "14.0" {
			t.Fatalf("bad decrypted reply: %v", value)
		}
		written := make(chan error, 1)
		go func() {
			written <- c.Put(t.Context(), "/heatingCircuits/hc1/temperatureRoomManual", map[string]any{"value": 14.0})
		}()
		request = d.readThrough(t, "</message>")
		var stanza struct {
			Body string `xml:"body"`
		}
		if err := xml.Unmarshal([]byte(request), &stanza); err != nil {
			t.Fatal(err)
		}
		_, encrypted, ok := strings.Cut(stanza.Body, "\n\n")
		if !ok {
			t.Fatalf("invalid PUT: %q", stanza.Body)
		}
		plain, err := c.encryptor.DecryptAndStrip(encrypted)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(plain), &data); err != nil {
			t.Fatal(err)
		}
		if data["value"] != 14.0 {
			t.Fatalf("wrong encrypted PUT: %s", plain)
		}
		d.reply(t, c, "HTTP/1.0 204 No Content\r\nContent-Type: application/json\r\n\r\n")
		if err := wait(t, written); err != nil {
			t.Fatal(err)
		}
		if cycle == 0 {
			done := c.Done()
			_ = d.socket.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("disconnect not detected")
			}
			d = connectFixture(t, c)
		}
	}
}

func TestLocalClientHandshakeTimeout(t *testing.T) {
	t.Parallel()
	c := localTestClient(t, 100*time.Millisecond)
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(context.Background()) }()
	s, err := net.Dial("tcp", c.LocalAddress().String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck
	if err := wait(t, connected); err == nil {
		t.Fatal("silent handshake succeeded")
	}
}

func TestLocalHandshakeRejectsDeviations(t *testing.T) {
	t.Parallel()
	cfg := Config{SerialNumber: "123456789", Host: DefaultHost}
	header := `<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" from="rrcgateway_123456789" to="` + DefaultHost + `" version="1.0">`
	auth := `<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="DIGEST-MD5"/>`
	response := `<response xmlns="urn:ietf:params:xml:ns:xmpp-sasl">` + base64.StdEncoding.EncodeToString([]byte("proof")) + `</response>`
	bind := `<iq type="set" id="b"><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"><resource>RRC-RestApi</resource></bind></iq>`
	session := `<iq type="set" id="s"><session xmlns="urn:ietf:params:xml:ns:xmpp-session"/></iq>`
	authenticated := []string{header, auth, response, header}
	for name, steps := range map[string][]string{
		"wrong serial":      {`<stream:stream xmlns:stream="http://etherx.jabber.org/streams" from="rrcgateway_wrong" to="` + DefaultHost + `">`},
		"wrong domain":      {`<stream:stream xmlns:stream="http://etherx.jabber.org/streams" from="rrcgateway_123456789" to="other.example">`},
		"stanza first":      {`<presence/>`},
		"plain mechanism":   {header, `<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="PLAIN"/>`},
		"invalid response":  {header, auth, `<response xmlns="urn:ietf:params:xml:ns:xmpp-sasl">!</response>`},
		"early restart":     {header, header},
		"starttls":          {header, `<starttls xmlns="urn:ietf:params:xml:ns:xmpp-tls"/>`},
		"other resource":    append(append([]string{}, authenticated...), `<iq type="set" id="b"><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"><resource>Other</resource></bind></iq>`),
		"second bind":       append(append([]string{}, authenticated...), bind, bind),
		"session first":     append(append([]string{}, authenticated...), session),
		"presence too soon": append(append([]string{}, authenticated...), bind, `<presence/>`),
		"response first":    {header, response},
		"bind before login": {header, bind},
		"second auth":       {header, auth, auth},
	} {
		t.Run(name, func(t *testing.T) {
			server, device := net.Pipe()
			defer device.Close() //nolint:errcheck
			defer server.Close() //nolint:errcheck
			go func() { _, _ = io.Copy(io.Discard, device) }()
			result := make(chan error, 1)
			go func() {
				tr, err := acceptLocal(t.Context(), server, cfg, LocalOptions{RequestTimeout: time.Second})
				if tr != nil {
					_ = tr.Close()
				}
				result <- err
			}()
			for _, step := range steps {
				if _, err := io.WriteString(device, step); err != nil {
					break // the server gave up early, which the result shows
				}
			}
			if err := wait(t, result); err == nil {
				t.Fatal("deviating handshake accepted")
			}
		})
	}
}

func TestSocketWriterRetiredSessionIsUnsent(t *testing.T) {
	t.Parallel()
	socket, peer := net.Pipe()
	t.Cleanup(func() { _ = socket.Close(); _ = peer.Close() })
	done := make(chan struct{})
	close(done)
	w := newSocketWriter(socket, done, func() {}, time.Second)
	// A free slot and a closed done race in select; repeat for the check after.
	for range 64 {
		err := w.write(t.Context(), []byte("<presence/>"), nil, nil)
		var unsent *unsentError
		if !errors.As(err, &unsent) || !errors.Is(err, errSessionEnded) {
			t.Fatalf("write on a retired session = %v", err)
		}
	}
	_ = peer.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if n, _ := peer.Read(make([]byte, 1)); n != 0 {
		t.Fatal("retired session written")
	}
}

func TestSocketWriterDeadlineFailureIsUnsent(t *testing.T) {
	t.Parallel()
	socket, peer := net.Pipe()
	_ = peer.Close()
	_ = socket.Close()
	aborted := false
	w := newSocketWriter(socket, make(chan struct{}), func() { aborted = true }, time.Second)
	err := w.write(t.Context(), []byte("<presence/>"), nil, nil)
	var unsent *unsentError
	if !errors.As(err, &unsent) || !errors.Is(err, errSessionEnded) || !aborted {
		t.Fatalf("write on a dead socket = %v, aborted = %v; want an unsent session end", err, aborted)
	}
}

func TestSetLoggerWhileAdmitting(t *testing.T) {
	t.Parallel()
	c := localTestClient(t, time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 20 {
			if s, err := net.Dial("tcp", c.LocalAddress().String()); err == nil {
				_, _ = io.WriteString(s, `<presence/>`)
				_ = s.Close()
			}
		}
	}()
	for range 20 {
		c.SetLogger(slog.New(slog.DiscardHandler))
	}
	<-done
}

// relayMode sets "both" to relay to a fake Bosch and "both offline" to an
// unreachable one; any other mode stays offline.
func relayMode(t *testing.T, mode string, opts *LocalOptions) {
	t.Helper()
	switch mode {
	case "both":
		opts.Mode = ModeBoth
		opts.UpstreamAddress, _ = serve(t, func(_ int32, s net.Conn) {
			if fakeBosch(&cloudFixture{conn: s, reader: wire.NewReader(s)}, nil, nil) == nil {
				_ = s.SetDeadline(time.Time{})
				_, _ = io.Copy(io.Discard, s)
			}
		})
	case "both offline":
		opts.Mode, opts.UpstreamAddress = ModeBoth, unreachable
	}
}

func reconnectTestClient(t *testing.T, mode string) *Client {
	t.Helper()
	opts := LocalOptions{RequestTimeout: 10 * time.Second, ReconnectInterval: time.Hour}
	relayMode(t, mode, &opts)
	return newLocalTestClient(t, Config{ConnectTimeout: time.Second, RetryTimeout: 10 * time.Second}, opts)
}

func reconnectSocketClosed(t *testing.T, socket net.Conn) {
	t.Helper()
	_ = socket.SetReadDeadline(time.Now().Add(time.Second))
	_, err := socket.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("retired socket still readable")
	}
	if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("socket remained open")
	}
}

func TestLocalReconnectReplacesHalfOpenDevice(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"offline", "both", "both offline"} {
		t.Run(mode, func(t *testing.T) {
			c := reconnectTestClient(t, mode)
			oldDevice := connectFixture(t, c)
			old := c.conn.Load()
			oldDone := c.Done()
			pending := make(chan error, 1)
			go func() { pending <- c.Put(t.Context(), "/old", 1) }()
			if request := oldDevice.readThrough(t, "</message>"); !strings.Contains(request, "PUT /old ") {
				t.Fatal(request)
			}
			queued := make(chan error, 1)
			go func() { queued <- c.Put(t.Context(), "/replacement", 2) }()
			type loginResult struct {
				socket net.Conn
				err    error
			}
			loggedIn := make(chan loginResult, 1)
			go func() {
				socket, err := deviceLogin(c)
				loggedIn <- loginResult{socket, err}
			}()
			select {
			case <-oldDone:
			case <-time.After(time.Second):
				t.Fatal("replacement did not retire the half-open session")
			}
			login := wait(t, loggedIn)
			if login.err != nil {
				t.Fatal(login.err)
			}
			t.Cleanup(func() { _ = login.socket.Close() })
			if err := wait(t, pending); !errors.Is(err, errConnectionLost) {
				t.Fatal("old request did not wake with connection loss:", err)
			}
			if c.conn.Load() == old || !c.IsConnected() {
				t.Fatal("replacement session not published")
			}
			if c.UpstreamConnected() != (mode == "both") {
				t.Fatal("replacement lost authentication mode")
			}
			reconnectSocketClosed(t, oldDevice.socket)
			_ = login.socket.SetDeadline(time.Now().Add(time.Second))
			replacement := &deviceFixture{socket: login.socket, reader: bufio.NewReader(login.socket)}
			if request := replacement.readThrough(t, "</message>"); !strings.Contains(request, "PUT /replacement ") || strings.Contains(request, "PUT /old ") {
				t.Fatal("queued request did not use replacement exclusively:", request)
			}
			replacement.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
			if err := wait(t, queued); err != nil {
				t.Fatal(err)
			}
			if err := c.Connect(t.Context()); err != nil || c.conn.Load() == old {
				t.Fatal("Connect did not preserve replacement:", err)
			}
		})
	}
}

func reconnectCandidate(t *testing.T, c *Client) *deviceFixture {
	t.Helper()
	socket, err := net.DialTimeout("tcp", c.LocalAddress().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	_ = socket.SetDeadline(time.Now().Add(time.Second))
	return &deviceFixture{socket: socket, reader: bufio.NewReader(socket)}
}

func TestLocalReconnectCloseDuringLogin(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"offline", "both"} {
		t.Run(mode, func(t *testing.T) {
			c := reconnectTestClient(t, mode)
			device := connectFixture(t, c)
			old := c.conn.Load()
			candidate := reconnectCandidate(t, c)
			candidate.write(t, fmt.Sprintf(`<stream:stream from="rrcgateway_%s" to="%s" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`, c.config.SerialNumber, c.config.Host))
			candidate.readThrough(t, "</stream:features>")
			if c.conn.Load() != old || !old.alive() {
				t.Fatal("unfinished login displaced the authenticated device")
			}
			closed := make(chan error, 1)
			go func() { closed <- c.Close() }()
			if err := wait(t, closed); err != nil {
				t.Fatal(err)
			}
			reconnectSocketClosed(t, candidate.socket)
			reconnectSocketClosed(t, device.socket)
			if c.IsConnected() || !errors.Is(c.Connect(t.Context()), ErrClosed) {
				t.Fatal("Close left a usable session")
			}
			select {
			case <-old.ctx.Done():
			default:
				t.Fatal("Close did not wake Done")
			}
		})
	}
}

func TestLocalReconnectCancelledConnectKeepsAdmission(t *testing.T) {
	t.Parallel()
	c := reconnectTestClient(t, "offline")
	candidate := reconnectCandidate(t, c)
	candidate.write(t, fmt.Sprintf(`<stream:stream from="rrcgateway_%s" to="%s" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`, c.config.SerialNumber, c.config.Host))
	candidate.readThrough(t, "</stream:features>")
	ctx, cancel := context.WithCancel(t.Context())
	cancelled := make(chan error, 1)
	go func() { cancelled <- c.Connect(ctx) }()
	cancel()
	if err := wait(t, cancelled); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled Connect kept waiting:", err)
	}
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	// The first caller's cancellation must not abort the shared handshake.
	candidate.write(t, `<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="DIGEST-MD5"/>`)
	candidate.readThrough(t, "</challenge>")
	_ = candidate.socket.Close()
	device, err := deviceLogin(c)
	if err != nil {
		t.Fatal("listener did not recover from the abandoned handshake:", err)
	}
	t.Cleanup(func() { _ = device.Close() })
	if err := wait(t, connected); err != nil {
		t.Fatal(err)
	}
	if !c.IsConnected() {
		t.Fatal("later login not published")
	}
}

func TestLocalReconnectLoginTimeoutPreservesSession(t *testing.T) {
	t.Parallel()
	// Long enough for the first login on a loaded machine, short enough to
	// expire the stalled candidate within reconnectSocketClosed's wait.
	c := localTestClient(t, 500*time.Millisecond)
	device := connectFixture(t, c)
	old := c.conn.Load()
	candidate := reconnectCandidate(t, c)
	candidate.write(t, fmt.Sprintf(`<stream:stream from="rrcgateway_%s" to="%s" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`, c.config.SerialNumber, c.config.Host))
	candidate.readThrough(t, "</stream:features>")
	reconnectSocketClosed(t, candidate.socket)
	if c.conn.Load() != old || !old.alive() {
		t.Fatal("timed-out replacement retired the authenticated device")
	}
	written := make(chan error, 1)
	go func() { written <- c.Put(t.Context(), "/after-timeout", 1) }()
	device.readThrough(t, "</message>")
	device.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, written); err != nil {
		t.Fatal(err)
	}
}

type admissionEvent struct {
	at  time.Time
	err error
}

// Errors are injected at the listener boundary; successful accepts still use
// real loopback sockets and the complete gateway login.
type admissionListener struct {
	net.Listener
	steps     chan error
	events    chan admissionEvent
	closed    chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func (l *admissionListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case err := <-l.steps:
		select {
		case l.events <- admissionEvent{time.Now(), err}:
		case <-l.closed:
			return nil, net.ErrClosed
		}
		if err != nil {
			return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept4", err)}
		}
		return l.Listener.Accept()
	}
}

func (l *admissionListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		_ = l.Listener.Close()
	})
	return nil
}

func admissionClient(t *testing.T, mode string, steps ...error) (*Client, *admissionListener) {
	t.Helper()
	c := newTestClient(t, Config{ConnectTimeout: time.Hour, RetryTimeout: 5 * time.Second})
	options := LocalOptions{
		Mode: ModeOffline, DeviceIP: netip.MustParseAddr("127.0.0.1"), UpdatePolicy: UpdatesAllow,
		RequestTimeout: 5 * time.Second, ReconnectInterval: time.Hour,
	}
	relayMode(t, mode, &options)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &admissionListener{
		Listener: ln, steps: make(chan error, 32), events: make(chan admissionEvent, 32),
		closed: make(chan struct{}), done: make(chan struct{}),
	}
	for _, err := range steps {
		l.steps <- err
	}
	c.localListener, c.localMode, c.localReady = l, options.Mode, make(chan struct{})
	c.wg.Go(func() {
		defer close(l.done)
		c.acceptDevices(options)
	})
	return c, l
}

func admissionDialStarted(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		c.mu.Lock()
		waiting := c.dialing != nil
		c.mu.Unlock()
		if waiting {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatal("Connect never started waiting for admission")
		}
	}
}

type admissionNetError struct{ timeout, temporary bool }

func (e admissionNetError) Error() string   { return "injected listener error" }
func (e admissionNetError) Timeout() bool   { return e.timeout }
func (e admissionNetError) Temporary() bool { return e.temporary }

func TestLocalListenerRecoversAcceptErrors(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{
		syscall.EMFILE, syscall.ENFILE, syscall.EINTR, syscall.ECONNABORTED,
		syscall.EAGAIN, syscall.ENOMEM, syscall.ENOBUFS,
		syscall.EPROTO, syscall.ENETDOWN, syscall.EPERM,
		admissionNetError{timeout: true},
		admissionNetError{},
		errors.New("unknown accept failure"),
	} {
		t.Run(fmt.Sprintf("%T/%v", cause, cause), func(t *testing.T) {
			c, l := admissionClient(t, "offline", cause, nil)
			connectFixture(t, c)
			first, next := wait(t, l.events), wait(t, l.events)
			if first.err != cause || next.err != nil || next.at.Sub(first.at) < 4*time.Millisecond {
				t.Fatal("Accept was not retried with backoff")
			}
			if !c.IsConnected() || c.ctx.Err() != nil {
				t.Fatal("recovered listener did not publish a usable session")
			}
		})
	}
}

func TestLocalListenerRetryPreservesReplacement(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"offline", "both", "both offline"} {
		t.Run(mode, func(t *testing.T) {
			c, l := admissionClient(t, mode, nil)
			oldDevice := connectFixture(t, c)
			wait(t, l.events)
			old, done := c.conn.Load(), c.Done()
			l.steps <- syscall.EMFILE
			wait(t, l.events)
			if !old.alive() || c.conn.Load() != old {
				t.Fatal("transient Accept failure retired the authenticated session")
			}

			l.steps <- nil
			dialer := net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP("127.0.0.2")}, Timeout: time.Second}
			unauthorized, err := dialer.DialContext(t.Context(), "tcp", c.LocalAddress().String())
			if errors.Is(err, syscall.EADDRNOTAVAIL) {
				// Only Linux routes all of 127/8 to loopback by default.
				t.Skip("no second loopback address on this host")
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = unauthorized.Close() })
			reconnectSocketClosed(t, unauthorized)
			wait(t, l.events)

			l.steps <- nil
			invalid := reconnectCandidate(t, c)
			invalid.write(t, fmt.Sprintf(`<stream:stream from="rrcgateway_wrong" to="%s" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`, c.config.Host))
			reconnectSocketClosed(t, invalid.socket)
			wait(t, l.events)
			if !old.alive() || c.conn.Load() != old {
				t.Fatal("recovery bypassed admission restrictions")
			}

			l.steps <- nil
			incomplete := reconnectCandidate(t, c)
			incomplete.write(t, fmt.Sprintf(`<stream:stream from="rrcgateway_%s" to="%s" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`, c.config.SerialNumber, c.config.Host))
			incomplete.readThrough(t, "</stream:features>")
			wait(t, l.events)
			if !old.alive() || c.conn.Load() != old {
				t.Fatal("incomplete replacement retired the authenticated session")
			}
			_ = incomplete.socket.Close()

			l.steps <- nil
			replacement := connectFixture(t, c)
			wait(t, done)
			c.mu.Lock()
			current := c.conn.Load()
			c.mu.Unlock()
			if current == old || !current.alive() || c.UpstreamConnected() != (mode == "both") {
				t.Fatal("replacement did not retain the configured authentication mode")
			}
			reconnectSocketClosed(t, oldDevice.socket)
			written := make(chan error, 1)
			go func() { written <- c.Put(t.Context(), "/after-recovery", 1) }()
			if request := replacement.readThrough(t, "</message>"); !strings.Contains(request, "PUT /after-recovery ") {
				t.Fatal("request did not reach the replacement")
			}
			replacement.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
			if err := wait(t, written); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLocalListenerBackoffAndCloseCancels(t *testing.T) {
	t.Parallel()
	steps := make([]error, 7)
	for i := range steps {
		steps[i] = syscall.EMFILE
	}
	c, l := admissionClient(t, "offline", steps...)
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	previous := wait(t, l.events)
	var delay time.Duration
	for range len(steps) - 1 {
		next := wait(t, l.events)
		delay = min(max(2*delay, 5*time.Millisecond), time.Second)
		if elapsed := next.at.Sub(previous.at); elapsed < delay*4/5 {
			t.Fatalf("retried after %v, before its %v backoff", elapsed, delay)
		}
		previous = next
	}
	// A retry timer is pending now; Close must not wait for it.
	closed := make(chan error, 1)
	start := time.Now()
	go func() { closed <- c.Close() }()
	if err := wait(t, closed); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed >= 2*delay*3/4 {
		t.Fatalf("Close waited %v for the retry timer", elapsed)
	}
	wait(t, l.done)
	if err := wait(t, connected); !errors.Is(err, ErrClosed) {
		t.Fatal("Close did not wake the pending login:", err)
	}
	select {
	case <-l.events:
		t.Fatal("admission kept retrying after Close")
	default:
	}
}

func TestLocalListenerCancelledConnectKeepsRetrying(t *testing.T) {
	t.Parallel()
	c, l := admissionClient(t, "offline", syscall.EMFILE, syscall.EMFILE, syscall.EMFILE, syscall.EMFILE)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(ctx) }()
	for range 4 {
		wait(t, l.events)
	}
	cancel()
	if err := wait(t, connected); !errors.Is(err, context.Canceled) {
		t.Fatal("caller cancellation did not end its wait:", err)
	}
	l.steps <- nil
	connectFixture(t, c)
	if !c.IsConnected() || c.ctx.Err() != nil {
		t.Fatal("caller cancellation stopped client-owned admission")
	}
}

func TestLocalListenerBackoffResetsAfterAccept(t *testing.T) {
	t.Parallel()
	steps := make([]error, 8)
	for i := range steps[:7] {
		steps[i] = syscall.EMFILE
	}
	c, l := admissionClient(t, "offline", steps...)
	connectFixture(t, c)
	for range steps {
		wait(t, l.events)
	}
	l.steps <- syscall.ENFILE
	l.steps <- nil
	first, next := wait(t, l.events), wait(t, l.events)
	if elapsed := next.at.Sub(first.at); elapsed < 4*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatal("a successful Accept did not reset the next failure's backoff:", elapsed)
	}
}

func TestLocalListenerCloseWhileAcceptingIsNotFailure(t *testing.T) {
	t.Parallel()
	c, l := admissionClient(t, "offline", nil)
	wait(t, l.events)
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	admissionDialStarted(t, c)
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	if err := wait(t, closed); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, connected); !errors.Is(err, ErrClosed) || errors.Is(err, ErrListenerFailed) {
		t.Fatal("normal shutdown returned a listener failure:", err)
	}
	wait(t, l.done)
}

func TestLocalListenerPermanentFailureWakesWaiters(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{
		syscall.EBADF, syscall.EINVAL,
		errors.Join(net.ErrClosed, admissionNetError{temporary: true}),
	} {
		t.Run(fmt.Sprintf("%T/%v", cause, cause), func(t *testing.T) {
			c, l := admissionClient(t, "offline")
			connected := make(chan error, 1)
			go func() { connected <- c.Connect(t.Context()) }()
			admissionDialStarted(t, c)
			get, put := make(chan error, 1), make(chan error, 1)
			go func() { _, err := c.Get(t.Context(), "/waiting"); get <- err }()
			go func() { put <- c.Put(t.Context(), "/waiting", 1) }()
			l.steps <- cause
			for _, result := range []<-chan error{connected, get, put} {
				if err := wait(t, result); !errors.Is(err, ErrListenerFailed) || !errors.Is(err, cause) || retryable(err) {
					t.Fatal("terminal cause was not surfaced to the waiter:", err)
				}
			}
			wait(t, l.closed)
			wait(t, l.done)
			if err := c.Connect(t.Context()); !errors.Is(err, ErrListenerFailed) || !errors.Is(err, cause) {
				t.Fatal("later login lost the terminal listener error:", err)
			}
			if c.ctx.Err() != nil {
				t.Fatal("admission took over the owner's Close lifecycle")
			}
		})
	}
}

func TestLocalListenerUnexpectedCloseIsPermanent(t *testing.T) {
	t.Parallel()
	c, l := admissionClient(t, "offline", nil)
	wait(t, l.events)
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	admissionDialStarted(t, c)
	_ = l.Listener.Close()
	if err := wait(t, connected); !errors.Is(err, ErrListenerFailed) || !errors.Is(err, net.ErrClosed) {
		t.Fatal("unexpected listener closure was silently treated as client shutdown:", err)
	}
	wait(t, l.done)
}

func TestLocalListenerPermanentFailureRetiresSession(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"offline", "both", "both offline"} {
		t.Run(mode, func(t *testing.T) {
			c, l := admissionClient(t, mode, nil)
			device := connectFixture(t, c)
			wait(t, l.events)
			done := c.Done()
			active := make(chan error, 1)
			go func() { active <- c.Put(t.Context(), "/active", 1) }()
			device.readThrough(t, "</message>")
			queued := make(chan error, 1)
			go func() { queued <- c.Put(t.Context(), "/queued", 2) }()
			l.steps <- syscall.EBADF
			wait(t, done)
			if err := wait(t, active); !errors.Is(err, errConnectionLost) {
				t.Fatal("listener failure did not release the active request:", err)
			}
			if err := wait(t, queued); !errors.Is(err, ErrListenerFailed) || !errors.Is(err, syscall.EBADF) {
				t.Fatal("queued request did not receive the terminal listener cause:", err)
			}
			if c.IsConnected() || !errors.Is(c.Connect(t.Context()), ErrListenerFailed) {
				t.Fatal("permanent admission failure left a usable session")
			}
			reconnectSocketClosed(t, device.socket)
			wait(t, l.closed)
			wait(t, l.done)
			closed := make(chan error, 1)
			go func() { closed <- c.Close() }()
			if err := wait(t, closed); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func serve(t *testing.T, handle func(n int32, s net.Conn)) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepts := new(atomic.Int32)
	go func() {
		for {
			s, err := ln.Accept()
			if err != nil {
				return
			}
			n := accepts.Add(1)
			go func() { defer s.Close(); handle(n, s) }() //nolint:errcheck
		}
	}()
	return ln.Addr().String(), accepts
}

// deviceLogin runs the device side of a login, reporting rather than
// failing, for connections the server is expected to drop.
func deviceLogin(c *Client) (net.Conn, error) {
	return deviceLoginAt(c, c.LocalAddress().String())
}

func deviceLoginAt(c *Client, address string) (net.Conn, error) {
	s, err := net.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	_ = s.SetDeadline(time.Now().Add(3 * time.Second))
	r := bufio.NewReader(s)
	for _, step := range loginSteps(c) {
		_, err := io.WriteString(s, step.send)
		for text := ""; err == nil && !strings.Contains(text, step.end); {
			var part string
			part, err = r.ReadString('>')
			text += part
		}
		if err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	_ = s.SetDeadline(time.Time{})
	return s, nil
}

func TestMappedDeviceIPAdmitted(t *testing.T) {
	t.Parallel()
	c := newLocalTestClient(t, Config{ConnectTimeout: 2 * time.Second},
		LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("::ffff:127.0.0.1")})
	s, err := deviceLogin(c)
	if err != nil {
		t.Fatal("IPv4 peer refused for its mapped device IP:", err)
	}
	_ = s.Close()
}

func TestDualStackListenerAdmitsIPv4Device(t *testing.T) {
	t.Parallel()
	if ln, err := net.Listen("tcp", "[::]:0"); err != nil {
		t.Skip("no IPv6:", err)
	} else {
		_ = ln.Close()
	}
	// A dual-stack socket reports the IPv4 device as ::ffff:127.0.0.1.
	c := newLocalTestClient(t, Config{ConnectTimeout: 2 * time.Second},
		LocalOptions{ListenAddress: "[::]:0", DeviceIP: netip.MustParseAddr("127.0.0.1")})
	s, err := deviceLoginAt(c, net.JoinHostPort("127.0.0.1", fmt.Sprint(c.LocalAddress().(*net.TCPAddr).Port)))
	if err != nil {
		t.Fatal("IPv4 device refused on a dual-stack listener:", err)
	}
	_ = s.Close()
}

func TestDevicePeer(t *testing.T) {
	for _, tc := range []struct {
		peer, device string
		match        bool
	}{
		{"192.0.2.10", "192.0.2.10", true},
		{"::ffff:192.0.2.10", "192.0.2.10", true},
		{"192.0.2.11", "192.0.2.10", false},
		// A zone binds the device to one link only when configured.
		{"fe80::10%eth0", "fe80::10", true},
		{"fe80::10%eth0", "fe80::10%eth0", true},
		{"fe80::10%eth1", "fe80::10%eth0", false},
	} {
		if got := devicePeer(netip.MustParseAddr(tc.peer), netip.MustParseAddr(tc.device)); got != tc.match {
			t.Errorf("devicePeer(%s, %s) = %t", tc.peer, tc.device, got)
		}
	}
}

func TestOfflineRejectsOtherSourceIPs(t *testing.T) {
	t.Parallel()
	c := newLocalTestClient(t, Config{ConnectTimeout: 100 * time.Millisecond},
		LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("192.0.2.1")})
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	s, err := net.Dial("tcp", c.LocalAddress().String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck
	// An admitted peer would get stream features back.
	_, _ = io.WriteString(s, loginSteps(c)[0].send)
	reconnectSocketClosed(t, s)
	if err := wait(t, connected); err == nil {
		t.Fatal("login from another IP accepted")
	}
}

// TestDeviceOutputMatchesSuccessfulCaptures pins the bytes the firmware
// accepted in captures. A conforming XML parser cannot tell attribute order
// or element spelling apart, so only a byte comparison keeps them.
func TestDeviceOutputMatchesSuccessfulCaptures(t *testing.T) {
	t.Parallel()
	fixture := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("../protocol/testdata/xmpp", name))
		if err != nil {
			t.Fatal(err)
		}
		// The generator declares the stream's default namespace on each stanza.
		return strings.Replace(string(raw), ` xmlns="jabber:client"`, "", 1)
	}
	c := localTestClient(t, time.Second)
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	s, err := net.Dial("tcp", c.LocalAddress().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_ = s.SetDeadline(time.Now().Add(5 * time.Second))
	d := &deviceFixture{socket: s, reader: bufio.NewReader(s)}
	header := fmt.Sprintf(`<stream:stream from="rrcgateway_%s" to="%s" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`, c.config.SerialNumber, c.config.Host)
	after := func(raw, marker string) string { return raw[strings.Index(raw, marker):] }
	d.write(t, header)
	if got := after(d.readThrough(t, "</stream:features>"), "<stream:features"); got != fixture("0168-stream-features.xml") {
		t.Errorf("SASL features differ:\n%s", got)
	}
	d.write(t, `<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="DIGEST-MD5"/>`)
	d.readThrough(t, "</challenge>")
	d.write(t, `<response xmlns="urn:ietf:params:xml:ns:xmpp-sasl">`+base64.StdEncoding.EncodeToString([]byte("proof"))+`</response>`)
	if !strings.Contains(d.readThrough(t, "</success>"), base64.StdEncoding.EncodeToString([]byte("rspauth="+strings.Repeat("0", 32)))) {
		t.Error("missing compatible rspauth")
	}
	d.write(t, header)
	if got := after(d.readThrough(t, "</stream:features>"), "<stream:features"); got != fixture("0169-stream-features.xml") {
		t.Errorf("session features differ:\n%s", got)
	}
	d.write(t, `<iq type="set" id="bind_1"><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"><resource>RRC-RestApi</resource></bind></iq>`)
	if got := d.readThrough(t, "</iq>"); got != fixture("0170-iq.xml") {
		t.Errorf("bind result differs:\n%s", got)
	}
	d.write(t, `<iq type="set" id="sess_1"><session xmlns="urn:ietf:params:xml:ns:xmpp-session"/></iq>`)
	if got := d.readThrough(t, "/>"); got != fixture("0005-iq.xml") {
		t.Errorf("session acknowledgement differs:\n%s", got)
	}
	d.write(t, `<presence><status>RRC</status></presence>`)
	if err := wait(t, connected); err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = c.Get(t.Context(), "/heatingCircuits/hc1/temperatureRoomManual") }()
	if got := d.readThrough(t, "</message>"); got != fixture("0166-message.xml") {
		t.Errorf("local GET differs:\n%q", got)
	}
}

func TestUnansweredLocalRequestRetiresSession(t *testing.T) {
	t.Parallel()
	c := newLocalTestClient(t, Config{ConnectTimeout: time.Second, RetryTimeout: 3 * time.Second},
		LocalOptions{RequestTimeout: 100 * time.Millisecond, ReconnectInterval: time.Hour})
	d := connectFixture(t, c)
	done := c.Done()
	result := make(chan error, 1)
	go func() { _, err := c.Get(t.Context(), "/silent"); result <- err }()
	d.readThrough(t, "</message>")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("unanswered local request kept the device session")
	}
	// The device's answer window closed before the caller's: still a timeout.
	if err := wait(t, result); !errors.Is(err, context.DeadlineExceeded) || retryable(err) {
		t.Fatal(err)
	}
}

func TestDeviceStreamRestartEndsSession(t *testing.T) {
	t.Parallel()
	c := localTestClient(t, time.Second)
	d := connectFixture(t, c)
	done := c.Done()
	d.write(t, `<stream:stream from="rrcgateway_123456789" to="`+DefaultHost+`" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("authenticated stream restart ignored")
	}
}

func TestNewLocalClientRejectsInvalidOptions(t *testing.T) {
	t.Parallel()
	cfg := Config{SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret"}
	for name, o := range map[string]LocalOptions{
		"no device IP":       {ListenAddress: "127.0.0.1:0"},
		"mode":               {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), Mode: "relay"},
		"policy":             {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), UpdatePolicy: "freeze"},
		"upstream offline":   {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), UpstreamAddress: "cloud.example:5222"},
		"upstream no port":   {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), Mode: ModeBoth, UpstreamAddress: "invalid"},
		"upstream bad port":  {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), Mode: ModeBoth, UpstreamAddress: "cloud.example:65536"},
		"services allowed":   {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), UpdateServices: []string{"gservice_extra"}},
		"empty service name": {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), UpdatePolicy: UpdatesBlock, UpdateServices: []string{" /x"}},
		"root service name":  {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), UpdatePolicy: UpdatesBlock, UpdateServices: []string{"./x"}},
		// Each below would be accepted, then fail every login or relay quietly.
		"upstream empty port":   {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), Mode: ModeBoth, UpstreamAddress: "cloud.example:"},
		"upstream port zero":    {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), Mode: ModeBoth, UpstreamAddress: "cloud.example:0"},
		"unspecified device IP": {ListenAddress: "127.0.0.1:0", DeviceIP: netip.IPv4Unspecified()},
		"multicast device IP":   {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("224.0.0.1")},
		"broadcast device IP":   {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("255.255.255.255")},
		"mapped broadcast IP":   {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("::ffff:255.255.255.255")},
		"upstream without host": {ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1"), Mode: ModeBoth, UpstreamAddress: ":5222"},
	} {
		if c, err := NewLocalClient(cfg, o); err == nil {
			_ = c.Close()
			t.Error(name, "accepted")
		}
	}
	cfg.ConnectTimeout = -time.Second
	if c, err := NewLocalClient(cfg, LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: netip.MustParseAddr("127.0.0.1")}); err == nil {
		_ = c.Close()
		t.Error("negative connect timeout accepted")
	}
}

func TestQueuedWriteRetriesOnNextSession(t *testing.T) {
	t.Parallel()
	c := newLocalTestClient(t, Config{ConnectTimeout: 5 * time.Second, RetryTimeout: 3 * time.Second},
		LocalOptions{RequestTimeout: 200 * time.Millisecond})
	d := connectFixture(t, c)
	held, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	go func() { _, _ = c.Get(held, "/held") }()
	d.readThrough(t, "</message>")
	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- c.Put(t.Context(), "/probe", 1) }()
	// The unanswered GET retires the session before the PUT is written.
	reconnectSocketClosed(t, d.socket)
	next := connectFixture(t, c)
	if raw := next.readThrough(t, "</message>"); !strings.Contains(raw, "PUT /probe ") {
		t.Fatal(raw)
	}
	next.reply(t, c, "HTTP/1.0 204 No Content\r\n\r\n")
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	// Waiting for the login paced the retry; a backoff would only add delay.
	if elapsed := time.Since(start); elapsed >= c.config.RetryTimeout {
		t.Fatalf("write waited %v for its backoff", elapsed)
	}
}

func TestLocalReplyWithNamespacedMetadata(t *testing.T) {
	t.Parallel()
	c := localTestClient(t, time.Second)
	device := connectFixture(t, c)
	result := make(chan error, 1)
	go func() { _, err := c.Get(t.Context(), "/x"); result <- err }()
	device.readThrough(t, "</message>")
	cipher, err := c.encryptor.Encrypt(`{"id":"/x","value":1}`)
	if err != nil {
		t.Fatal(err)
	}
	device.write(t, fmt.Sprintf(`<message to="%s/localprobe" xmlns:meta="urn:example:metadata" meta:to="metadata-value" type="chat"><body>HTTP/1.0 200 OK
Content-Type: application/json

%s</body></message>`, c.config.JID(), cipher))
	if err := wait(t, result); err != nil {
		t.Fatalf("valid namespaced metadata prevented local GET reply: %v", err)
	}
}

func TestNegativeIntervalsUseDefaults(t *testing.T) {
	t.Parallel()
	// A negative probe interval would panic the bridge's ticker.
	c := newLocalTestClient(t, Config{ConnectTimeout: time.Second}, LocalOptions{RequestTimeout: -1, ReconnectInterval: -1})
	d := connectFixture(t, c)
	localWrite(t, c, d)
}
