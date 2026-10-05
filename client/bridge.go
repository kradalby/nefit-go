package client

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	legacy "github.com/xmppo/go-xmpp"

	nc "github.com/kradalby/nefit-go/crypto"
	"github.com/kradalby/nefit-go/protocol"
	wire "github.com/kradalby/nefit-go/xmpp"
)

type bridgeState struct {
	ctx               context.Context
	cancel            context.CancelFunc
	upstream          net.Conn
	upstreamConnected atomic.Bool
	probeRunning      atomic.Bool
	recovery          chan struct{}
	upstreamMu        sync.Mutex
	inputs            chan frameEvent
	requests          chan bridgeRequest
	workers           sync.WaitGroup
	closeOnce         sync.Once
	logger            *slog.Logger
	runOnce           sync.Once
	deliveries        chan bridgeDelivery
	initialRequests   []bridgeRequest
}
type (
	bridgeDelivery struct {
		value any
		err   error
	}
	frameEvent struct {
		cloud bool
		frame wire.Frame
		err   error
	}
	sendResult struct {
		n   int
		err error
	}
	bridgeRequest struct {
		message wire.Message
		element *wire.Element
		sent    chan sendResult
		ctx     context.Context
		state   *atomic.Int32
	}
	activeRequest struct {
		request     bridgeRequest
		method, uri string
		deadline    time.Time
	}
)

func (t *localTransport) startReader(conn net.Conn, reader *wire.Reader, cloud bool) {
	t.workers.Go(func() {
		for {
			f, err := reader.Next()
			select {
			case t.inputs <- frameEvent{cloud: cloud, frame: f, err: err}:
			case <-t.ctx.Done():
				return
			}
			if err != nil || f.End {
				return
			}
		}
	})
}

func acceptBoth(ctx context.Context, device net.Conn, cfg Config, options LocalOptions) (*localTransport, error) {
	address := options.UpstreamAddress
	if address == "" {
		address = net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	}
	var dialer net.Dialer
	dialCtx, dialCancel := context.WithTimeout(ctx, 3*time.Second)
	upstream, err := dialer.DialContext(dialCtx, "tcp", address)
	dialCancel()
	if err != nil { // Cloud unavailable at startup: the same listener still serves offline.
		t, localErr := acceptLocal(ctx, device, cfg)
		if localErr != nil {
			return nil, localErr
		}
		t.options = options
		t.initBridge(nil)
		return t, nil
	}
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = device.SetDeadline(deadline)
	_ = upstream.SetDeadline(deadline)
	t := &localTransport{socket: device, reader: wire.NewReader(device), config: cfg, options: options}
	t.initBridge(upstream)
	stop := context.AfterFunc(ctx, func() { _ = t.Close() })
	defer stop()
	fail := func(err error) (*localTransport, error) { _ = t.Close(); return nil, err }
	bound, session := false, false
	headerSeen := false
	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case event := <-t.inputs:
			if event.err != nil {
				return fail(event.err)
			}
			if event.frame.End {
				return fail(io.EOF)
			}
			if t.blockedFrame(event.frame) {
				continue
			}
			if event.cloud {
				if e := event.frame.Element; e != nil && e.Name == (xml.Name{Space: wire.ClientNS, Local: "message"}) {
					m, err := bridgeMessage(e)
					if err != nil {
						return fail(err)
					}
					if method, _ := requestLine(m.Body.Text); method != "" {
						if len(t.initialRequests) >= 64 {
							return fail(errors.New("cloud device request queue full during login"))
						}
						t.initialRequests = append(t.initialRequests, bridgeRequest{message: *m, element: e})
						continue
					}
				}
				if _, err = t.write(event.frame); err != nil {
					return fail(err)
				}
				continue
			}
			if event.frame.Stream != nil {
				if err := validateHeader(event.frame.Stream, cfg); err != nil {
					return fail(err)
				}
				headerSeen = true
			} else if !headerSeen {
				return fail(errors.New("missing device stream header"))
			}
			e := event.frame.Element
			if e != nil {
				if e.Name.Space == wire.TLSNS {
					return fail(errors.New("device STARTTLS relay is unsupported; no TLS downgrade attempted"))
				}
				if e.Name == (xml.Name{Space: wire.ClientNS, Local: "iq"}) {
					if e.Child(wire.BindNS, "bind") != nil {
						bound = true
					}
					if e.Child(wire.SessionNS, "session") != nil {
						session = true
					}
				}
			}
			if err = t.toCloud(event.frame); err != nil {
				return fail(err)
			}
			if e != nil && e.Name == (xml.Name{Space: wire.ClientNS, Local: "presence"}) && bound && session {
				if !stop() && ctx.Err() != nil {
					return fail(ctx.Err())
				}
				_ = device.SetDeadline(time.Time{})
				_ = upstream.SetDeadline(time.Time{})
				t.upstreamConnected.Store(true)
				return t, nil
			}
		}
	}
}

func (t *localTransport) initBridge(upstream net.Conn) {
	if t.ctx == nil {
		t.ctx, t.cancel = context.WithCancel(context.Background())
	}
	t.upstream = upstream
	t.inputs = make(chan frameEvent, 32)
	t.requests = make(chan bridgeRequest, 32)
	t.deliveries = make(chan bridgeDelivery, 32)
	t.recovery = make(chan struct{}, 1)
	t.startReader(t.socket, t.reader, false)
	if upstream != nil {
		t.startReader(upstream, wire.NewReader(upstream), true)
	}
}

func (t *localTransport) toCloud(frame wire.Frame) error {
	if t.upstream == nil {
		return errors.New("upstream unavailable")
	}
	t.upstreamMu.Lock()
	defer t.upstreamMu.Unlock()
	_ = t.upstream.SetWriteDeadline(time.Now().Add(t.options.RequestTimeout))
	defer func() { _ = t.upstream.SetWriteDeadline(time.Time{}) }()
	_, err := writeTyped(t.upstream, frame)
	return err
}

func (t *localTransport) submit(ctx context.Context, m wire.Message) (int, error) {
	r := bridgeRequest{message: m, sent: make(chan sendResult, 1), ctx: ctx, state: new(atomic.Int32)}
	abort := func() (int, error) {
		if r.state.CompareAndSwap(queued, abandoned) || r.state.Load() == abandoned {
			return 0, &unsentError{ctx.Err()}
		}
		_ = t.Close()
		return 0, ctx.Err()
	}
	select {
	case t.requests <- r:
	case <-ctx.Done():
		return abort()
	case <-t.ctx.Done():
		return 0, t.ctx.Err()
	}
	select {
	case result := <-r.sent:
		return result.n, result.err
	case <-ctx.Done():
		return abort()
	case <-t.ctx.Done():
		return 0, t.ctx.Err()
	}
}

func requestLine(text string) (method, uri string) {
	line, _, _ := strings.Cut(text, "\n")
	fields := strings.Fields(line)
	if len(fields) != 3 || !strings.HasPrefix(fields[2], "HTTP/") {
		return "", ""
	}
	return fields[0], fields[1]
}

func (t *localTransport) blockedService(from, to string) bool {
	if t.options.UpdatePolicy != UpdatesBlock {
		return false
	}
	for _, jid := range []string{from, to} {
		bare, _, _ := strings.Cut(jid, "/")
		local, _, _ := strings.Cut(bare, "@")
		if local == "gservice_update" || local == "gservice_firmware" {
			return true
		}
		for _, blocked := range t.options.UpdateServices {
			if bare == blocked || local == blocked {
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
	if t.blockedService(m.From, m.To) {
		return true
	}
	method, uri := requestLine(m.Body.Text)
	uri = resource(uri)
	return method != "" && method != "GET" && method != "HEAD" && (uri == "/gateway/update" || strings.HasPrefix(uri, "/gateway/update/"))
}

func (t *localTransport) blockedFrame(frame wire.Frame) bool {
	e := frame.Element
	if e == nil {
		return false
	}
	if t.blockedService(e.Get("from"), e.Get("to")) {
		return true
	}
	if e.Name != (xml.Name{Space: wire.ClientNS, Local: "message"}) {
		return false
	}
	// Inspect the preserved tree: typed decoding selects the last body, while
	// relaying retains all bodies, including language alternatives.
	for _, node := range e.Children {
		if body := node.Element; body != nil && body.Name == (xml.Name{Space: wire.ClientNS, Local: "body"}) {
			if t.blocked(wire.Message{From: e.Get("from"), To: e.Get("to"), Body: wire.Body{Text: body.Text()}}) {
				return true
			}
		}
	}
	return false
}

// API messages have one body. Ambiguous alternatives cannot be scheduled using
// one outstanding request while preserving their complete XML for forwarding.
func bridgeMessage(e *wire.Element) (*wire.Message, error) {
	bodies, requests := 0, 0
	for _, node := range e.Children {
		if body := node.Element; body != nil && body.Name == (xml.Name{Space: wire.ClientNS, Local: "body"}) {
			bodies++
			if method, _ := requestLine(body.Text()); method != "" {
				requests++
			}
		}
	}
	if requests != 0 && bodies != 1 {
		return nil, errors.New("multiple HTTP-over-XMPP bodies")
	}
	value, err := e.Typed()
	if err != nil {
		return nil, err
	}
	return value.(*wire.Message), nil
}

func messageFrame(m wire.Message) (wire.Frame, error) {
	raw, err := xml.Marshal(m)
	if err != nil {
		return wire.Frame{}, err
	}
	var e wire.Element
	if err = xml.Unmarshal(raw, &e); err != nil {
		return wire.Frame{}, err
	}
	return wire.Frame{Element: &e}, nil
}

func (t *localTransport) rejectCloud(m wire.Message) error {
	response := wire.Message{From: m.To, To: m.From, Type: "chat", Body: wire.Body{Text: "HTTP/1.0 403 Forbidden\r\nContent-Length: 0\r\n\r\n"}}
	frame, err := messageFrame(response)
	if err != nil {
		return err
	}
	return t.toCloud(frame)
}

func (t *localTransport) requestMatches(active *activeRequest, m *wire.Message) bool {
	if active == nil || m.To != active.request.message.From {
		return false
	}
	if m.Type == "error" {
		return true
	}
	response, err := protocol.ParseHTTPResponse(m.Body.Text)
	if err != nil {
		return false
	}
	if active.method == "PUT" {
		return response.Body == "" || response.StatusCode >= 300
	}
	if active.method == "GET" {
		if response.StatusCode >= 300 {
			return true
		}
		if response.Body == "" {
			return false
		}
		// Check echoed paths when decryptable. The destination resource additionally
		// isolates local and cloud acknowledgements on the shared device connection.
		enc, err := newEncryptor(t.config)
		if err == nil {
			if plain, err := enc.DecryptAndStrip(response.Body); err == nil {
				if id := responseID(plain); id != "" {
					return resource(id) == resource(active.uri)
				}
			}
		}
		return true
	}
	return true
}

func (t *localTransport) recvBoth() (any, error) {
	t.runOnce.Do(func() {
		t.workers.Go(func() {
			err := t.runBridge()
			select {
			case t.deliveries <- bridgeDelivery{err: err}:
			case <-t.ctx.Done():
			}
		})
	})
	select {
	case delivery := <-t.deliveries:
		return delivery.value, delivery.err
	case <-t.ctx.Done():
		return nil, t.ctx.Err()
	}
}

func (t *localTransport) runBridge() error {
	queue := t.initialRequests
	t.initialRequests = nil
	var active *activeRequest
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	probe := time.NewTicker(t.options.ReconnectInterval)
	defer probe.Stop()
	sendNext := func() error {
		var r bridgeRequest
		for {
			if active != nil || len(queue) == 0 {
				return nil
			}
			r = queue[0]
			queue = queue[1:]
			if r.state == nil {
				break
			}
			if r.ctx.Err() != nil {
				r.state.CompareAndSwap(queued, abandoned)
			}
			if r.state.Load() == queued {
				break
			}
		}
		method, uri := requestLine(r.message.Body.Text)
		var value any = r.message
		if r.element != nil {
			value = wire.Frame{Element: r.element}
		}
		ctx := t.ctx
		if r.ctx != nil {
			ctx = r.ctx
		}
		n, err := t.writeContext(ctx, value, r.state)
		if r.sent != nil {
			r.sent <- sendResult{n, err}
		}
		if err != nil {
			var unsent *unsentError
			if errors.As(err, &unsent) {
				return nil
			}
			return err
		}
		active = &activeRequest{request: r, method: method, uri: uri, deadline: time.Now().Add(t.options.RequestTimeout)}
		return nil
	}
	disconnectCloud := func() {
		if t.upstreamConnected.Swap(false) {
			_ = t.upstream.Close()
			if t.logger != nil {
				t.logger.Warn("Bosch connection lost; local service remains available")
			}
		}
		// The device still owes an active cloud request its reply. Dropping only
		// unsent cloud requests keeps the single-request constraint intact.
		queue = removeCloudRequests(queue)
	}
	for {
		if err := sendNext(); err != nil {
			return err
		}
		select {
		case <-t.ctx.Done():
			return t.ctx.Err()
		case r := <-t.requests:
			if t.blocked(r.message) {
				r.sent <- sendResult{err: &unsentError{errors.New("firmware update write blocked by policy")}}
				continue
			}
			if len(queue) >= 64 {
				r.sent <- sendResult{err: &unsentError{errors.New("device request queue full")}}
				continue
			}
			queue = append(queue, r)
		case <-timer.C:
			if active != nil && time.Now().After(active.deadline) {
				return fmt.Errorf("device request timed out: %s %s", active.method, active.uri)
			}
		case <-probe.C:
			if !t.upstreamConnected.Load() && !t.probeRunning.Swap(true) {
				t.workers.Go(func() {
					defer t.probeRunning.Store(false)
					address := t.options.UpstreamAddress
					if address == "" {
						address = net.JoinHostPort(t.config.Host, fmt.Sprint(t.config.Port))
					}
					ctx, cancel := context.WithTimeout(t.ctx, 2*time.Second)
					defer cancel()
					var d net.Dialer
					c, err := d.DialContext(ctx, "tcp", address)
					if err == nil {
						_ = c.Close()
						select {
						case t.recovery <- struct{}{}:
						case <-t.ctx.Done():
						}
					}
				})
			}
		case <-t.recovery:
			return errors.New("cloud recovered; reconnecting device for gateway authentication")

		case event := <-t.inputs:
			if event.cloud && !t.upstreamConnected.Load() {
				continue
			}
			if event.err != nil || event.frame.End {
				if !event.cloud {
					if event.err != nil {
						return event.err
					}
					return io.EOF
				}
				disconnectCloud()
				continue
			}
			if event.frame.Stream != nil {
				return errors.New("unexpected authenticated stream restart")
			}
			e := event.frame.Element
			if e == nil {
				continue
			}
			if t.blockedService(e.Get("from"), e.Get("to")) {
				continue
			}
			if t.blockedFrame(event.frame) {
				if event.cloud && e.Name == (xml.Name{Space: wire.ClientNS, Local: "message"}) {
					if err := t.rejectCloud(wire.Message{From: e.Get("from"), To: e.Get("to")}); err != nil {
						disconnectCloud()
					}
				}
				continue
			}
			if event.cloud {
				if e.Name == (xml.Name{Space: wire.StreamNS, Local: "error"}) {
					disconnectCloud()
					continue
				}
				if e.Name == (xml.Name{Space: wire.ClientNS, Local: "message"}) {
					m, err := bridgeMessage(e)
					if err != nil {
						return err
					}
					method, _ := requestLine(m.Body.Text)
					if method != "" {
						if len(queue) >= 64 {
							return errors.New("cloud device request queue full")
						}
						// Keep the preserved XML tree when relaying: parsing the HTTP
						// body for scheduling must not discard unmodeled metadata.
						queue = append(queue, bridgeRequest{message: *m, element: e})
						continue
					}
				}
				if _, err := t.write(event.frame); err != nil {
					return err
				}
				continue
			}
			if e.Name == (xml.Name{Space: wire.ClientNS, Local: "message"}) {
				m, err := bridgeMessage(e)
				if err != nil {
					return err
				}
				if t.requestMatches(active, m) {
					active = nil
				}
				if m.To == t.config.JID()+"/"+localResource {
					select {
					case t.deliveries <- bridgeDelivery{value: legacy.Chat{Remote: t.config.ResourceJID(), Type: m.Type, Text: m.Body.Text}}:
					case <-t.ctx.Done():
						return t.ctx.Err()
					}
					continue
				}
				if t.upstreamConnected.Load() {
					if err := t.toCloud(event.frame); err != nil {
						disconnectCloud()
					}
				} else {
					if err := t.service(*m); err != nil {
						return err
					}
				}
			} else {
				if t.upstreamConnected.Load() {
					if err := t.toCloud(event.frame); err != nil {
						disconnectCloud()
					}
				}
				if !t.upstreamConnected.Load() && e.Name == (xml.Name{Space: wire.ClientNS, Local: "iq"}) && e.Child(wire.PingNS, "ping") != nil {
					if _, err := t.write(wire.IQ{Type: "result", ID: e.Get("id"), From: e.Get("to"), To: e.Get("from")}); err != nil {
						return err
					}
				}
			}
		}
	}
}

func removeCloudRequests(queue []bridgeRequest) []bridgeRequest {
	result := queue[:0]
	for _, r := range queue {
		if r.sent != nil {
			result = append(result, r)
		}
	}
	return result
}

// Decryption helpers use the same credentials and resource semantics as Client.

func newEncryptor(cfg Config) (*nc.Encryptor, error) {
	return nc.NewEncryptor(cfg.SerialNumber, cfg.AccessKey, cfg.Password)
}

func responseID(plain string) string {
	var value struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(plain), &value)
	return value.ID
}
