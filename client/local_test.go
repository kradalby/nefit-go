package client

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func localTestClient(t *testing.T, timeout time.Duration) *Client {
	t.Helper()
	c, err := NewLocalClient(Config{
		SerialNumber: "123456789", AccessKey: "abcdefghijklmnop", Password: "secret",
		ConnectTimeout: timeout, RetryTimeout: time.Second, PingInterval: time.Hour,
	},
		LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

type deviceFixture struct {
	socket net.Conn
	reader *bufio.Reader
}

func (d *deviceFixture) readThrough(t *testing.T, end string) string {
	t.Helper()
	var text strings.Builder
	for !strings.Contains(text.String(), end) {
		part, err := d.reader.ReadString('>')
		if err != nil {
			t.Fatal(err)
		}
		text.WriteString(part)
	}
	return text.String()
}

func (d *deviceFixture) write(t *testing.T, text string) {
	t.Helper()
	if _, err := io.WriteString(d.socket, text); err != nil {
		t.Fatal(err)
	}
}

func connectFixture(t *testing.T, c *Client) *deviceFixture {
	t.Helper()
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(t.Context()) }()
	s, err := net.Dial("tcp", c.LocalAddress().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	d := &deviceFixture{socket: s, reader: bufio.NewReader(s)}
	header := fmt.Sprintf(`<stream:stream from="rrcgateway_%s" to="%s" xmlns="jabber:client" xmlns:stream="http://etherx.jabber.org/streams" version="1.0">`, c.config.SerialNumber, c.config.Host)
	d.write(t, header)
	d.readThrough(t, "</stream:features>")
	d.write(t, `<auth xmlns="urn:ietf:params:xml:ns:xmpp-sasl" mechanism="DIGEST-MD5"/>`)
	d.readThrough(t, "</challenge>")
	d.write(t, `<response xmlns="urn:ietf:params:xml:ns:xmpp-sasl">`+base64.StdEncoding.EncodeToString([]byte("test digest response"))+`</response>`)
	proof := d.readThrough(t, "</success>")
	if !strings.Contains(proof, base64.StdEncoding.EncodeToString([]byte("rspauth="+strings.Repeat("0", 32)))) {
		t.Fatal("missing compatible rspauth")
	}
	d.write(t, header)
	d.readThrough(t, "</stream:features>")
	d.write(t, `<iq type="set" id="bind_1"><bind xmlns="urn:ietf:params:xml:ns:xmpp-bind"><resource>RRC-RestApi</resource></bind></iq>`)
	d.readThrough(t, "</iq>")
	d.write(t, `<iq type="set" id="sess_1"><session xmlns="urn:ietf:params:xml:ns:xmpp-session"/></iq>`)
	ack := d.readThrough(t, "/>")
	// Firmware requires the captured acknowledgement layout, including attribute
	// order. A conforming XML parser alone cannot expose this compatibility bug.
	if !strings.HasPrefix(ack, `<iq type="result" id="sess_1"`) {
		t.Fatal("session acknowledgement differs from successful capture")
	}
	d.write(t, `<presence><status>RRC</status></presence>`)
	if err := wait(t, connected); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *deviceFixture) reply(t *testing.T, c *Client, body string) {
	t.Helper()
	d.write(t, fmt.Sprintf(`<message to="%s/localprobe" type="chat"><body>%s</body></message>`, c.config.JID(), body))
}

func TestLocalClientReadsWritesAndReconnects(t *testing.T) {
	c := localTestClient(t, time.Second)
	d := connectFixture(t, c)
	for cycle := 0; cycle < 2; cycle++ {
		result := make(chan any, 1)
		go func() {
			value, err := c.Get(t.Context(), "/ecus/rrc/uiStatus")
			if err != nil {
				result <- err
			} else {
				result <- value
			}
		}()
		request := d.readThrough(t, "</message>")
		if !strings.Contains(request, "GET /ecus/rrc/uiStatus HTTP/1.1\r\nUser-Agent: NefitEasy\r\n\r\n") {
			t.Fatalf("incorrect device line endings: %q", request)
		}
		// An encrypted cloud-service message must never satisfy the API request.
		d.write(t, `<message to="gservice_time@example.com"><body>not an HTTP response</body></message>`)
		encoded, err := c.encryptor.Encrypt(`{"id":"/ecus/rrc/uiStatus","value":{"TSP":"14.0"}}`)
		if err != nil {
			t.Fatal(err)
		}
		d.reply(t, c, "HTTP/1.0 200 OK\r\nContent-Type: application/json\r\n\r\n"+encoded)
		value := wait(t, result)
		if err, ok := value.(error); ok {
			t.Fatal(err)
		}
		if value.(map[string]any)["value"].(map[string]any)["TSP"] != "14.0" {
			t.Fatalf("bad decrypted reply: %v", value)
		}
		written := make(chan error, 1)
		go func() {
			written <- c.Put(t.Context(), "/heatingCircuits/hc1/temperatureRoomManual", map[string]any{"value": 14.0})
		}()
		request = d.readThrough(t, "</message>")
		var stanza struct {
			Body string `xml:"body"`
		}
		if err := xml.Unmarshal([]byte(request), &stanza); err != nil {
			t.Fatal(err)
		}
		_, encrypted, ok := strings.Cut(stanza.Body, "\n\n")
		if !ok {
			t.Fatalf("invalid PUT: %q", stanza.Body)
		}
		plain, err := c.encryptor.DecryptAndStrip(encrypted)
		if err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		if err := json.Unmarshal([]byte(plain), &data); err != nil {
			t.Fatal(err)
		}
		if data["value"] != 14.0 {
			t.Fatalf("wrong encrypted PUT: %s", plain)
		}
		d.reply(t, c, "HTTP/1.0 204 No Content\r\nContent-Type: application/json\r\n\r\n")
		if err := wait(t, written); err != nil {
			t.Fatal(err)
		}
		if cycle == 0 {
			done := c.Done()
			_ = d.socket.Close()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("disconnect not detected")
			}
			d = connectFixture(t, c)
		}
	}
}

func TestLocalClientCloseWhileAccepting(t *testing.T) {
	c := localTestClient(t, time.Hour)
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(context.Background()) }()
	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	if err := wait(t, closed); err != nil {
		t.Fatal(err)
	}
	if err := wait(t, connected); err == nil {
		t.Fatal("Connect succeeded after Close")
	}
}

func TestLocalClientHandshakeTimeout(t *testing.T) {
	c := localTestClient(t, 100*time.Millisecond)
	connected := make(chan error, 1)
	go func() { connected <- c.Connect(context.Background()) }()
	s, err := net.Dial("tcp", c.LocalAddress().String())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck
	if err := wait(t, connected); err == nil {
		t.Fatal("silent handshake succeeded")
	}
}

func TestLocalHandshakeRejectsWrongSerial(t *testing.T) {
	server, device := net.Pipe()
	defer device.Close() //nolint:errcheck
	defer server.Close() //nolint:errcheck
	result := make(chan error, 1)
	go func() {
		_, err := acceptLocal(t.Context(), server, Config{SerialNumber: "expected", Host: "domain"})
		result <- err
	}()
	_, err := io.WriteString(device, `<stream:stream xmlns:stream="http://etherx.jabber.org/streams" from="rrcgateway_wrong" to="domain">`)
	if err != nil {
		t.Fatal(err)
	}
	if err := wait(t, result); err == nil {
		t.Fatal("wrong serial accepted")
	}
}
