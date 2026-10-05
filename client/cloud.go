package client

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	legacy "github.com/xmppo/go-xmpp"

	wire "github.com/kradalby/nefit-go/xmpp"
)

// dialCloud owns every socket and uses the same typed codec as the device server.
// PLAIN authentication is sent only after verified STARTTLS completes.
func dialCloud(ctx context.Context, cfg Config, tlsConfig *tls.Config) (_ transport, err error) {
	var d net.Dialer
	socket, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stop()
	defer func() {
		if err != nil {
			_ = socket.Close()
			if ctx.Err() != nil {
				err = ctx.Err()
			} else {
				var timeout net.Error
				deadline, ok := ctx.Deadline()
				if ok && errors.As(err, &timeout) && timeout.Timeout() && !time.Now().Before(deadline) {
					err = context.DeadlineExceeded
				}
			}
		}
	}()
	if deadline, ok := ctx.Deadline(); ok {
		_ = socket.SetDeadline(deadline)
	}
	t := &cloudTransport{socket: socket, reader: wire.NewReader(socket), cfg: cfg}
	features, err := t.open()
	if err != nil {
		return nil, err
	}
	if features.Child(wire.TLSNS, "starttls") == nil {
		return nil, fmt.Errorf("bosch server did not offer STARTTLS")
	}
	start := wire.E(wire.TLSNS, "starttls")
	if _, err = t.write(start); err != nil {
		return nil, err
	}
	frame, err := t.reader.Next()
	if err != nil {
		return nil, err
	}
	if frame.Element == nil || frame.Element.Name != (xml.Name{Space: wire.TLSNS, Local: "proceed"}) {
		return nil, fmt.Errorf("STARTTLS rejected")
	}
	secure := tls.Client(socket, tlsConfig)
	if err = secure.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	t.socket = secure
	t.reader = wire.NewReader(secure)
	features, err = t.open()
	if err != nil {
		return nil, err
	}
	mechanisms := features.Child(wire.SASLNS, "mechanisms")
	plain := false
	if mechanisms != nil {
		for _, n := range mechanisms.Children {
			if n.Element != nil && n.Element.Name.Local == "mechanism" && n.Element.Text() == "PLAIN" {
				plain = true
			}
		}
	}
	if !plain {
		return nil, fmt.Errorf("bosch server did not offer PLAIN inside TLS")
	}
	auth := wire.Auth("PLAIN")
	auth.Text = base64.StdEncoding.EncodeToString([]byte("\x00" + RRCContactPrefix + cfg.SerialNumber + "\x00" + cfg.AuthPassword()))
	if _, err = t.write(auth); err != nil {
		return nil, err
	}
	frame, err = t.reader.Next()
	if err != nil {
		return nil, err
	}
	if frame.Element == nil || frame.Element.Name != (xml.Name{Space: wire.SASLNS, Local: "success"}) {
		return nil, fmt.Errorf("bosch contact authentication rejected")
	}
	features, err = t.open()
	if err != nil {
		return nil, err
	}
	if features.Child(wire.BindNS, "bind") == nil {
		return nil, fmt.Errorf("bosch server did not offer resource binding")
	}
	resourceBytes := make([]byte, 5)
	if _, err = rand.Read(resourceBytes); err != nil {
		return nil, err
	}
	if _, err = t.write(wire.IQ{Type: "set", ID: "bind_1", Bind: &wire.Bind{Resource: hex.EncodeToString(resourceBytes)}}); err != nil {
		return nil, err
	}
	frame, err = t.reader.Next()
	if err != nil {
		return nil, err
	}
	if frame.Element == nil || frame.Element.Get("type") != "result" || frame.Element.Get("id") != "bind_1" {
		return nil, fmt.Errorf("resource binding rejected")
	}
	bind := frame.Element.Child(wire.BindNS, "bind")
	if bind == nil || bind.Child(wire.BindNS, "jid") == nil {
		return nil, fmt.Errorf("missing bound JID")
	}
	t.jid = bind.Child(wire.BindNS, "jid").Text()
	if features.Child(wire.SessionNS, "session") != nil {
		session := wire.E(wire.SessionNS, "session")
		if _, err = t.write(wire.IQ{Type: "set", ID: "sess_1", Session: &session, To: cfg.Host}); err != nil {
			return nil, err
		}
		frame, err = t.reader.Next()
		if err != nil {
			return nil, err
		}
		if frame.Element == nil || frame.Element.Get("type") != "result" || frame.Element.Get("id") != "sess_1" {
			return nil, fmt.Errorf("session establishment rejected")
		}
	}
	if _, err = t.write(wire.Presence{From: t.jid}); err != nil {
		return nil, err
	}
	if !stop() && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	_ = socket.SetDeadline(time.Time{})
	return t, nil
}

type cloudTransport struct {
	socket net.Conn
	reader *wire.Reader
	cfg    Config
	jid    string
	mu     sync.Mutex
}

func (t *cloudTransport) open() (*wire.Element, error) {
	if _, err := t.write(wire.Frame{Stream: &wire.Stream{To: t.cfg.Host, Version: "1.0", Lang: "en"}}); err != nil {
		return nil, err
	}
	header, err := t.reader.Next()
	if err != nil {
		return nil, err
	}
	if header.Stream == nil {
		return nil, fmt.Errorf("missing Bosch stream header")
	}
	features, err := t.reader.Next()
	if err != nil {
		return nil, err
	}
	if features.Element == nil || features.Element.Name != (xml.Name{Space: wire.StreamNS, Local: "features"}) {
		return nil, fmt.Errorf("missing Bosch stream features")
	}
	return features.Element, nil
}

func (t *cloudTransport) write(v any) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.socket.SetWriteDeadline(time.Now().Add(15 * time.Second))
	defer func() { _ = t.socket.SetWriteDeadline(time.Time{}) }()
	return writeTyped(t.socket, v)
}
func (t *cloudTransport) Close() error { return t.socket.Close() }
func (t *cloudTransport) Send(chat legacy.Chat) (int, error) {
	return t.write(wire.Message{From: t.jid, To: chat.Remote, Type: chat.Type, Body: wire.Body{Text: chat.Text}})
}

func (t *cloudTransport) SendPresence(_ legacy.Presence) (int, error) {
	return t.write(wire.Presence{From: t.jid})
}

func (t *cloudTransport) Recv() (any, error) {
	for {
		frame, err := t.reader.Next()
		if err != nil {
			return nil, err
		}
		if frame.End {
			return nil, io.EOF
		}
		if frame.Element == nil {
			return nil, fmt.Errorf("unexpected cloud stream restart")
		}
		e := frame.Element
		switch e.Name {
		case xml.Name{Space: wire.ClientNS, Local: "message"}:
			value, err := e.Typed()
			if err != nil {
				return nil, err
			}
			m := value.(*wire.Message)
			return legacy.Chat{Remote: m.From, Type: m.Type, Text: m.Body.Text}, nil
		case xml.Name{Space: wire.ClientNS, Local: "presence"}:
			return legacy.Presence{}, nil
		case xml.Name{Space: wire.ClientNS, Local: "iq"}:
			if e.Get("type") == "get" && e.Child(wire.PingNS, "ping") != nil {
				if _, err := t.write(wire.IQ{Type: "result", ID: e.Get("id"), From: t.jid, To: e.Get("from")}); err != nil {
					return nil, err
				}
			} else if e.Get("type") == "get" || e.Get("type") == "set" {
				stanzaError := wire.E(wire.ClientNS, "error", wire.E("urn:ietf:params:xml:ns:xmpp-stanzas", "service-unavailable"))
				stanzaError.Set("type", "cancel")
				if _, err := t.write(wire.IQ{Type: "error", ID: e.Get("id"), From: t.jid, To: e.Get("from"), Extensions: []wire.Element{stanzaError}}); err != nil {
					return nil, err
				}
			}
		case xml.Name{Space: wire.StreamNS, Local: "error"}:
			return nil, fmt.Errorf("bosch stream error")
		}
	}
}
