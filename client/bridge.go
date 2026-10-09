package client

import (
	"cmp"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	nc "github.com/kradalby/nefit-go/crypto"
	wire "github.com/kradalby/nefit-go/internal/xmpp"
	"github.com/kradalby/nefit-go/protocol"
)

// errRecovering ends an offline session on purpose, so the device logs in
// again and is relayed.
var errRecovering = errors.New("bosch answers again; reconnecting device for gateway authentication")

// maxCloudRequests bounds app requests waiting for the single device slot.
// Local ones need no bound: the client sends one at a time.
const maxCloudRequests = 32

// maxStalledSenders bounds app senders refused after an unanswered request.
const maxStalledSenders = 64

// localTransport is a device session: the login, then the bridge.
type localTransport struct {
	socket            net.Conn
	input             *receivedReader
	reader            *wire.Reader
	config            Config
	options           LocalOptions
	w                 socketWriter
	ctx               context.Context
	cancel            context.CancelFunc
	upstream          net.Conn
	upstreamConnected atomic.Bool
	superseded        atomic.Bool // a newer device login is being relayed
	probeRunning      atomic.Bool
	recovery          chan struct{}
	inputs            chan frameEvent
	requests          chan bridgeRequest
	runOnce           sync.Once
	deliveries        chan bridgeDelivery
	health            *upstreamHealth
	encryptor         *nc.Encryptor

	// Scheduling state, owned by the login and then by runBridge.
	queue  []bridgeRequest
	active *activeRequest
	// Replies have no transaction identifier. A cloud sender whose request
	// timed out cannot reuse its slot until that reply arrives or the device
	// reconnects: any later reply could otherwise be the stale one.
	stalled map[string]*activeRequest
	// Recovery reconnects the device, which must not cut off a request the
	// device may already be acting on.
	recovering bool

	// spawn refuses new workers once Close starts, so Wait never races Add.
	workers   sync.WaitGroup
	workerMu  sync.Mutex
	closing   bool
	closeOnce sync.Once
}

type (
	bridgeDelivery struct {
		value inbound
		err   error
	}
	frameEvent struct {
		cloud bool
		frame wire.Frame
		err   error
	}
	bridgeRequest struct {
		message wire.Message
		// element is the preserved cloud stanza, relayed instead of message.
		element *wire.Element
		// sent receives the write outcome of a local request; nil for cloud ones.
		sent  chan error
		owner *pending
		ctx   context.Context
		state *atomic.Int32
	}
	activeRequest struct {
		request        bridgeRequest
		method, uri    string
		deadline       time.Time
		minReplyOffset int64
	}
)

// upstreamError marks a gateway login that failed on the Bosch side, so the
// next device connections are served offline for a growing interval.
type upstreamError struct{ err error }

func (e *upstreamError) Error() string { return "bosch login failed: " + e.err.Error() }
func (e *upstreamError) Unwrap() error { return e.err }

// upstreamHealth backs off relaying after Bosch is unreachable or rejects a
// login. A reachable server does not prove it will accept the gateway; only a
// relayed login can, and each failure costs the device a reconnect.
type upstreamHealth struct {
	mu       sync.Mutex
	failures int
	until    time.Time
}

func (h *upstreamHealth) ready() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !time.Now().Before(h.until)
}

func (h *upstreamHealth) fail(interval time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures++
	h.until = time.Now().Add(interval << min(h.failures-1, 5))
}

func (h *upstreamHealth) ok() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures, h.until = 0, time.Time{}
}

func (t *localTransport) spawn(f func()) bool {
	t.workerMu.Lock()
	defer t.workerMu.Unlock()
	if t.closing {
		return false
	}
	t.workers.Go(f)
	return true
}

func (t *localTransport) log() *slog.Logger {
	if t.options.logf != nil {
		return t.options.logf()
	}
	return slog.Default()
}

// Readers hand over one parsed frame at a time: a deep buffer would only hold
// more hostile trees in memory.
func (t *localTransport) startReader(reader *wire.Reader, cloud bool) {
	t.spawn(func() {
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

func upstreamAddress(cfg Config, options LocalOptions) string {
	if options.UpstreamAddress != "" {
		return options.UpstreamAddress
	}
	return net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
}

func acceptBoth(ctx context.Context, device net.Conn, cfg Config, options LocalOptions) (*localTransport, error) {
	var dialer net.Dialer
	dialCtx, dialCancel := context.WithTimeout(ctx, 3*time.Second)
	upstream, err := dialer.DialContext(dialCtx, "tcp", upstreamAddress(cfg, options))
	dialCancel()
	if err != nil {
		// Bosch unreachable: serve this device connection offline.
		t, localErr := acceptLocal(ctx, device, cfg, options)
		if localErr == nil {
			t.log().Warn("Bosch unreachable; serving the device offline", "error", err)
		}
		return t, localErr
	}
	t := newLocalTransport(device, cfg, options)
	deadline, _ := ctx.Deadline()
	_ = device.SetDeadline(deadline)
	_ = upstream.SetDeadline(deadline)
	t.initBridge(upstream)
	stop := context.AfterFunc(ctx, func() { _ = t.Close() })
	defer stop()
	fail := func(err error) (*localTransport, error) { _ = t.Close(); return nil, err }
	bound, session, headerSeen, cloudRejected := false, false, false, false
	// Bosch refusing the device's bind or session is a rejected login too.
	var bindID, sessionID string
	for {
		select {
		case <-ctx.Done():
			// A login that runs out of time stalled on one side; like the
			// socket deadline below, count it against Bosch so it backs off.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fail(&upstreamError{ctx.Err()})
			}
			return fail(ctx.Err())
		case event := <-t.inputs:
			if event.cloud {
				if event.err != nil || event.frame.End {
					return fail(&upstreamError{cmp.Or(event.err, io.EOF)})
				}
				e := event.frame.Element
				if e == nil {
					// Bosch's stream headers, including the restart after SASL.
					if err := t.relayToDevice(event.frame); err != nil {
						return fail(err)
					}
					continue
				}
				if e.Name == (xml.Name{Space: wire.StreamNS, Local: "error"}) || e.Name == (xml.Name{Space: wire.SASLNS, Local: "failure"}) ||
					e.Name == (xml.Name{Space: wire.ClientNS, Local: "iq"}) && e.Get("type") == "error" && (bound && e.Get("id") == bindID || session && e.Get("id") == sessionID) {
					cloudRejected = true
				}
				relay, err := t.admitCloud(e)
				if err != nil {
					return fail(&upstreamError{err})
				}
				if relay {
					if err := t.relayToDevice(event.frame); err != nil {
						return fail(err)
					}
				}
				continue
			}
			if event.err != nil || event.frame.End {
				err := cmp.Or(event.err, io.EOF)
				// A device quitting after a Bosch rejection, or both sides
				// stalling until the deadline, is a failed relayed login.
				if cloudRejected || errors.Is(err, os.ErrDeadlineExceeded) {
					err = &upstreamError{err}
				}
				return fail(err)
			}
			if t.blockedFrame(event.frame) {
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
					if e.Child(wire.BindNS, "bind") != nil && !bound {
						bound, bindID = true, e.Get("id")
					}
					if e.Child(wire.SessionNS, "session") != nil && !session {
						session, sessionID = true, e.Get("id")
					}
				}
			}
			if err = t.toCloud(event.frame); err != nil {
				return fail(&upstreamError{err})
			}
			if e != nil && e.Name == (xml.Name{Space: wire.ClientNS, Local: "presence"}) && bound && session {
				if cloudRejected {
					return fail(&upstreamError{errors.New("bosch refused the gateway session")})
				}
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

func isMessage(e *wire.Element) bool {
	return e != nil && e.Name == (xml.Name{Space: wire.ClientNS, Local: "message"})
}

// reservedSource reports a cloud stanza using the resource local requests are
// sent from: the device would address its reply to the local client.
func reservedSource(e *wire.Element) bool {
	_, resource, ok := strings.Cut(e.Get("from"), "/")
	return ok && resource == localResource
}

// admitCloud applies the ingress rules for Bosch-side stanzas, during login
// and after, and reports whether e is relayed as is. Requests are scheduled;
// refused ones are answered in the device's place so they end for their
// sender instead of the shared device session. An error means a write to
// Bosch failed.
func (t *localTransport) admitCloud(e *wire.Element) (relay bool, err error) {
	if reservedSource(e) {
		t.log().Warn("dropping cloud stanza claiming the local resource")
		return false, nil
	}
	refuse := func(status string) (bool, error) {
		// RFC 6120 8.3.1: an error is never answered, or a bounce could loop.
		// Nor is a message without a request: Bosch services answer every
		// gateway message, so a refusal could ping-pong with them.
		if e.Get("type") == "error" || !carriesRequest(e) {
			return false, nil
		}
		return false, t.replyCloud(e.Get("from"), e.Get("to"), status)
	}
	if t.blockedFrame(wire.Frame{Element: e}) {
		if isMessage(e) {
			return refuse("403 Forbidden")
		}
		return false, nil
	}
	if strings.EqualFold(e.Name.Local, "message") && !isMessage(e) {
		// The device speaks jabber:client; a lenient parser might still read
		// anything like a message, which would reach it unscheduled.
		return refuse("400 Bad Request")
	}
	if !isMessage(e) {
		// Only messages are scheduled: a request elsewhere is not relayed.
		return !carriesRequest(e), nil
	}
	m, err := bridgeMessage(e)
	if err != nil {
		// Relaying it outside the scheduler would break the one-request rule.
		return refuse("400 Bad Request")
	}
	if method, _ := requestLine(m.Body.Text); method == "" {
		return true, nil
	}
	if m.Type == "error" {
		// A bounce quoting a request is no request: scheduling it would write
		// it to the device and answer it, and relaying would skip the scheduler.
		return false, nil
	}
	if _, err := wire.Marshal(wire.Frame{Element: e}, true); err != nil {
		// Escaping can outgrow the encoder's limit; refused later, it would
		// end unanswered.
		return refuse("400 Bad Request")
	}
	// Relay the preserved tree: scheduling must not discard unmodeled metadata.
	return false, t.schedule(bridgeRequest{message: m, element: e, ctx: t.ctx})
}

func (t *localTransport) initBridge(upstream net.Conn) {
	t.upstream = upstream
	t.inputs = make(chan frameEvent)
	t.requests = make(chan bridgeRequest, 32)
	t.deliveries = make(chan bridgeDelivery, 32)
	t.recovery = make(chan struct{}, 1)
	t.stalled = make(map[string]*activeRequest)
	t.startReader(t.reader, false)
	if upstream != nil {
		t.startReader(wire.NewReader(upstream), true)
	}
}

// toCloud writes a relayed frame or a typed stanza to Bosch.
func (t *localTransport) toCloud(v any) error {
	if t.upstream == nil {
		return errors.New("upstream unavailable")
	}
	_ = t.upstream.SetWriteDeadline(time.Now().Add(t.options.RequestTimeout))
	defer func() { _ = t.upstream.SetWriteDeadline(time.Time{}) }()
	// Relayed stanzas speak for the device, so they keep its lexical form.
	_, err := writeTyped(t.upstream, v)
	return err
}

// relayToDevice forwards a cloud stanza. One that cannot be re-encoded is
// dropped: it reached nobody, and must not end the device session.
func (t *localTransport) relayToDevice(frame wire.Frame) error {
	err := t.write(frame)
	var unsent *unsentError
	if errors.As(err, &unsent) && !errors.Is(err, errSessionEnded) {
		t.log().Warn("dropping cloud stanza that cannot be relayed", "error", err)
		return nil
	}
	return err
}

func (t *localTransport) submit(ctx context.Context, m wire.Message, owner *pending) error {
	r := bridgeRequest{message: m, sent: make(chan error, 1), owner: owner, ctx: ctx, state: new(atomic.Int32)}
	select {
	case t.requests <- r:
	case <-ctx.Done():
		return &unsentError{ctx.Err()}
	case <-t.ctx.Done():
		return &unsentError{errSessionEnded}
	}
	select {
	case err := <-r.sent:
		return err
	case <-ctx.Done():
	case <-t.ctx.Done():
	}
	if r.state.CompareAndSwap(queued, abandoned) {
		if ctx.Err() != nil {
			return &unsentError{ctx.Err()}
		}
		return &unsentError{errSessionEnded}
	}
	// Writing started; the writer bounds it by ctx and the session.
	return <-r.sent
}

func requestLine(text string) (method, uri string) {
	line := text
	if end := strings.IndexAny(line, "\r\n"); end >= 0 {
		line = line[:end]
	}
	fields := strings.Fields(line)
	if len(fields) != 3 || !strings.HasPrefix(fields[2], "HTTP/") {
		return "", ""
	}
	return fields[0], fields[1]
}

// bridgeMessage reads the routing fields and HTTP text of a message. API
// messages have one plain-text body: alternatives with HTTP requests could
// not be scheduled as one outstanding request, and a request anywhere else
// could reach a lenient device parser unseen by the scheduler.
func bridgeMessage(e *wire.Element) (wire.Message, error) {
	m := wire.Message{From: e.Get("from"), To: e.Get("to"), Type: e.Get("type")}
	bodies, requests := 0, 0
	for _, node := range e.Children {
		child := node.Element
		if child == nil {
			if strings.TrimSpace(node.Text) != "" {
				return wire.Message{}, errors.New("message text outside its body")
			}
			continue
		}
		if child.Name != (xml.Name{Space: wire.ClientNS, Local: "body"}) {
			if hasBody(child) {
				return wire.Message{}, errors.New("message body in an unexpected place")
			}
			if carriesRequest(child) {
				return wire.Message{}, errors.New("HTTP request outside the message body")
			}
			continue
		}
		for _, n := range child.Children {
			if n.Element != nil {
				return wire.Message{}, errors.New("message body contains elements")
			}
		}
		bodies++
		m.Body.Text = child.Text()
		if hasRequestLine(m.Body.Text) {
			requests++
			if method, _ := requestLine(m.Body.Text); method == "" {
				return wire.Message{}, errors.New("malformed HTTP request line")
			}
		}
	}
	if requests != 0 && bodies != 1 {
		return wire.Message{}, errors.New("multiple HTTP-over-XMPP bodies")
	}
	return m, nil
}

// hasBody reports any element named body, whatever its namespace or case.
func hasBody(e *wire.Element) bool {
	if strings.EqualFold(e.Name.Local, "body") {
		return true
	}
	for _, n := range e.Children {
		if n.Element != nil && hasBody(n.Element) {
			return true
		}
	}
	return false
}

// replyCloud answers a cloud requester in the device's place.
func (t *localTransport) replyCloud(requester, gateway, status string) error {
	return t.toCloud(wire.Message{From: gateway, To: requester, Type: "chat", Body: wire.Body{Text: "HTTP/1.0 " + status + "\r\nContent-Length: 0\r\n\r\n"}})
}

// requestMatches applies the client's reply rules, so the bridge and the
// client agree on which reply ends a request.
func (t *localTransport) requestMatches(active *activeRequest, m wire.Message) bool {
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
	p := &pending{uri: active.uri, get: active.method == "GET"}
	return p.answeredBy(decodeReply(t.encryptor, response))
}

func (t *localTransport) Recv() (inbound, error) {
	t.runOnce.Do(func() {
		t.spawn(func() {
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
		return inbound{}, t.ctx.Err()
	}
}

// probeUpstream checks that an XMPP server answers a stream open with SASL
// features. It cannot prove Bosch will accept the gateway: failed relayed
// logins back off through upstreamHealth instead.
func probeUpstream(ctx context.Context, address, domain string) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := wire.WriteFrame(conn, wire.Frame{Stream: &wire.Stream{To: domain, Version: "1.0"}}); err != nil {
		return err
	}
	reader := wire.NewReader(conn)
	header, err := reader.Next()
	if err != nil {
		return err
	}
	if header.Stream == nil {
		return errors.New("missing stream header")
	}
	features, err := reader.Next()
	if err != nil {
		return err
	}
	if features.Element == nil || features.Element.Child(wire.SASLNS, "mechanisms") == nil {
		return errors.New("no SASL mechanisms offered")
	}
	return nil
}

func (t *localTransport) startProbe() {
	// A recovering bridge already found Bosch and waits only for its request.
	if t.options.Mode != ModeBoth || t.recovering || t.upstreamConnected.Load() || !t.health.ready() || t.probeRunning.Swap(true) {
		return
	}
	if !t.spawn(func() {
		defer t.probeRunning.Store(false)
		ctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
		defer cancel()
		if err := probeUpstream(ctx, upstreamAddress(t.config, t.options), t.config.Host); err != nil {
			t.log().Debug("Bosch still unavailable", "error", err)
			return
		}
		select {
		case t.recovery <- struct{}{}:
		case <-t.ctx.Done():
		}
	}) {
		t.probeRunning.Store(false)
	}
}

// runBridge owns the device connection after login: it schedules one request
// at a time and decides, per device message, which request it answers.
func (t *localTransport) runBridge() error {
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	probe := time.NewTicker(t.options.ReconnectInterval)
	defer probe.Stop()
	for {
		if err := t.sendNext(); err != nil {
			return err
		}
		if t.recovering && t.active == nil {
			return errRecovering
		}
		var err error
		select {
		case <-t.ctx.Done():
			return t.ctx.Err()
		case r := <-t.requests:
			// Callers that gave up leave their entry; only one is live at a time.
			t.queue = slices.DeleteFunc(t.queue, func(q bridgeRequest) bool { return q.state != nil && q.state.Load() == abandoned })
			t.queue = append(t.queue, r)
		case <-timer.C:
			err = t.expire()
		case <-probe.C:
			if t.upstreamConnected.Load() {
				t.health.ok()
			}
			t.startProbe()
		case <-t.recovery:
			t.recovering = true
		case event := <-t.inputs:
			if event.cloud {
				err = t.fromCloud(event)
			} else {
				err = t.fromDevice(event)
			}
		}
		if err != nil {
			return err
		}
	}
}

// schedule queues a cloud request, or answers it in the device's place.
func (t *localTransport) schedule(r bridgeRequest) error {
	switch {
	case t.stalled[r.message.From] != nil:
		return t.replyCloud(r.message.From, r.message.To, "504 Gateway Timeout")
	case cloudQueued(t.queue) >= maxCloudRequests:
		return t.replyCloud(r.message.From, r.message.To, "503 Service Unavailable")
	}
	t.queue = append(t.queue, r)
	return nil
}

func cloudQueued(queue []bridgeRequest) int {
	n := 0
	for _, r := range queue {
		if r.sent == nil {
			n++
		}
	}
	return n
}

// refuse answers a cloud request in the device's place after login, when a
// failed reply costs only the relay.
func (t *localTransport) refuse(m wire.Message, status string) {
	if err := t.replyCloud(m.From, m.To, status); err != nil {
		t.disconnectCloud()
	}
}

func (t *localTransport) disconnectCloud() {
	if t.upstreamConnected.Swap(false) {
		// A relay Bosch drops soon after login backs off like a rejected one;
		// the probe resets it once a relay outlives an interval.
		// abort cancels ctx before closing the relay: a session nefit ends
		// itself is no Bosch failure either.
		if !t.superseded.Load() && t.ctx.Err() == nil {
			t.health.fail(t.options.ReconnectInterval)
		}
		_ = t.upstream.Close()
		t.log().Warn("Bosch connection lost; local service remains available")
	}
	// The device still owes an active cloud request its reply. Dropping only
	// unsent cloud requests keeps the single-request constraint intact.
	t.queue = slices.DeleteFunc(t.queue, func(r bridgeRequest) bool { return r.sent == nil })
}

// sendNext writes queued requests until one is outstanding.
func (t *localTransport) sendNext() error {
	for !t.recovering && t.active == nil && len(t.queue) > 0 {
		// Local requests go first: each app request the device ignores
		// holds the slot for RequestTimeout, which would starve them.
		i := max(slices.IndexFunc(t.queue, func(r bridgeRequest) bool { return r.sent != nil }), 0)
		r := t.queue[i]
		t.queue = slices.Delete(t.queue, i, i+1)
		if r.sent == nil && t.stalled[r.message.From] != nil {
			t.refuse(r.message, "504 Gateway Timeout")
			continue
		}
		// An expired caller is refused by the writer, which reports it on
		// r.sent; only submit itself abandons a request.
		if r.state != nil && r.state.Load() != queued {
			continue
		}
		method, uri := requestLine(r.message.Body.Text)
		var value any = r.message
		if r.element != nil {
			value = wire.Frame{Element: r.element}
		}
		var minReplyOffset int64
		err := t.writeContext(r.ctx, value, r.state, func() {
			minReplyOffset = t.input.bytes.Load()
			r.owner.markSent()
		})
		if r.sent != nil {
			r.sent <- err
		}
		if err != nil {
			var unsent *unsentError
			if errors.As(err, &unsent) {
				continue
			}
			return err
		}
		r.element = nil // only needed to relay
		t.active = &activeRequest{request: r, method: method, uri: uri, deadline: time.Now().Add(t.options.RequestTimeout), minReplyOffset: minReplyOffset}
	}
	return nil
}

// expire ends the outstanding request once the device has had its time.
func (t *localTransport) expire() error {
	a := t.active
	if a == nil || time.Now().Before(a.deadline) {
		return nil
	}
	if a.request.sent != nil {
		// A late reply could answer the next local request.
		return fmt.Errorf("device request timed out: %s %s: %w: %w", a.method, a.uri, errUnanswered, context.DeadlineExceeded)
	}
	t.log().Warn("cloud request unanswered", "method", a.method, "uri", a.uri)
	t.stalled[a.request.message.From] = a
	t.refuse(a.request.message, "504 Gateway Timeout")
	t.active = nil
	if len(t.stalled) >= maxStalledSenders {
		t.disconnectCloud()
	}
	return nil
}

// fromCloud handles a Bosch-side frame; only a failed relay to the device ends
// the session.
func (t *localTransport) fromCloud(event frameEvent) error {
	if !t.upstreamConnected.Load() {
		return nil
	}
	e := event.frame.Element
	if event.err != nil || e == nil || e.Name == (xml.Name{Space: wire.StreamNS, Local: "error"}) {
		t.disconnectCloud()
		return nil
	}
	relay, err := t.admitCloud(e)
	if err != nil {
		t.disconnectCloud()
		return nil
	}
	if relay {
		return t.relayToDevice(event.frame)
	}
	return nil
}

// fromDevice routes a device frame to the request it answers, to Bosch, or
// to nobody.
func (t *localTransport) fromDevice(event frameEvent) error {
	if event.frame.Stream != nil {
		return errors.New("unexpected authenticated stream restart")
	}
	if event.err != nil || event.frame.End {
		return cmp.Or(event.err, io.EOF)
	}
	if t.blockedFrame(event.frame) {
		return nil
	}
	e := event.frame.Element
	localJID := t.config.JID() + "/" + localResource
	if !isMessage(e) {
		if e.Get("to") == localJID {
			// Replies to the local keepalive belong to no one upstream.
			return nil
		}
		if t.upstreamConnected.Load() {
			if err := t.toCloud(event.frame); err == nil {
				return nil
			}
			t.disconnectCloud()
		}
		if reply, ok := iqReply(e, e.Get("to")); ok {
			return t.write(reply)
		}
		return nil
	}
	m, err := bridgeMessage(e)
	if err != nil {
		t.log().Warn("dropping ambiguous device message", "error", err)
		return nil
	}
	if stale := t.stalled[m.To]; stale != nil {
		if event.frame.Offset >= stale.minReplyOffset && t.requestMatches(stale, m) {
			delete(t.stalled, m.To)
		}
		// The sender already received a timeout. Forwarding this reply
		// could acknowledge a later request it was refused.
		return nil
	}
	var owner *activeRequest
	if t.active != nil && event.frame.Offset >= t.active.minReplyOffset && t.requestMatches(t.active, m) {
		owner, t.active = t.active, nil
	}
	switch {
	case m.To == localJID:
		in := inbound{text: m.Body.Text, error: m.Type == "error", push: owner == nil}
		if owner != nil {
			in.owner = owner.request.owner
		}
		select {
		case t.deliveries <- bridgeDelivery{value: in}:
		case <-t.ctx.Done():
			return t.ctx.Err()
		}
	case t.upstreamConnected.Load():
		if err := t.toCloud(event.frame); err != nil {
			t.disconnectCloud()
		}
	case strings.HasPrefix(m.To, RRCContactPrefix):
		// A late reply to a cloud sender that has gone.
	default:
		t.log().Debug("dropping device service message", "to", m.To)
	}
	return nil
}
