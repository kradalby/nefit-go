package client

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kradalby/nefit-go/crypto"
	"github.com/kradalby/nefit-go/protocol"
	xmpp "github.com/xmppo/go-xmpp"
)

// EventHandler is called when unsolicited messages are received from the backend
type EventHandler func(uri string, data interface{})

// PushNotification represents a queued push notification
type PushNotification struct {
	URI  string
	Data interface{}
}

// Client represents an active connection to the Nefit Easy backend.
// It handles XMPP communication, encryption, request queueing, and push notifications.
type Client struct {
	config    Config
	encryptor *crypto.Encryptor
	queue     *RequestQueue

	xmppClient *xmpp.Client
	connMu     sync.RWMutex

	// Backend limitation: only one concurrent request allowed, so we need request/response correlation
	pendingRequests map[string]chan *protocol.HTTPResponse
	pendingErrors   map[string]chan error
	pendingMu       sync.RWMutex

	eventHandlers        []EventHandler
	eventHandlersMu      sync.RWMutex
	pushNotificationChan chan PushNotification

	logger *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewClient creates a new Nefit Easy client with the given configuration.
// The client must be explicitly connected using Connect() before use.
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
		config:               config,
		encryptor:            encryptor,
		queue:                NewRequestQueue(),
		pendingRequests:      make(map[string]chan *protocol.HTTPResponse),
		pendingErrors:        make(map[string]chan error),
		pushNotificationChan: make(chan PushNotification, 100),
		logger:               slog.Default(),
		ctx:                  ctx,
		cancel:               cancel,
	}

	return client, nil
}

// SetLogger configures a custom logger for the client.
// By default, the client uses slog.Default().
func (c *Client) SetLogger(logger *slog.Logger) {
	c.logger = logger
}

// Connect establishes the XMPP connection and starts background workers.
// The connection uses STARTTLS (plain TCP upgraded to TLS) as required by Bosch servers.
func (c *Client) Connect(ctx context.Context) error {
	c.logger.Info("connecting to Nefit Easy backend",
		"host", c.config.Host,
		"jid", c.config.JID())

	// Bosch servers require STARTTLS (plain TCP → TLS upgrade), not direct TLS
	options := xmpp.Options{
		Host:     fmt.Sprintf("%s:%d", c.config.Host, c.config.Port),
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

	xmppClient, err := options.NewClient()
	if err != nil {
		return fmt.Errorf("failed to create XMPP client: %w", err)
	}

	c.connMu.Lock()
	c.xmppClient = xmppClient
	c.connMu.Unlock()

	c.logger.Info("connected to Nefit Easy backend")

	c.wg.Add(3)
	go c.pingWorker()
	go c.receiveWorker()
	go c.pushNotificationWorker()

	return nil
}

// Close disconnects from the XMPP server and cleans up resources.
// It gracefully shuts down all background workers and drains any pending push notifications.
func (c *Client) Close() error {
	c.logger.Info("closing Nefit Easy client")

	c.cancel()

	c.connMu.Lock()
	if c.xmppClient != nil {
		_ = c.xmppClient.Close()
		c.xmppClient = nil
	}
	c.connMu.Unlock()

	close(c.pushNotificationChan)

	c.wg.Wait()
	c.queue.Close()

	c.logger.Info("closed Nefit Easy client")

	return nil
}

// IsConnected checks whether the client currently has an active XMPP connection.
func (c *Client) IsConnected() bool {
	c.connMu.RLock()
	defer c.connMu.RUnlock()
	return c.xmppClient != nil
}

func (c *Client) pingWorker() {
	defer c.wg.Done()

	ticker := time.NewTicker(c.config.PingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ticker.C:
			if err := c.sendPing(); err != nil {
				c.logger.Error("failed to send ping", "error", err)
			}
		}
	}
}

func (c *Client) sendPing() error {
	c.connMu.RLock()
	client := c.xmppClient
	c.connMu.RUnlock()

	if client == nil {
		return fmt.Errorf("not connected")
	}

	_, err := client.SendPresence(xmpp.Presence{})
	if err != nil {
		return fmt.Errorf("failed to send presence: %w", err)
	}

	c.logger.Debug("sent keepalive ping")
	return nil
}

func (c *Client) receiveWorker() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			return
		default:
			if err := c.receiveMessage(); err != nil {
				c.logger.Error("error receiving message", "error", err)
				// Add a small delay to prevent tight loop on errors
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
}

func (c *Client) pushNotificationWorker() {
	defer c.wg.Done()

	for {
		select {
		case <-c.ctx.Done():
			// Context cancelled - drain remaining messages before exiting
			c.logger.Debug("push notification worker shutting down, draining queue")
			c.drainPushNotifications()
			return
		case notification, ok := <-c.pushNotificationChan:
			if !ok {
				// Channel closed - drain any remaining messages
				c.logger.Debug("push notification channel closed")
				return
			}
			c.dispatchPushNotification(notification)
		}
	}
}

func (c *Client) drainPushNotifications() {
	for {
		select {
		case notification, ok := <-c.pushNotificationChan:
			if !ok {
				return
			}
			c.dispatchPushNotification(notification)
		default:
			return
		}
	}
}

func (c *Client) dispatchPushNotification(notification PushNotification) {
	c.eventHandlersMu.RLock()
	handlers := make([]EventHandler, len(c.eventHandlers))
	copy(handlers, c.eventHandlers)
	c.eventHandlersMu.RUnlock()

	// Each handler runs concurrently to avoid blocking on slow handlers
	for _, handler := range handlers {
		go handler(notification.URI, notification.Data)
	}
}

func (c *Client) receiveMessage() error {
	c.connMu.RLock()
	client := c.xmppClient
	c.connMu.RUnlock()

	if client == nil {
		return fmt.Errorf("not connected")
	}

	stanza, err := client.Recv()
	if err != nil {
		return fmt.Errorf("failed to receive stanza: %w", err)
	}

	switch v := stanza.(type) {
	case xmpp.Chat:
		return c.handleChatMessage(v)
	case xmpp.Presence:
		// Ignore presence for now
		return nil
	case xmpp.IQ:
		// Ignore IQ for now
		return nil
	default:
		c.logger.Debug("unknown stanza type", "type", fmt.Sprintf("%T", v))
		return nil
	}
}

func (c *Client) handleChatMessage(msg xmpp.Chat) error {
	c.logger.Debug("received chat message", "from", msg.Remote, "type", msg.Type)

	if msg.Type == "error" {
		c.logger.Error("received error message", "from", msg.Remote, "text", msg.Text)
		c.notifyError(fmt.Errorf("XMPP error: %s", msg.Text))
		return nil
	}

	if msg.Text != "" {
		resp, err := protocol.ParseHTTPResponse(msg.Text)
		if err != nil {
			c.logger.Error("failed to parse HTTP response", "error", err, "body", msg.Text)
			return nil
		}

		c.logger.Debug("parsed HTTP response", "status", resp.StatusCode)

		// Check if this is a response to a pending request or an unsolicited push notification
		c.pendingMu.RLock()
		hasPendingRequests := len(c.pendingRequests) > 0
		c.pendingMu.RUnlock()

		if hasPendingRequests {
			// This is likely a response to our request
			c.notifyResponse(resp)
		} else {
			// This is an unsolicited push notification from the backend
			c.handlePushNotification(resp)
		}
	}

	return nil
}

// Subscribe registers an event handler that will be called when the backend
// sends unsolicited push notifications. Multiple handlers can be registered.
func (c *Client) Subscribe(handler EventHandler) {
	c.eventHandlersMu.Lock()
	defer c.eventHandlersMu.Unlock()
	c.eventHandlers = append(c.eventHandlers, handler)
}

func (c *Client) handlePushNotification(resp *protocol.HTTPResponse) {
	c.logger.Debug("received push notification", "status", resp.StatusCode)

	if resp.Body != "" && resp.StatusCode == 200 {
		decrypted, err := c.encryptor.Decrypt(resp.Body)
		if err != nil {
			c.logger.Error("failed to decrypt push notification", "error", err)
			return
		}

		var data interface{}
		if resp.ContentType == "application/json" {
			if err := json.Unmarshal([]byte(decrypted), &data); err != nil {
				c.logger.Warn("failed to parse JSON push notification", "error", err, "data", decrypted)
				data = decrypted
			}
		} else {
			data = decrypted
		}

		// Extract URI from the data if possible (the response might contain an 'id' field with the URI)
		uri := ""
		if dataMap, ok := data.(map[string]interface{}); ok {
			if id, ok := dataMap["id"].(string); ok {
				uri = id
			}
		}

		c.logger.Info("push notification received", "uri", uri, "data", data)

		select {
		case c.pushNotificationChan <- PushNotification{URI: uri, Data: data}:
		default:
			// Channel full - log warning but don't block
			c.logger.Warn("push notification queue full, dropping message", "uri", uri)
		}
	}
}

func (c *Client) notifyResponse(resp *protocol.HTTPResponse) {
	c.pendingMu.RLock()
	defer c.pendingMu.RUnlock()

	for _, ch := range c.pendingRequests {
		select {
		case ch <- resp:
		default:
		}
	}
}

func (c *Client) notifyError(err error) {
	c.pendingMu.RLock()
	defer c.pendingMu.RUnlock()

	for _, ch := range c.pendingErrors {
		select {
		case ch <- err:
		default:
		}
	}
}

func (c *Client) sendMessage(msg string) error {
	c.connMu.RLock()
	client := c.xmppClient
	c.connMu.RUnlock()

	if client == nil {
		return fmt.Errorf("not connected")
	}

	var msgStanza struct {
		To   string `xml:"to,attr"`
		Body string `xml:"body"`
	}
	if err := xml.Unmarshal([]byte(msg), &msgStanza); err != nil {
		return fmt.Errorf("failed to parse message: %w", err)
	}

	_, err := client.Send(xmpp.Chat{
		Remote: msgStanza.To,
		Type:   "chat",
		Text:   msgStanza.Body,
	})
	return err
}

// Get performs a GET request to the specified URI and returns the decrypted response data.
// The method automatically retries on timeout and deserializes JSON responses.
func (c *Client) Get(ctx context.Context, uri string) (interface{}, error) {
	if !c.IsConnected() {
		return nil, fmt.Errorf("not connected")
	}

	var lastErr error
	for attempt := 0; attempt <= c.config.MaxRetries; attempt++ {
		if attempt > 0 {
			c.logger.Debug("retrying GET request", "uri", uri, "attempt", attempt)
		}

		reqCtx, cancel := context.WithTimeout(ctx, c.config.RetryTimeout)
		result, err := c.queue.Submit(reqCtx, func() (interface{}, error) {
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

func (c *Client) executeGet(ctx context.Context, uri string) (interface{}, error) {
	msg := protocol.BuildGetMessage(c.config.JID(), c.config.ResourceJID(), uri)

	c.logger.Debug("sending GET request", "uri", uri)

	responseCh := make(chan *protocol.HTTPResponse, 1)
	errorCh := make(chan error, 1)

	reqID := fmt.Sprintf("get:%s:%d", uri, time.Now().UnixNano())
	c.pendingMu.Lock()
	c.pendingRequests[reqID] = responseCh
	c.pendingErrors[reqID] = errorCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pendingRequests, reqID)
		delete(c.pendingErrors, reqID)
		c.pendingMu.Unlock()
	}()

	if err := c.sendMessage(msg); err != nil {
		return nil, fmt.Errorf("failed to send message: %w", err)
	}

	select {
	case resp := <-responseCh:
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("HTTP error %d: %s", resp.StatusCode, resp.Status)
		}

		decrypted, err := c.encryptor.DecryptAndStrip(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("decryption failed: %w", err)
		}

		if strings.Contains(resp.ContentType, "json") {
			var result interface{}
			if err := json.Unmarshal([]byte(decrypted), &result); err != nil {
				return decrypted, nil
			}
			return result, nil
		}

		return decrypted, nil

	case err := <-errorCh:
		return nil, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Put performs a PUT request to the specified URI with the given data.
// Data is automatically marshalled to JSON and encrypted before sending.
// The method uses exponential backoff for retries on transient errors.
func (c *Client) Put(ctx context.Context, uri string, data interface{}) error {
	if !c.IsConnected() {
		return fmt.Errorf("not connected")
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
		_, err := c.queue.Submit(reqCtx, func() (interface{}, error) {
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

	responseCh := make(chan *protocol.HTTPResponse, 1)
	errorCh := make(chan error, 1)

	reqID := fmt.Sprintf("put:%s:%d", uri, time.Now().UnixNano())
	c.pendingMu.Lock()
	c.pendingRequests[reqID] = responseCh
	c.pendingErrors[reqID] = errorCh
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pendingRequests, reqID)
		delete(c.pendingErrors, reqID)
		c.pendingMu.Unlock()
	}()

	if err := c.sendMessage(msg); err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	select {
	case resp := <-responseCh:
		if resp.StatusCode >= 300 {
			c.logger.Error("PUT request failed",
				"uri", uri,
				"status_code", resp.StatusCode,
				"status", resp.Status,
				"json_data", jsonData)
			return fmt.Errorf("HTTP error %d: %s", resp.StatusCode, resp.Status)
		}
		c.logger.Debug("PUT request successful",
			"uri", uri,
			"status_code", resp.StatusCode)
		return nil
	case err := <-errorCh:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
