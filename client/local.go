package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	legacy "github.com/xmppo/go-xmpp"

	wire "github.com/kradalby/nefit-go/xmpp"
)

const (
	localResource = "localprobe"
	streamNS      = wire.StreamNS
)

// ServerMode selects whether the device is served locally or also relayed.
type ServerMode string

const (
	ModeOffline ServerMode = "offline"
	ModeBoth    ServerMode = "both"
)

// UpdatePolicy controls update-related XMPP traffic. Block applies to configured
// service JIDs and update writes; independent firmware downloads need egress rules.
type UpdatePolicy string

const (
	UpdatesAllow UpdatePolicy = "allow"
	UpdatesBlock UpdatePolicy = "block"
)

// LocalOptions configures the embedded device server. The source IP and gateway
// identity restrict access: compatible offline DIGEST-MD5 does not verify a secret.
type LocalOptions struct {
	ListenAddress   string
	DeviceIP        net.IP
	Mode            ServerMode
	UpstreamAddress string
	UpdatePolicy    UpdatePolicy
	// UpdateServices are additional bare JIDs (or localparts) to block.
	UpdateServices    []string
	RequestTimeout    time.Duration
	ReconnectInterval time.Duration
	// Service handles offline service messages. nil leaves unknown services unanswered.
	Service ServiceHandler
}

// ServiceHandler returns a typed response to a device's service request. Returning
// nil acknowledges that no response is available. It must honor ctx cancellation.
type ServiceHandler func(ctx context.Context, request wire.Message) (*wire.Message, error)

func NewLocalClient(config Config, options LocalOptions) (*Client, error) {
	options.UpdateServices = append([]string(nil), options.UpdateServices...)
	if options.DeviceIP == nil {
		return nil, fmt.Errorf("local device IP is required")
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
	if options.UpdatePolicy == "" {
		options.UpdatePolicy = UpdatesAllow
	}
	if options.UpdatePolicy != UpdatesAllow && options.UpdatePolicy != UpdatesBlock {
		return nil, fmt.Errorf("invalid update policy %q", options.UpdatePolicy)
	}
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
	deviceIP := append(net.IP(nil), options.DeviceIP...)
	var upstreamBackoffUntil time.Time
	c.dial = func(ctx context.Context) (transport, error) {
		listener := ln.(*net.TCPListener)
		deadline, _ := ctx.Deadline()
		if err := listener.SetDeadline(deadline); err != nil {
			return nil, err
		}
		stop := context.AfterFunc(ctx, func() { _ = listener.SetDeadline(time.Now()) })
		defer stop()
		for {
			conn, err := listener.Accept()
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, err
			}
			if !conn.RemoteAddr().(*net.TCPAddr).IP.Equal(deviceIP) {
				_ = conn.Close()
				continue
			}
			var t *localTransport
			if options.Mode == ModeBoth {
				if time.Now().Before(upstreamBackoffUntil) {
					t, err = acceptLocal(ctx, conn, c.config)
					if err == nil {
						t.options = options
						t.initBridge(nil)
					}
				} else {
					t, err = acceptBoth(ctx, conn, c.config, options)
					if err != nil {
						upstreamBackoffUntil = time.Now().Add(options.ReconnectInterval)
					}
				}
			} else {
				t, err = acceptLocal(ctx, conn, c.config)
			}
			if err == nil {
				t.options = options
				t.logger = c.logger
				return t, nil
			}
			_ = conn.Close()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			c.logger.Warn("device handshake failed", "error", err)
		}
	}
	return c, nil
}

func (c *Client) LocalAddress() net.Addr {
	if c.localListener == nil {
		return nil
	}
	return c.localListener.Addr()
}

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

type localTransport struct {
	socket    net.Conn
	reader    *wire.Reader
	config    Config
	options   LocalOptions
	writeOnce sync.Once
	writer    chan struct{}
	bridgeState
}

func (t *localTransport) write(v any) (int, error) {
	return t.writeContext(context.Background(), v, nil)
}

// Acquire the writer and claim the request only immediately before socket I/O.
// Presence/service traffic may hold the writer while a queued API call expires.
func (t *localTransport) writeContext(ctx context.Context, v any, state *atomic.Int32) (int, error) {
	data, err := encodeTyped(v)
	if err != nil {
		return 0, &unsentError{err}
	}
	t.writeOnce.Do(func() { t.writer = make(chan struct{}, 1) })
	select {
	case t.writer <- struct{}{}:
	case <-ctx.Done():
		return 0, &unsentError{ctx.Err()}
	}
	defer func() { <-t.writer }()
	if err := ctx.Err(); err != nil {
		return 0, &unsentError{err}
	}
	if state != nil && !state.CompareAndSwap(queued, started) {
		return 0, &unsentError{context.Canceled}
	}
	timeout := t.options.RequestTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	_ = t.socket.SetWriteDeadline(time.Now().Add(timeout))
	defer func() { _ = t.socket.SetWriteDeadline(time.Time{}) }()
	stop := context.AfterFunc(ctx, func() { _ = t.socket.Close() })
	defer stop()
	return writeBytes(t.socket, data)
}

func writeTyped(w io.Writer, v any) (int, error) {
	data, err := encodeTyped(v)
	if err != nil {
		return 0, err
	}
	return writeBytes(w, data)
}

func writeBytes(w io.Writer, data []byte) (int, error) {
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

func encodeTyped(v any) ([]byte, error) {
	var buffer bytes.Buffer
	var err error
	var element *wire.Element
	if frame, ok := v.(wire.Frame); ok {
		if frame.Element != nil {
			element = frame.Element
		} else {
			err = wire.WriteFrame(&buffer, frame)
		}
	} else {
		raw, marshalErr := xml.Marshal(v)
		if marshalErr != nil {
			return nil, marshalErr
		}
		element = new(wire.Element)
		err = xml.Unmarshal(raw, element)
	}
	if err != nil {
		return nil, err
	}
	if element != nil {
		enc := xml.NewEncoder(&buffer)
		err = element.EncodeInStream(enc)
		if err == nil {
			err = enc.Flush()
		}
	}
	if err != nil {
		return nil, err
	}
	return wire.DeviceXML(buffer.Bytes())
}

func (t *localTransport) Close() error {
	t.closeOnce.Do(func() {
		if t.cancel != nil {
			t.cancel()
		}
		_ = t.socket.Close()
		if t.upstream != nil {
			_ = t.upstream.Close()
		}
	})
	t.workers.Wait()
	return nil
}

func (t *localTransport) Send(chat legacy.Chat) (int, error) {
	return t.SendContext(context.Background(), chat)
}

func (t *localTransport) SendContext(ctx context.Context, chat legacy.Chat) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, &unsentError{err}
	}
	message := wire.Message{From: t.config.JID() + "/" + localResource, To: chat.Remote, Type: "chat", Body: wire.Body{Text: chat.Text}}
	if t.blocked(message) {
		return 0, &unsentError{errors.New("firmware update write blocked by policy")}
	}
	if t.inputs == nil {
		return t.writeContext(ctx, message, nil)
	}
	return t.submit(ctx, message)
}

func (t *localTransport) SendPresence(_ legacy.Presence) (int, error) {
	return t.write(wire.Presence{From: t.config.JID() + "/" + localResource, To: t.config.ResourceJID()})
}

func validateHeader(s *wire.Stream, cfg Config) error {
	if s == nil || (s.From != RRCGatewayPrefix+cfg.SerialNumber && s.From != cfg.ResourceJID()) || s.To != cfg.Host {
		return fmt.Errorf("unexpected device identity or XMPP domain")
	}
	return nil
}

func acceptLocal(ctx context.Context, socket net.Conn, cfg Config) (*localTransport, error) {
	t := &localTransport{socket: socket, reader: wire.NewReader(socket), config: cfg}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	deadline := time.Now().Add(15 * time.Second)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
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
			if _, err = t.write(wire.Frame{Stream: &wire.Stream{From: cfg.Host, ID: "local-nefit", Version: "1.0"}}); err != nil {
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
				return t, nil
			default:
				return nil, fmt.Errorf("unexpected handshake stanza %s", e.Name.Local)
			}
		} else {
			return nil, fmt.Errorf("device ended handshake")
		}
		if _, err = t.write(response); err != nil {
			return nil, err
		}
	}
}

func (t *localTransport) Recv() (any, error) {
	if t.inputs != nil {
		return t.recvBoth()
	}
	for {
		frame, err := t.reader.Next()
		if err != nil {
			return nil, err
		}
		if frame.End {
			return nil, io.EOF
		}
		if frame.Element == nil {
			return nil, fmt.Errorf("unexpected stream restart")
		}
		if t.blockedFrame(frame) {
			continue
		}
		e := frame.Element
		switch e.Name {
		case xml.Name{Space: wire.ClientNS, Local: "message"}:
			value, err := e.Typed()
			if err != nil {
				return nil, err
			}
			m := value.(*wire.Message)
			if m.To != t.config.JID()+"/"+localResource {
				if err := t.service(*m); err != nil {
					return nil, err
				}
				continue
			}
			return legacy.Chat{Remote: t.config.ResourceJID(), Type: m.Type, Text: m.Body.Text}, nil
		case xml.Name{Space: wire.ClientNS, Local: "presence"}:
			return legacy.Presence{}, nil
		case xml.Name{Space: wire.ClientNS, Local: "iq"}:
			if e.Get("type") == "get" && e.Child(wire.PingNS, "ping") != nil {
				if _, err := t.write(wire.IQ{Type: "result", ID: e.Get("id"), From: e.Get("to"), To: e.Get("from")}); err != nil {
					return nil, err
				}
			}
		case xml.Name{Space: wire.StreamNS, Local: "error"}:
			return nil, fmt.Errorf("device stream error")
		}
	}
}

func (t *localTransport) service(m wire.Message) error {
	if t.options.Service == nil || t.blocked(m) {
		return nil
	}
	ctx := t.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, t.options.RequestTimeout)
	defer cancel()
	response, err := t.options.Service(ctx, m)
	if err != nil {
		return err
	}
	if response != nil {
		frame, err := messageFrame(*response)
		if err != nil {
			return err
		}
		if !t.blockedFrame(frame) {
			_, err = t.write(frame)
		}
		return err
	}
	return err
}
