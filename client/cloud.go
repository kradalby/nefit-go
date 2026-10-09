package client

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wire "github.com/kradalby/nefit-go/internal/xmpp"
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
			// Ending ctx closes the socket, whose error would hide why.
			if ctx.Err() != nil {
				err = ctx.Err()
			}
		}
	}()
	t := &cloudTransport{socket: socket, cfg: cfg, done: make(chan struct{})}
	t.resetReader(socket)
	t.w = newSocketWriter(socket, t.done, func() { _ = t.Close() }, 15*time.Second)
	features, err := t.open()
	if err != nil {
		return nil, err
	}
	if features.Child(wire.TLSNS, "starttls") == nil {
		return nil, fmt.Errorf("bosch server did not offer STARTTLS")
	}
	start := wire.E(wire.TLSNS, "starttls")
	if err = t.write(start); err != nil {
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
	t.w.socket = secure
	t.resetReader(secure)
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
	auth := wire.SASLText("auth", base64.StdEncoding.EncodeToString([]byte("\x00"+RRCContactPrefix+cfg.SerialNumber+"\x00"+cfg.AuthPassword())))
	auth.Mechanism = "PLAIN"
	if err = t.write(auth); err != nil {
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
	if err = t.write(wire.IQ{Type: "set", ID: "bind_1", Bind: &wire.Bind{Resource: hex.EncodeToString(resourceBytes)}}); err != nil {
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
		if err = t.write(wire.IQ{Type: "set", ID: "sess_1", Session: &session, To: cfg.Host}); err != nil {
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
	if err = t.write(wire.Presence{From: t.jid}); err != nil {
		return nil, err
	}
	if !stop() && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return t, nil
}

type cloudTransport struct {
	socket    net.Conn
	input     *receivedReader
	reader    *wire.Reader
	reply     atomic.Pointer[replyWindow]
	cfg       Config
	jid       string
	w         socketWriter
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

type replyWindow struct {
	owner  *pending
	offset int64
}

func (t *cloudTransport) resetReader(r io.Reader) {
	t.input = &receivedReader{reader: r}
	t.reader = wire.NewReader(t.input)
}

func (t *cloudTransport) open() (*wire.Element, error) {
	if err := t.write(wire.Frame{Stream: &wire.Stream{To: t.cfg.Host, Version: "1.0", Lang: "en"}}); err != nil {
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

// write encodes plain XML: a conforming server would normalise literal CR in
// HTTP bodies away, so the cloud keeps character references.
func (t *cloudTransport) write(v any) error {
	return t.writeContext(context.Background(), v, nil)
}

func (t *cloudTransport) writeContext(ctx context.Context, v any, onStart func()) error {
	data, err := wire.Marshal(v, false)
	if err != nil {
		return &unsentError{err}
	}
	return t.w.write(ctx, data, nil, onStart)
}

func (t *cloudTransport) Close() error {
	t.closeOnce.Do(func() {
		close(t.done)
		t.closeErr = t.socket.Close()
	})
	return t.closeErr
}

func (t *cloudTransport) Send(ctx context.Context, body string, p *pending) error {
	return t.writeContext(ctx, wire.Message{From: t.jid, To: t.cfg.ResourceJID(), Type: "chat", Body: wire.Body{Text: body}}, func() {
		offset := t.input.bytes.Load()
		p.markSent()
		t.reply.Store(&replyWindow{owner: p, offset: offset})
	})
}

func (t *cloudTransport) Ping() error {
	return t.write(wire.Presence{From: t.jid})
}

func (t *cloudTransport) Recv() (inbound, error) {
	for {
		frame, err := t.reader.Next()
		if err != nil {
			return inbound{}, err
		}
		if frame.End {
			return inbound{}, io.EOF
		}
		if frame.Element == nil {
			return inbound{}, fmt.Errorf("unexpected cloud stream restart")
		}
		e := frame.Element
		switch e.Name {
		case xml.Name{Space: wire.ClientNS, Local: "message"}:
			in := inbound{error: e.Get("type") == "error"}
			if body := e.Child(wire.ClientNS, "body"); body != nil {
				in.text = body.Text()
			}
			// Bosch app sessions share our bare JID: only the gateway's
			// messages to this resource can answer our request.
			from, _, _ := strings.Cut(e.Get("from"), "/")
			to := e.Get("to")
			in.push = from != t.cfg.ResourceJID() || to != "" && to != t.jid
			if window := t.reply.Load(); window != nil && window.owner != nil && !window.owner.replied.Load() && frame.Offset >= window.offset {
				in.owner = window.owner
			} else {
				in.push = true
			}
			return in, nil
		case xml.Name{Space: wire.ClientNS, Local: "iq"}:
			if reply, ok := iqReply(e, t.jid); ok {
				if err := t.write(reply); err != nil {
					return inbound{}, err
				}
			}
		case xml.Name{Space: wire.StreamNS, Local: "error"}:
			return inbound{}, fmt.Errorf("bosch stream error")
		}
	}
}
