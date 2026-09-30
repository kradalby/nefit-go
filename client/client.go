package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	xmpp "github.com/xmppo/go-xmpp"

	"github.com/kradalby/nefit-go/crypto"
	"github.com/kradalby/nefit-go/protocol"
)

// EventHandler is called when unsolicited messages are received from the backend
type EventHandler func(uri string, data any)

// PushNotification is an unsolicited update from the backend.
type PushNotification struct {
	URI  string
	Data any
}

// Client represents an active connection to the Nefit Easy backend.
// It handles XMPP communication, encryption, request queueing, and push notifications.
type Client struct {
	config    Config
	encryptor *crypto.Encryptor
	queue     *RequestQueue

	dial func(context.Context) (transport, error)
	conn atomic.Pointer[conn]
	// mu serialises starting and publishing a dial against Close.
	mu      sync.Mutex
	dialing *dialCall

	eventHandlers   []EventHandler
	eventHandlersMu sync.RWMutex

	logger *slog.Logger

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
		logger:    slog.Default(),
		ctx:       ctx,
		cancel:    cancel,
	}
	client.dial = client.dialBackend

	return client, nil
}

// SetLogger configures a custom logger for the client.
// By default, the client uses slog.Default().
func (c *Client) SetLogger(logger *slog.Logger) {
	c.logger = logger
}

// Connect opens a session unless one is live, so pushes flow before the
// first request. Concurrent callers, requests included, share one login. A
// nil error means a session was established; it may have ended since, so
// watch Done. If ctx ends first, the login carries on, bounded by
// ConnectTimeout, for whoever needs a session next.
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
		return nil, errClosed
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
	c.logger.Info("connecting to Nefit Easy backend",
		"host", c.config.Host,
		"jid", c.config.JID())

	ctx, cancel := context.WithTimeout(c.ctx, c.config.ConnectTimeout)
	t, err := c.dial(ctx)
	cancel()

	c.mu.Lock()
	switch {
	case c.ctx.Err() != nil:
		if err == nil {
			_ = t.Close()
		}
		err = errClosed
	case err == nil:
		d.cn = newConn(c.ctx, t)
		c.conn.Store(d.cn)
		c.wg.Go(func() { c.pingWorker(d.cn) })
		c.wg.Go(func() { c.receiveWorker(d.cn) })
	}
	d.err = err
	c.dialing = nil
	c.mu.Unlock()
	close(d.done)

	switch {
	case err == nil:
		c.logger.Info("connected to Nefit Easy backend")
	case !errors.Is(err, errClosed):
		c.logger.Error("failed to connect to Nefit Easy backend", "error", err)
	}
}

func (c *Client) dialBackend(ctx context.Context) (transport, error) {
	// Bosch servers require STARTTLS (plain TCP → TLS upgrade), not direct TLS
	options := xmpp.Options{
		User:     c.config.JID(),
		Password: c.config.AuthPassword(),
		NoTLS:    true,
		StartTLS: true,
		TLSConfig: &tls.Config{
			ServerName: c.config.Host,
			MinVersion: tls.VersionTLS12,
		},
		InsecureAllowUnencryptedAuth: false,
	}

	return dialXMPP(ctx, net.JoinHostPort(c.config.Host, strconv.Itoa(c.config.Port)), options)
}

// Close disconnects from the XMPP server, stops background workers and waits
// for running push handlers, so a handler must not call it. It is safe to
// call more than once.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.logger.Info("closing Nefit Easy client")

		c.mu.Lock()
		c.cancel()
		cn := c.conn.Swap(nil)
		c.mu.Unlock()

		if cn != nil {
			cn.close()
		}

		// Before waiting: handlers may be blocked submitting requests.
		c.queue.Close()
		c.wg.Wait()

		c.logger.Info("closed Nefit Easy client")
	})

	return nil
}

// IsConnected reports whether the current connection is alive.
func (c *Client) IsConnected() bool {
	cn := c.conn.Load()
	return cn != nil && cn.alive()
}

// Done returns a channel that is closed when the current connection ends:
// the stream fails, the client is closed, or a request fails after it may
// have gone out, since its late reply would pass for the answer to the next
// one. Without a connection it is already closed.
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
			if _, err := cn.xmpp.SendPresence(xmpp.Presence{}); err != nil {
				c.logger.Error("failed to send ping", "error", err)
				continue
			}
			c.logger.Debug("sent keepalive ping")
		}
	}
}

// receiveWorker owns the read side of cn. A read error ends the session: the
// stream decoder's error is sticky, so retrying could never succeed.
func (c *Client) receiveWorker(cn *conn) {
	defer cn.close()

	for {
		stanza, err := cn.xmpp.Recv()
		if cn.ctx.Err() != nil {
			// Retired: whatever arrives now has no owner.
			return
		}
		if err != nil {
			c.logger.Error("connection lost", "error", err)
			return
		}
		c.handleStanza(cn, stanza)
	}
}

func (c *Client) dispatchPushNotification(notification PushNotification) {
	c.eventHandlersMu.RLock()
	handlers := make([]EventHandler, len(c.eventHandlers))
	copy(handlers, c.eventHandlers)
	c.eventHandlersMu.RUnlock()

	// Concurrent so a slow handler cannot stall the stream; tracked so Close
	// does not return while one is still running.
	for _, handler := range handlers {
		c.wg.Go(func() { handler(notification.URI, notification.Data) })
	}
}

func (c *Client) handleStanza(cn *conn, stanza any) {
	switch v := stanza.(type) {
	case xmpp.Chat:
		c.handleChatMessage(cn, v)
	case xmpp.Presence, xmpp.IQ:
	default:
		c.logger.Debug("unknown stanza type", "type", fmt.Sprintf("%T", v))
	}
}

func (c *Client) handleChatMessage(cn *conn, msg xmpp.Chat) {
	c.logger.Debug("received chat message", "from", msg.Remote, "type", msg.Type)

	if msg.Type == "error" {
		c.logger.Error("received error message", "from", msg.Remote, "text", msg.Text)
		c.route(cn, reply{err: fmt.Errorf("XMPP error: %s", msg.Text)})
		return
	}

	if msg.Text == "" {
		return
	}

	resp, err := protocol.ParseHTTPResponse(msg.Text)
	if err != nil {
		c.logger.Error("failed to parse HTTP response", "error", err, "body", msg.Text)
		return
	}

	c.logger.Debug("parsed HTTP response", "status", resp.StatusCode)

	c.route(cn, c.decode(resp))
}

// Subscribe registers an event handler that will be called when the backend
// sends unsolicited push notifications. Multiple handlers can be registered.
// Handlers run concurrently; Close waits for them to return.
func (c *Client) Subscribe(handler EventHandler) {
	c.eventHandlersMu.Lock()
	defer c.eventHandlersMu.Unlock()
	c.eventHandlers = append(c.eventHandlers, handler)
}

// decode decrypts a successful reply's body and picks out the resource path
// the backend echoes as its JSON id.
func (c *Client) decode(resp *protocol.HTTPResponse) reply {
	r := reply{resp: resp}
	if resp.StatusCode != 200 || resp.Body == "" {
		return r
	}

	// Strip: AES-ECB pads with NUL bytes, which break the JSON parse.
	decrypted, err := c.encryptor.DecryptAndStrip(resp.Body)
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
func (c *Client) route(cn *conn, r reply) {
	if p := cn.match(r); p != nil {
		p.reply <- r
		return
	}

	if r.err != nil {
		c.logger.Warn("dropping unmatched reply", "error", r.err)
		return
	}

	if r.resp.StatusCode != 200 || r.data == nil {
		c.logger.Debug("dropping unmatched reply", "status", r.resp.StatusCode)
		return
	}

	c.logger.Info("push notification received", "uri", r.id, "data", r.data)
	c.dispatchPushNotification(PushNotification{URI: r.id, Data: r.data})
}

// roundTrip sends msg on the current session and waits for its reply. The
// session is read once: a reply can only arrive on the stream the request
// went out on.
func (c *Client) roundTrip(ctx context.Context, uri, msg string, get bool) (reply, error) {
	chat, err := chatOf(msg)
	if err != nil {
		return reply{}, err
	}

	cn, err := c.session(ctx)
	if err != nil {
		return reply{}, err
	}
	// Unsent, an expired request costs nothing.
	if err := ctx.Err(); err != nil {
		return reply{}, err
	}

	// The queue runs one request at a time, so the slot is free. Claim it
	// before sending: the reply may beat Send's return.
	p := &pending{uri: uri, get: get, reply: make(chan reply, 1)}
	cn.begin(p)

	// From here the request may reach the backend, whose replies carry no
	// request id: if it fails, its late reply would pass for the answer to
	// the next, so the session goes with it. Closing also aborts a write the
	// peer stopped reading.
	stop := context.AfterFunc(ctx, cn.close)
	defer stop()

	if _, err := cn.xmpp.Send(chat); err != nil {
		cn.close()
		if err := ctx.Err(); err != nil {
			return reply{}, err
		}
		return reply{}, fmt.Errorf("failed to send message: %w", err)
	}

	select {
	case r := <-p.reply:
		return r, r.err
	case <-cn.ctx.Done():
		if err := ctx.Err(); err != nil {
			return reply{}, err
		}
		return reply{}, errConnectionLost
	}
}

// chatOf unwraps the message stanza protocol builds, for go-xmpp to rewrap.
func chatOf(msg string) (xmpp.Chat, error) {
	var stanza struct {
		To   string `xml:"to,attr"`
		Body string `xml:"body"`
	}
	if err := xml.Unmarshal([]byte(msg), &stanza); err != nil {
		return xmpp.Chat{}, fmt.Errorf("failed to parse message: %w", err)
	}
	return xmpp.Chat{Remote: stanza.To, Type: "chat", Text: stanza.Body}, nil
}

// Get performs a GET request to the specified URI and returns the decrypted response data.
// The method automatically retries on timeout and deserializes JSON responses.
func (c *Client) Get(ctx context.Context, uri string) (any, error) {
	var lastErr error
	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if attempt > 0 {
			c.logger.Debug("retrying GET request", "uri", uri, "attempt", attempt)
		}

		reqCtx, cancel := context.WithTimeout(ctx, c.config.RetryTimeout)
		result, err := c.queue.Submit(reqCtx, func() (any, error) {
			return c.executeGet(reqCtx, uri)
		})
		cancel()

		if err == nil {
			return result, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			break
		}

		if err != context.DeadlineExceeded {
			break
		}
	}

	return nil, fmt.Errorf("GET request failed after %d attempts: %w", c.config.MaxRetries, lastErr)
}

func (c *Client) executeGet(ctx context.Context, uri string) (any, error) {
	msg := protocol.BuildGetMessage(c.config.JID(), c.config.ResourceJID(), uri)

	c.logger.Debug("sending GET request", "uri", uri)

	r, err := c.roundTrip(ctx, uri, msg, true)
	if err != nil {
		return nil, err
	}

	if r.resp.StatusCode != 200 {
		return nil, fmt.Errorf("HTTP error %d: %s", r.resp.StatusCode, r.resp.Status)
	}

	return r.data, nil
}

// Put performs a PUT request to the specified URI with the given data.
// Data is automatically marshalled to JSON and encrypted before sending.
// The method uses exponential backoff for retries on transient errors.
func (c *Client) Put(ctx context.Context, uri string, data any) error {
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

	c.logger.Debug("PUT request data prepared",
		"uri", uri,
		"json_data", jsonData,
		"json_length", len(jsonData))

	encrypted, err := c.encryptor.Encrypt(jsonData)
	if err != nil {
		return fmt.Errorf("failed to encrypt data: %w", err)
	}

	c.logger.Debug("PUT request encrypted",
		"uri", uri,
		"encrypted_length", len(encrypted))

	var lastErr error
	backoff := c.config.RetryTimeout
	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if attempt > 0 {
			c.logger.Debug("retrying PUT request",
				"uri", uri,
				"attempt", attempt,
				"backoff", backoff,
				"last_error", lastErr)

			// Exponential backoff: wait before retrying
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}

			// Double the backoff for next attempt, up to 30 seconds
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}

		reqCtx, cancel := context.WithTimeout(ctx, c.config.RetryTimeout)
		_, err := c.queue.Submit(reqCtx, func() (any, error) {
			return nil, c.executePut(reqCtx, uri, encrypted, jsonData)
		})
		cancel()

		if err == nil {
			if attempt > 0 {
				c.logger.Info("PUT request succeeded after retry",
					"uri", uri,
					"attempts", attempt+1)
			}
			return nil
		}

		lastErr = err

		if ctx.Err() != nil {
			break
		}

		// Only retry on timeout errors - 400 Bad Request indicates invalid data
		if err != context.DeadlineExceeded && !strings.Contains(err.Error(), "timeout") {
			c.logger.Warn("PUT request failed with non-retryable error",
				"uri", uri,
				"error", err,
				"json_data", jsonData)
			break
		}
	}

	return fmt.Errorf("PUT request failed after %d attempts: %w", c.config.MaxRetries+1, lastErr)
}

func (c *Client) executePut(ctx context.Context, uri, encryptedData, jsonData string) error {
	msg := protocol.BuildPutMessage(c.config.JID(), c.config.ResourceJID(), uri, encryptedData)

	c.logger.Debug("sending PUT request",
		"uri", uri,
		"from", c.config.JID(),
		"to", c.config.ResourceJID(),
		"encrypted_payload_length", len(encryptedData),
		"decrypted_json", jsonData)

	r, err := c.roundTrip(ctx, uri, msg, false)
	if err != nil {
		return err
	}

	if r.resp.StatusCode >= 300 {
		c.logger.Error("PUT request failed",
			"uri", uri,
			"status_code", r.resp.StatusCode,
			"status", r.resp.Status,
			"json_data", jsonData)
		return fmt.Errorf("HTTP error %d: %s", r.resp.StatusCode, r.resp.Status)
	}

	c.logger.Debug("PUT request successful",
		"uri", uri,
		"status_code", r.resp.StatusCode)

	return nil
}
