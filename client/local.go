package client

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
)

const (
	localResource = "localprobe"
)

// ErrListenerFailed marks a device listener that can no longer accept
// connections. Connect and subsequent requests return it with the Accept
// cause; Done wakes owners of an existing session. The client must be closed
// and recreated to recover.
var ErrListenerFailed = errors.New("local XMPP listener failed")

// ServerMode selects whether the device is served locally or also relayed.
type ServerMode string

const (
	ModeOffline ServerMode = "offline" // serve the device locally only
	ModeBoth    ServerMode = "both"    // also relay its login and app traffic to Bosch
)

// UpdatePolicy controls update-related XMPP traffic. Block refuses update
// writes, malformed request lines and writes to non-canonical targets in any
// element's text, bodies with markup, and traffic to or from gservice_update,
// gservice_firmware, UpdateServices and any JID that is not plain ASCII;
// independent firmware downloads need egress rules.
type UpdatePolicy string

const (
	UpdatesAllow UpdatePolicy = "allow" // relay update traffic unchanged
	UpdatesBlock UpdatePolicy = "block" // refuse it, as UpdatePolicy describes
)

// LocalOptions configures the embedded device server. The source IP and gateway
// identity restrict access: compatible offline DIGEST-MD5 does not verify a secret.
type LocalOptions struct {
	// ListenAddress is the device XMPP listener (default 127.0.0.1:5222,
	// which the thermostat cannot reach).
	ListenAddress string
	// DeviceIP, required, is the only source address admitted.
	DeviceIP netip.Addr
	Mode     ServerMode // default ModeOffline
	// UpstreamAddress overrides the Bosch host:port relayed to in ModeBoth.
	UpstreamAddress string
	UpdatePolicy    UpdatePolicy // default UpdatesAllow
	// UpdateServices are additional bare JIDs (or localparts) to block under
	// UpdatesBlock.
	UpdateServices []string
	// RequestTimeout bounds how long the device may take to answer a request
	// (default 15s). An unanswered local request then reconnects the device;
	// an unanswered app request relayed from Bosch is answered 504.
	RequestTimeout time.Duration
	// ReconnectInterval paces the Bosch reachability probe in ModeBoth and is
	// the first backoff after Bosch is unreachable, rejects the relayed login
	// or drops the relay (default 1m). Each further failure doubles it, up to
	// 32 times the interval; a relay that outlives an interval resets it.
	ReconnectInterval time.Duration

	// logf reaches transports before their login completes, so login-time
	// warnings follow SetLogger.
	logf func() *slog.Logger
}

// NewLocalClient binds the device listener and returns a client served by the
// thermostat logging in to it instead of Bosch. Devices are admitted until
// Close, so a fresh login replaces a stale session.
func NewLocalClient(config Config, options LocalOptions) (*Client, error) {
	if !options.DeviceIP.IsValid() {
		return nil, errors.New("local device IP is required")
	}
	options.DeviceIP = options.DeviceIP.Unmap()
	if ip := options.DeviceIP; ip.IsUnspecified() || ip.IsMulticast() || ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		// Nothing would ever be admitted, and nothing would say why.
		return nil, fmt.Errorf("local device IP %q is not a device address", options.DeviceIP)
	}
	if options.ListenAddress == "" {
		options.ListenAddress = "127.0.0.1:5222"
	}
	if options.Mode == "" {
		options.Mode = ModeOffline
	}
	if options.Mode != ModeOffline && options.Mode != ModeBoth {
		return nil, fmt.Errorf("invalid server mode %q", options.Mode)
	}
	if options.UpstreamAddress != "" {
		if options.Mode != ModeBoth {
			return nil, fmt.Errorf("upstream address requires %s mode", ModeBoth)
		}
		// Dialled only once a device logs in, so a typo would surface late.
		host, port, err := net.SplitHostPort(options.UpstreamAddress)
		number := 0
		if err == nil {
			number, err = net.LookupPort("tcp", port)
		}
		if err == nil && (host == "" || number == 0) {
			// An empty host dials this machine, and port 0 nothing.
			err = errors.New("empty host or port 0")
		}
		if err != nil {
			return nil, fmt.Errorf("upstream address %q needs a host and port: %w", options.UpstreamAddress, err)
		}
	}
	if options.UpdatePolicy == "" {
		options.UpdatePolicy = UpdatesAllow
	}
	if options.UpdatePolicy != UpdatesAllow && options.UpdatePolicy != UpdatesBlock {
		return nil, fmt.Errorf("invalid update policy %q", options.UpdatePolicy)
	}
	if len(options.UpdateServices) > 0 && options.UpdatePolicy != UpdatesBlock {
		// Ignoring them would leave the named services unblocked.
		return nil, fmt.Errorf("update services require the %s policy", UpdatesBlock)
	}
	services := make([]string, len(options.UpdateServices))
	for i, service := range options.UpdateServices {
		// An empty entry matches every stanza without an address, the login's
		// SASL exchange and the device's replies included.
		if services[i] = bareJID(service); services[i] == "" {
			return nil, fmt.Errorf("update service %q names no JID", service)
		}
	}
	options.UpdateServices = services
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 15 * time.Second
	}
	if options.ReconnectInterval <= 0 {
		options.ReconnectInterval = time.Minute
	}
	c, err := NewClient(config)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", options.ListenAddress)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	c.localListener = ln
	c.localMode = options.Mode
	c.localReady = make(chan struct{})
	options.logf = c.log
	c.wg.Go(func() { c.acceptDevices(options) })
	return c, nil
}

// Admission outlives Connect calls: a fresh login must not wait for a silent
// session to time out. Failed logins leave the current session untouched.
func (c *Client) acceptDevices(options LocalOptions) {
	health := new(upstreamHealth)
	var delay time.Duration
	var refused time.Time
	for c.ctx.Err() == nil {
		socket, err := c.localListener.Accept()
		if err != nil {
			if c.ctx.Err() != nil {
				return
			}
			if !retryAccept(err) {
				c.failListener(err)
				return
			}
			// Capped so recovery stays prompt.
			delay = min(max(2*delay, 5*time.Millisecond), time.Second)
			c.log().Warn("device listener retrying", "error", err, "delay", delay)
			timer := time.NewTimer(delay)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		delay = 0
		if peer := socket.RemoteAddr().(*net.TCPAddr).AddrPort().Addr(); !devicePeer(peer, options.DeviceIP) {
			// Warned at most once a minute: a changed or mistyped device IP
			// would otherwise leave no trace.
			level := slog.LevelDebug
			if time.Since(refused) >= time.Minute {
				level, refused = slog.LevelWarn, time.Now()
			}
			c.log().Log(c.ctx, level, "refusing a connection from another address", "peer", peer, "device", options.DeviceIP)
			_ = socket.Close()
			continue
		}
		// ConnectTimeout bounds waiting for a device; one login gets less.
		ctx, cancel := context.WithTimeout(c.ctx, min(c.config.ConnectTimeout, 15*time.Second))
		var t *localTransport
		switch {
		case options.Mode != ModeBoth || !health.ready():
			t, err = acceptLocal(ctx, socket, c.config, options)
		default:
			// Logging in again may make Bosch end the current relay, as
			// servers do for a rebound resource; that is no Bosch failure.
			var current *localTransport
			if cn := c.conn.Load(); cn != nil {
				current, _ = cn.xmpp.(*localTransport)
			}
			if current != nil {
				current.superseded.Store(true)
			}
			t, err = acceptBoth(ctx, socket, c.config, options)
			if err != nil && current != nil {
				current.superseded.Store(false)
			}
			// Bosch failed the login, or was unreachable and the device is served offline.
			var upstream *upstreamError
			if errors.As(err, &upstream) || err == nil && t.upstream == nil {
				health.fail(options.ReconnectInterval)
			}
		}
		if err == nil {
			err = ctx.Err()
		}
		cancel()
		if err != nil {
			if t != nil {
				_ = t.Close()
			}
			_ = socket.Close()
			if c.ctx.Err() != nil {
				return
			}
			c.log().Warn("device handshake failed", "error", err)
			continue
		}
		t.health, t.encryptor = health, c.encryptor
		c.mu.Lock()
		if c.ctx.Err() != nil {
			c.mu.Unlock()
			_ = t.Close()
			return
		}
		// Published first, so a caller woken by the old Done finds the new one.
		old := c.conn.Load()
		c.publishSession(t)
		if old != nil {
			old.cancel(nil)
		}
		close(c.localReady)
		c.localReady = make(chan struct{})
		c.mu.Unlock()
		// Logged here: a replacement wakes no Connect, which finds it live.
		c.log().Info("device logged in", "mode", options.Mode, "upstream", t.upstreamConnected.Load(), "replaced", old != nil)
		if old != nil {
			old.close()
		}
	}
}

// devicePeer reports whether peer is the device: a mapped IPv4 address is
// the IPv4 one, and a zone counts only when the device IP names one.
func devicePeer(peer, device netip.Addr) bool {
	if peer = peer.Unmap(); device.Zone() == "" {
		peer = peer.WithZone("")
	}
	return peer == device
}

// retryAccept treats only a dead listener as permanent: accept(2) also
// reports errors from the incoming connection and resource pressure, which
// pass.
func retryAccept(err error) bool {
	return !errors.Is(err, net.ErrClosed) && !errors.Is(err, syscall.EBADF) && !errors.Is(err, syscall.EINVAL)
}

func (c *Client) failListener(cause error) {
	c.mu.Lock()
	if c.ctx.Err() != nil || c.localErr != nil {
		c.mu.Unlock()
		return
	}
	c.localErr = fmt.Errorf("%w: %w", ErrListenerFailed, cause)
	err := c.localErr
	close(c.localReady)
	cn := c.conn.Load()
	if cn != nil {
		// Wake Done before closing the transport, which may wait on workers.
		cn.cancel(nil)
	}
	c.mu.Unlock()
	c.log().Error("device listener stopped", "error", err)
	_ = c.localListener.Close()
	if cn != nil {
		cn.close()
	}
}

func (c *Client) waitLocal(ctx context.Context) (*conn, error) {
	for {
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
		ready := c.localReady
		c.mu.Unlock()
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// LocalAddress is the device listener's address, or nil for a cloud client.
func (c *Client) LocalAddress() net.Addr {
	if c.localListener == nil {
		return nil
	}
	return c.localListener.Addr()
}

// UpstreamConnected reports whether the current session reaches Bosch: a
// cloud session, or a local one relaying in ModeBoth.
func (c *Client) UpstreamConnected() bool {
	if cn := c.conn.Load(); cn != nil && cn.alive() {
		switch t := cn.xmpp.(type) {
		case *localTransport:
			return t.upstreamConnected.Load()
		case *cloudTransport:
			return true
		}
	}
	return false
}

func newLocalTransport(socket net.Conn, cfg Config, options LocalOptions) *localTransport {
	input := &receivedReader{reader: socket}
	t := &localTransport{socket: socket, input: input, reader: wire.NewReader(input), config: cfg, options: options}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.w = newSocketWriter(socket, t.ctx.Done(), t.abort, options.RequestTimeout)
	return t
}

func (t *localTransport) write(v any) error {
	return t.writeContext(context.Background(), v, nil, nil)
}

// writeContext writes in the device's lexical form; state and onStart are
// those of a queued request (see socketWriter.write).
func (t *localTransport) writeContext(ctx context.Context, v any, state *atomic.Int32, onStart func()) error {
	data, err := wire.Marshal(v, true)
	if err != nil {
		return &unsentError{err}
	}
	return t.w.write(ctx, data, state, onStart)
}

// writeTyped writes v in the device's lexical form.
func writeTyped(w io.Writer, v any) (int, error) {
	data, err := wire.Marshal(v, true)
	if err != nil {
		return 0, err
	}
	return w.Write(data)
}

func (t *localTransport) Close() error {
	t.abort()
	t.workers.Wait()
	return nil
}

// Workers can retire a damaged stream without joining themselves.
func (t *localTransport) abort() {
	t.closeOnce.Do(func() {
		t.workerMu.Lock()
		t.closing = true
		t.workerMu.Unlock()
		t.cancel()
		_ = t.socket.Close()
		if t.upstream != nil {
			_ = t.upstream.Close()
		}
	})
}

func (t *localTransport) Send(ctx context.Context, body string, p *pending) error {
	if err := ctx.Err(); err != nil {
		return &unsentError{err}
	}
	message := wire.Message{From: t.config.JID() + "/" + localResource, To: t.config.ResourceJID(), Type: "chat", Body: wire.Body{Text: body}}
	if t.blocked(message) {
		return &unsentError{ErrUpdateBlocked}
	}
	return t.submit(ctx, message, p)
}

func (t *localTransport) Ping() error {
	return t.write(wire.Presence{From: t.config.JID() + "/" + localResource, To: t.config.ResourceJID()})
}

func validateHeader(s *wire.Stream, cfg Config) error {
	if s == nil || (s.From != RRCGatewayPrefix+cfg.SerialNumber && s.From != cfg.ResourceJID()) || s.To != cfg.Host {
		return fmt.Errorf("unexpected device identity or XMPP domain")
	}
	return nil
}

// acceptLocal serves a device login itself and returns its bridged session,
// with no Bosch relay.
func acceptLocal(ctx context.Context, socket net.Conn, cfg Config, options LocalOptions) (_ *localTransport, err error) {
	t := newLocalTransport(socket, cfg, options)
	defer func() {
		if err != nil {
			t.cancel()
		}
	}()
	deadline, _ := ctx.Deadline()
	if err := socket.SetDeadline(deadline); err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	stage := 0
	bound, session := false, false
	for {
		frame, err := t.reader.Next()
		if err != nil {
			return nil, err
		}
		var response any
		if frame.Stream != nil {
			if err := validateHeader(frame.Stream, cfg); err != nil {
				return nil, err
			}
			if stage != 0 && stage != 3 {
				return nil, fmt.Errorf("unexpected stream restart")
			}
			id := make([]byte, 8)
			if _, err = rand.Read(id); err != nil {
				return nil, err
			}
			// RFC 6120 4.7.3: a fresh, unpredictable id for every stream.
			if err = t.write(wire.Frame{Stream: &wire.Stream{From: cfg.Host, ID: hex.EncodeToString(id), Version: "1.0"}}); err != nil {
				return nil, err
			}
			var features wire.Element
			if stage == 0 {
				features = wire.E(wire.StreamNS, "features", wire.E(wire.SASLNS, "mechanisms", wire.Text(wire.SASLNS, "mechanism", "DIGEST-MD5")))
				stage = 1
			} else {
				features = wire.E(wire.StreamNS, "features", wire.E(wire.BindNS, "bind"), wire.E(wire.SessionNS, "session"))
				stage = 4
			}
			response = features
		} else if frame.Element != nil {
			e := frame.Element
			switch {
			case e.Name == (xml.Name{Space: wire.SASLNS, Local: "auth"}) && stage == 1:
				if e.Get("mechanism") != "DIGEST-MD5" {
					return nil, fmt.Errorf("unsupported device authentication")
				}
				nonce := make([]byte, 16)
				if _, err = rand.Read(nonce); err != nil {
					return nil, err
				}
				challenge := fmt.Sprintf(`realm="%s",nonce="%s",charset=utf-8,algorithm=md5-sess`, cfg.Host, hex.EncodeToString(nonce))
				response = wire.SASLText("challenge", base64.StdEncoding.EncodeToString([]byte(challenge)))
				stage = 2
			case e.Name == (xml.Name{Space: wire.SASLNS, Local: "response"}) && stage == 2:
				decoded, err := base64.StdEncoding.DecodeString(e.Text())
				if err != nil || len(decoded) == 0 {
					return nil, fmt.Errorf("invalid SASL response")
				}
				// Device firmware accepts this proof. Source-IP restriction is mandatory;
				// this exchange is protocol compatibility, not password authentication.
				response = wire.SASLText("success", base64.StdEncoding.EncodeToString([]byte("rspauth="+strings.Repeat("0", 32))))
				stage = 3
			case e.Name == (xml.Name{Space: wire.ClientNS, Local: "iq"}) && stage == 4:
				value, err := e.Typed()
				if err != nil {
					return nil, err
				}
				iq := value.(*wire.IQ)
				if iq.Bind != nil {
					if bound || iq.Bind.Resource != "RRC-RestApi" {
						return nil, fmt.Errorf("unexpected device resource")
					}
					response = wire.IQ{Type: "result", ID: iq.ID, Bind: &wire.Bind{JID: cfg.ResourceJID() + "/RRC-RestApi"}}
					bound = true
				} else if iq.Session != nil && bound && !session {
					response = wire.IQ{Type: "result", ID: iq.ID, From: cfg.Host, To: cfg.ResourceJID() + "/RRC-RestApi"}
					session = true
				} else {
					return nil, fmt.Errorf("unexpected handshake IQ")
				}
			case e.Name == (xml.Name{Space: wire.ClientNS, Local: "presence"}) && stage == 4 && bound && session:
				if !stop() && ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if err := socket.SetDeadline(time.Time{}); err != nil {
					return nil, err
				}
				t.initBridge(nil)
				return t, nil
			default:
				return nil, fmt.Errorf("unexpected handshake stanza %s", e.Name.Local)
			}
		} else {
			return nil, fmt.Errorf("device ended handshake")
		}
		if err = t.write(response); err != nil {
			return nil, err
		}
	}
}
