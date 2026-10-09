package client

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kradalby/nefit-go/crypto"
	"github.com/kradalby/nefit-go/protocol"
)

// maxPushHandlers bounds push handler calls running at once.
const maxPushHandlers = 64

// EventHandler is called when unsolicited messages are received from the backend
type EventHandler func(uri string, data any)

// PushNotification is an unsolicited update from the backend.
type PushNotification struct {
	URI  string
	Data any
}

// Client represents an active connection to the Nefit Easy backend.
// It handles XMPP communication, encryption, request queueing, and push
// notifications. Its methods are safe for concurrent use.
type Client struct {
	config    Config
	encryptor *crypto.Encryptor
	queue     *RequestQueue

	dial          func(context.Context) (transport, error)
	localListener net.Listener
	localMode     ServerMode
	localReady    chan struct{} // guarded by mu; closed on a new session or listener failure
	localErr      error         // guarded by mu; terminal listener failure
	conn          atomic.Pointer[conn]
	// mu serialises starting and publishing a dial against Close.
	mu      sync.Mutex
	dialing *dialCall

	eventHandlers   []EventHandler
	eventHandlersMu sync.RWMutex
	pushSlots       chan struct{}

	// logger is read by background workers started before SetLogger.
	logger atomic.Pointer[slog.Logger]

	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
}

// NewClient creates a new Nefit Easy client with the given configuration.
// Requests connect on demand; Connect does so ahead of them.
func NewClient(config Config) (*Client, error) {
	config = config.WithDefaults()
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	encryptor, err := crypto.NewEncryptor(config.SerialNumber, config.AccessKey, config.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to create encryptor: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	client := &Client{
		config:    config,
		encryptor: encryptor,
		queue:     NewRequestQueue(),
		pushSlots: make(chan struct{}, maxPushHandlers),
		ctx:       ctx,
		cancel:    cancel,
	}
	client.logger.Store(slog.Default())
	client.dial = client.dialBackend

	return client, nil
}

// SetLogger configures a custom logger for the client. By default, and after
// SetLogger(nil), the client uses slog.Default().
func (c *Client) SetLogger(logger *slog.Logger) {
	// Workers log without checking; nil would crash one of them.
	c.logger.Store(cmp.Or(logger, slog.Default()))
}

func (c *Client) log() *slog.Logger { return c.logger.Load() }

// Connect opens a session unless one is live, so pushes flow before the
// first request. Concurrent callers, requests included, share one login. A
// nil error means a session was established; it may have ended since, so
// watch Done. If ctx ends first, the login carries on for whoever needs a
// session next, bounded by [Config.ConnectTimeout].
func (c *Client) Connect(ctx context.Context) error {
	_, err := c.session(ctx)
	return err
}

// dialCall is a login in flight.
type dialCall struct {
	done chan struct{}
	cn   *conn
	err  error
}

// session returns the live session, or waits for one to be dialled. The dial
// runs under the client's lifetime, not ctx: a caller giving up must not
// abort a login others are waiting on.
func (c *Client) session(ctx context.Context) (*conn, error) {
	c.mu.Lock()
	if c.ctx.Err() != nil {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	if err := c.localErr; err != nil {
		c.mu.Unlock()
		return nil, err
	}
	if cn := c.conn.Load(); cn != nil && cn.alive() {
		c.mu.Unlock()
		return cn, nil
	}
	d := c.dialing
	if d == nil {
		d = &dialCall{done: make(chan struct{})}
		c.dialing = d
		c.wg.Go(func() { c.runDial(d) })
	}
	c.mu.Unlock()

	select {
	case <-d.done:
		return d.cn, d.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Client) runDial(d *dialCall) {
	if c.localListener != nil {
		c.log().Debug("waiting for the device to log in", "mode", c.localMode, "listen", c.localListener.Addr())
	} else {
		c.log().Info("connecting to Nefit Easy backend", "host", c.config.Host, "jid", c.config.JID())
	}

	ctx, cancel := context.WithTimeout(c.ctx, c.config.ConnectTimeout)
	var t transport
	var cn *conn
	var err error
	if c.localListener != nil {
		cn, err = c.waitLocal(ctx)
	} else {
		t, err = c.dial(ctx)
	}
	cancel()

	c.mu.Lock()
	switch {
	case c.ctx.Err() != nil:
		if t != nil {
			_ = t.Close()
		}
		err = ErrClosed
	case c.localErr != nil:
		err = c.localErr
	case err == nil:
		if cn == nil {
			cn = c.publishSession(t)
		}
		d.cn = cn
	}
	d.err = err
	c.dialing = nil
	c.mu.Unlock()
	close(d.done)

	switch {
	case err == nil:
		// acceptDevices logs device logins, replacements included.
		if c.localListener == nil {
			c.log().Info("connected to Nefit Easy backend")
		}
	case errors.Is(err, ErrClosed), errors.Is(err, ErrListenerFailed):
		// The listener logs its own failure.
	case c.localListener != nil && errors.Is(err, context.DeadlineExceeded):
		// The device logs in on its own schedule: a quiet window is no failure.
		c.log().Debug("no device login yet", "waited", c.config.ConnectTimeout)
	default:
		c.log().Error("failed to connect to Nefit Easy backend", "error", err)
	}
}

// publishSession requires mu, fencing worker registration against Close.
func (c *Client) publishSession(t transport) *conn {
	cn := newConn(c.ctx, t)
	c.conn.Store(cn)
	c.wg.Go(func() { c.pingWorker(cn) })
	c.wg.Go(func() { c.receiveWorker(cn) })
	return cn
}

func (c *Client) dialBackend(ctx context.Context) (transport, error) {
	return dialCloud(ctx, c.config, &tls.Config{ServerName: c.config.Host, MinVersion: tls.VersionTLS12})
}

// Close disconnects from the XMPP server, stops background workers and waits
// for running push handlers, so a handler must not call it. It is safe to
// call more than once.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.log().Info("closing Nefit Easy client")

		c.mu.Lock()
		c.cancel()
		cn := c.conn.Swap(nil)
		c.mu.Unlock()
		if c.localListener != nil {
			_ = c.localListener.Close()
		}

		if cn != nil {
			cn.close()
		}

		// Before waiting: handlers may be blocked submitting requests.
		c.queue.Close()
		c.wg.Wait()

		c.log().Info("closed Nefit Easy client")
	})

	return nil
}

// IsConnected reports whether the current connection is alive.
func (c *Client) IsConnected() bool {
	cn := c.conn.Load()
	return cn != nil && cn.alive()
}

// Done returns a channel that is closed when the current connection ends:
// the stream fails, the client is closed, or a sent request goes unanswered.
// The cloud transport retires a session as soon as such a request is
// abandoned, since its late reply would pass for the next one's answer; the
// device server waits up to RequestTimeout for it. Without a connection it is
// already closed.
func (c *Client) Done() <-chan struct{} {
	if cn := c.conn.Load(); cn != nil {
		return cn.ctx.Done()
	}
	return closedChan
}

func (c *Client) pingWorker(cn *conn) {
	ticker := time.NewTicker(c.config.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-cn.ctx.Done():
			return
		case <-ticker.C:
			if err := cn.xmpp.Ping(); err != nil {
				c.log().Error("failed to send ping", "error", err)
				continue
			}
			c.log().Debug("sent keepalive ping")
		}
	}
}

// receiveWorker owns the read side of cn. A read error ends the session: the
// stream decoder's error is sticky, so retrying could never succeed.
func (c *Client) receiveWorker(cn *conn) {
	defer cn.close()

	for {
		in, err := cn.xmpp.Recv()
		if cn.ctx.Err() != nil {
			// Retired: whatever arrives now has no owner.
			return
		}
		if err != nil {
			if errors.Is(err, errRecovering) {
				c.log().Info("connection ended", "reason", err)
			} else {
				c.log().Error("connection lost", "error", err)
			}
			cn.fail(err)
			return
		}
		c.handle(cn, in)
	}
}

func (c *Client) dispatchPushNotification(notification PushNotification) {
	c.eventHandlersMu.RLock()
	handlers := make([]EventHandler, len(c.eventHandlers))
	copy(handlers, c.eventHandlers)
	c.eventHandlersMu.RUnlock()

	// Concurrent so a slow handler cannot stall the stream; tracked so Close
	// does not return while one is still running. Bounded so a device that
	// floods pushes cannot exhaust memory: pushes past the bound are dropped.
	for _, handler := range handlers {
		select {
		case c.pushSlots <- struct{}{}:
		default:
			c.log().Warn("push handlers busy; dropping push", "uri", notification.URI)
			continue
		}
		c.wg.Go(func() {
			defer func() { <-c.pushSlots }()
			handler(notification.URI, notification.Data)
		})
	}
}

func (c *Client) handle(cn *conn, in inbound) {
	if in.error {
		c.log().Error("received error message", "text", in.text)
		if !in.push {
			c.route(cn, reply{err: fmt.Errorf("XMPP error: %s", in.text)}, in.owner)
		}
		return
	}
	if in.text == "" {
		return
	}
	resp, err := protocol.ParseHTTPResponse(in.text)
	if err != nil {
		c.log().Debug("dropping unparseable reply", "error", err)
		return
	}
	c.log().Debug("parsed HTTP response", "status", resp.StatusCode)
	r := decodeReply(c.encryptor, resp)
	if in.push {
		c.push(r)
		return
	}
	c.route(cn, r, in.owner)
}

// Subscribe registers an event handler that will be called when the backend
// sends unsolicited push notifications. Multiple handlers can be registered.
// Handlers run concurrently; Close waits for them to return. While 64 handler
// calls are running, further pushes are dropped, so a handler keeping state
// from pushes should also poll.
func (c *Client) Subscribe(handler EventHandler) {
	c.eventHandlersMu.Lock()
	defer c.eventHandlersMu.Unlock()
	c.eventHandlers = append(c.eventHandlers, handler)
}

// decodeReply decrypts a successful reply's body and picks out the resource
// path the backend echoes as its JSON id. The device bridge uses it too, so
// both agree on which reply answers a request.
func decodeReply(encryptor *crypto.Encryptor, resp *protocol.HTTPResponse) reply {
	r := reply{resp: resp}
	if resp.StatusCode != 200 || resp.Body == "" {
		return r
	}

	// Strip: AES-ECB pads with NUL bytes, which break the JSON parse.
	decrypted, err := encryptor.DecryptAndStrip(resp.Body)
	if err != nil {
		r.err = fmt.Errorf("decryption failed: %w", err)
		return r
	}

	r.data = decrypted
	if strings.Contains(resp.ContentType, "json") {
		var v any
		if err := json.Unmarshal([]byte(decrypted), &v); err == nil {
			r.data = v
			if m, ok := v.(map[string]any); ok {
				r.id, _ = m["id"].(string)
			}
		}
	}

	return r
}

// route hands r to the in-flight request if it can be its answer. Anything
// else is a push notification.
func (c *Client) route(cn *conn, r reply, owner *pending) {
	if owner != nil {
		if !owner.sent.Load() || !owner.answeredBy(r) {
			c.push(r)
			return
		}
		owner.replied.Store(true)
	}
	if p := cn.match(r, owner); p != nil {
		p.reply <- r
		return
	}
	if owner != nil {
		// Its request stopped waiting; the reply answers nothing else.
		c.log().Debug("dropping reply for an abandoned request")
		return
	}
	c.push(r)
}

func (c *Client) push(r reply) {
	if r.err != nil {
		c.log().Warn("dropping unmatched reply", "error", r.err)
		return
	}

	if r.resp.StatusCode != 200 || r.data == nil {
		c.log().Debug("dropping unmatched reply", "status", r.resp.StatusCode)
		return
	}

	c.log().Debug("push notification received", "uri", r.id, "data", r.data)
	c.dispatchPushNotification(PushNotification{URI: r.id, Data: r.data})
}

// roundTrip sends body on the current session and waits for its reply. The
// session is read once: a reply can only arrive on the stream the request
// went out on.
func (c *Client) roundTrip(ctx context.Context, uri, body string, get bool) (reply, error) {
	cn, err := c.session(ctx)
	if err != nil {
		return reply{}, err
	}
	// The queue runs one request at a time, so the slot is free. Claim it
	// before sending: the reply may beat Send's return. It matches only once
	// the request is written, so a queued request cannot take a stray reply.
	p := &pending{uri: uri, get: get, reply: make(chan reply, 1)}
	cn.begin(p)
	defer cn.abandon(p)

	if err := cn.xmpp.Send(ctx, body, p); err != nil {
		var unsent *unsentError
		if errors.As(err, &unsent) {
			if errors.Is(err, errSessionEnded) {
				// The transport is already gone; retire the session now, or
				// the retry would find it alive and fail the same way.
				cn.close()
			}
			return reply{}, fmt.Errorf("request not sent: %w", err)
		}
		cn.close()
		if ctx.Err() != nil {
			return reply{}, fmt.Errorf("%w: %w", errUnanswered, ctx.Err())
		}
		return reply{}, fmt.Errorf("failed to send message: %w: %w", errUnanswered, err)
	}
	select {
	case r := <-p.reply:
		return r, r.err
	case <-ctx.Done():
	case <-cn.ctx.Done():
	}
	if !cn.abandon(p) {
		// Matched before the deadline or session end: route sends it now.
		r := <-p.reply
		return r, r.err
	}
	if ctx.Err() != nil {
		if _, bridged := cn.xmpp.(*localTransport); !bridged {
			// Replies carry no request id: a late one would pass for the next
			// request's answer, so an abandoned request takes its session
			// along before the next can start. The device bridge instead
			// decides which request a reply answers.
			cn.close()
		}
		return reply{}, fmt.Errorf("%w: %w", errUnanswered, ctx.Err())
	}
	if c.ctx.Err() != nil {
		return reply{}, ErrClosed
	}
	if cause := context.Cause(cn.ctx); errors.Is(cause, errUnanswered) {
		return reply{}, cause
	}
	return reply{}, errConnectionLost
}

// retryable permits a deadline or session end before the request was sent.
// One that went out is not retried: its late reply could answer the retry,
// and a write must not be replayed.
func retryable(err error) bool {
	if errors.Is(err, errUnanswered) {
		return false
	}
	var unsent *unsentError
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &unsent) && errors.Is(err, errSessionEnded)
}

// Get performs a GET request to the specified URI and returns the decrypted response data.
// It retries deadlines and session endings before sending, and deserializes JSON responses.
func (c *Client) Get(ctx context.Context, uri string) (any, error) {
	if err := protocol.ValidateURI(uri); err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, c.config.RetryTimeout)
		result, err := c.queue.Submit(reqCtx, func() (any, error) {
			return c.executeGet(reqCtx, uri)
		})
		cancel()
		if err == nil {
			return result, nil
		}
		if attempt > c.config.MaxRetries || ctx.Err() != nil || !retryable(err) {
			return nil, fmt.Errorf("GET request failed after %d attempts: %w", attempt, err)
		}
		c.log().Debug("retrying GET request", "uri", uri, "attempt", attempt, "last_error", err)
	}
}

func (c *Client) executeGet(ctx context.Context, uri string) (any, error) {
	c.log().Debug("sending GET request", "uri", uri)

	r, err := c.roundTrip(ctx, uri, protocol.GetRequest(uri), true)
	if err != nil {
		return nil, err
	}

	if r.resp.StatusCode != 200 {
		return nil, &HTTPError{StatusCode: r.resp.StatusCode, Status: r.resp.Status}
	}

	return r.data, nil
}

// Put performs a PUT request to the specified URI with the given data.
// A string is sent as given; anything else is marshalled to JSON. Either is
// encrypted before sending.
// Deadlines before sending are retried with exponential backoff, and session
// endings before sending at once, since the next login paces them; a write
// that may have arrived is never replayed.
func (c *Client) Put(ctx context.Context, uri string, data any) error {
	if err := protocol.ValidateURI(uri); err != nil {
		return err
	}
	var jsonData string
	switch v := data.(type) {
	case string:
		jsonData = v
	default:
		jsonBytes, err := json.Marshal(data)
		if err != nil {
			return fmt.Errorf("failed to marshal data: %w", err)
		}
		jsonData = string(jsonBytes)
	}

	c.log().Debug("PUT request data prepared",
		"uri", uri,
		"json_data", jsonData,
		"json_length", len(jsonData))

	encrypted, err := c.encryptor.Encrypt(jsonData)
	if err != nil {
		return fmt.Errorf("failed to encrypt data: %w", err)
	}

	c.log().Debug("PUT request encrypted",
		"uri", uri,
		"encrypted_length", len(encrypted))

	backoff := c.config.RetryTimeout
	for attempt := 1; ; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, c.config.RetryTimeout)
		_, err := c.queue.Submit(reqCtx, func() (any, error) {
			return nil, c.executePut(reqCtx, uri, encrypted, jsonData)
		})
		cancel()
		if err == nil {
			if attempt > 1 {
				c.log().Info("PUT request succeeded after retry", "uri", uri, "attempts", attempt)
			}
			return nil
		}
		if ctx.Err() == nil && !retryable(err) {
			// Sent, answered or invalid requests are not retried.
			c.log().Warn("PUT request failed with non-retryable error", "uri", uri, "error", err, "json_data", jsonData)
		}
		if attempt > c.config.MaxRetries || ctx.Err() != nil || !retryable(err) {
			return fmt.Errorf("PUT request failed after %d attempts: %w", attempt, err)
		}
		// A session that ended before the write needs no backoff: waiting
		// for the next login already paces the retry.
		if errors.Is(err, errSessionEnded) {
			continue
		}
		c.log().Debug("retrying PUT request", "uri", uri, "attempt", attempt, "backoff", backoff, "last_error", err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			// Close waits for push handlers, which may be in this backoff.
			return ErrClosed
		}
		backoff = min(2*backoff, 30*time.Second)
	}
}

func (c *Client) executePut(ctx context.Context, uri, encryptedData, jsonData string) error {
	c.log().Debug("sending PUT request",
		"uri", uri,
		"from", c.config.JID(),
		"to", c.config.ResourceJID(),
		"encrypted_payload_length", len(encryptedData),
		"decrypted_json", jsonData)

	r, err := c.roundTrip(ctx, uri, protocol.PutRequest(uri, encryptedData), false)
	if err != nil {
		return err
	}

	if r.resp.StatusCode >= 300 {
		c.log().Error("PUT request failed",
			"uri", uri,
			"status_code", r.resp.StatusCode,
			"status", r.resp.Status,
			"json_data", jsonData)
		return &HTTPError{StatusCode: r.resp.StatusCode, Status: r.resp.Status}
	}

	c.log().Debug("PUT request successful",
		"uri", uri,
		"status_code", r.resp.StatusCode)

	return nil
}
