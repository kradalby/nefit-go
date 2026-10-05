package server

import (
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kradalby/nefit-go/client"
)

func TestHTTPRejectsInvalidWritesWithoutDevice(t *testing.T) {
	c, err := client.NewLocalClient(client.Config{SerialNumber: "123", AccessKey: "key", Password: "secret"},
		client.LocalOptions{ListenAddress: "127.0.0.1:0", DeviceIP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	defer c.Close() //nolint:errcheck
	handler := NewHandler(c, time.Second)
	for _, test := range []struct {
		method, path, body string
		code               int
	}{
		{"PUT", "/heatingCircuits/hc1/temperatureRoomManual", "{", 400},
		{"POST", "/bridge/ecus/rrc/usermode", `{"value":"manual"} {"value":"clock"}`, 400},
		{"PUT", "/api/temperature", `{}`, 400},
		{"PUT", "/api/temperature", `{"value":null}`, 400},
		{"PUT", "/api/temperature", `{"value":"warm"}`, 400},
		{"PUT", "/api/user-mode", `{"value":"off"}`, 400},
		{"PUT", "/api/hot-water", `{"value":1}`, 400},
		{"DELETE", "/bridge/ecus/rrc/uiStatus", "", 405},
		{"GET", "/healthz", "", 503},
	} {
		t.Run(test.method+test.path+test.body, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			handler.ServeHTTP(recorder, request)
			if recorder.Code != test.code {
				t.Fatalf("status %d, want %d: %s", recorder.Code, test.code, recorder.Body.String())
			}
			if c.IsConnected() {
				t.Fatal("invalid input connected to a device")
			}
		})
	}
}
