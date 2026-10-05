package client

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	wire "github.com/kradalby/nefit-go/xmpp"
)

func TestExtensionBodyNewlinesPreserved(t *testing.T) {
	original := "HTTP/1.0 200 OK\r\nContent-Length: 0\r\n\r\n"
	extension := wire.Text("urn:example:extension", "body", original)
	message := wire.Message{Body: wire.Body{Text: "ordinary message"}, Extensions: []wire.Element{extension}}
	var buffer bytes.Buffer
	if _, err := writeTyped(&buffer, message); err != nil {
		t.Fatal(err)
	}
	reader := wire.NewReader(strings.NewReader(`<stream:stream xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams">` + buffer.String()))
	if _, err := reader.Next(); err != nil {
		t.Fatal(err)
	}
	frame, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	got := frame.Element.Child(extension.Name.Space, extension.Name.Local).Text()
	if got != original {
		t.Fatalf("extension changed from %q to %q; wire %s", original, got, buffer.String())
	}
}

func TestCloudAnswersIQRequests(t *testing.T) {
	cfg, tlsCfg, ready := tlsCloudFixture(t)
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	c.dial = func(ctx context.Context) (transport, error) { return dialCloud(ctx, cfg, tlsCfg) }
	t.Cleanup(func() { _ = c.Close() })
	if err := c.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	cloud := wait(t, ready)
	t.Cleanup(func() { _ = cloud.conn.Close() })
	for _, kind := range []string{"get", "set", "ping"} {
		extension := wire.E("jabber:iq:version", "query")
		requestType := kind
		if kind == "ping" {
			extension = wire.E(wire.PingNS, "ping")
			requestType = "get"
		}
		query := wire.IQ{Type: requestType, ID: kind, From: cfg.Host, To: cfg.JID() + "/nefit00101", Extensions: []wire.Element{extension}}
		cloud.write(t, query)
		response := cloud.next(t)
		expectedType := "error"
		if kind == "ping" {
			expectedType = "result"
		}
		if response.Get("id") != query.ID || response.Get("type") != expectedType || response.Get("from") != query.To || response.Get("to") != query.From {
			t.Fatal("incorrect IQ response", response)
		}
		if expectedType == "error" {
			stanzaError := response.Child(wire.ClientNS, "error")
			if stanzaError == nil || stanzaError.Get("type") != "cancel" || stanzaError.Child("urn:ietf:params:xml:ns:xmpp-stanzas", "service-unavailable") == nil {
				t.Fatal("missing service-unavailable error")
			}
		}
	}
}

func TestLocalReplyWithNamespacedMetadata(t *testing.T) {
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
