// Package server exposes the thermostat API over HTTP using a cloud or local client.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/kradalby/nefit-go/client"
)

type inputError string

func (e inputError) Error() string { return string(e) }

// NewHandler serves raw thermostat paths (GET/PUT), /bridge paths (GET/PUT/POST),
// and the high-level status, pressure, temperature, user-mode and hot-water APIs.
// It does not authenticate HTTP callers; bind it to loopback or protect it with
// an authenticated reverse proxy. Requests share the client's serialized queue.
func NewHandler(c *client.Client, timeout time.Duration) http.Handler {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	mux := http.NewServeMux()
	respond := func(w http.ResponseWriter, result any, err error) {
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			status := http.StatusBadGateway
			var invalid inputError
			if errors.As(err, &invalid) {
				status = http.StatusBadRequest
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}
	decode := func(w http.ResponseWriter, r *http.Request, result any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(result); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return false
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			http.Error(w, "expected one JSON value", http.StatusBadRequest)
			return false
		}
		return true
	}
	raw := func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		uri := "/" + r.PathValue("path")
		if r.URL.RawQuery != "" {
			uri += "?" + r.URL.RawQuery
		}
		if r.Method == http.MethodGet {
			data, err := c.Get(ctx, uri)
			respond(w, data, err)
			return
		}
		var data any
		if !decode(w, r, &data) {
			return
		}
		respond(w, map[string]bool{"ok": true}, c.Put(ctx, uri, data))
	}
	for _, method := range []string{"GET", "PUT"} {
		mux.HandleFunc(method+" /{path...}", raw)
	}
	for _, method := range []string{"GET", "PUT", "POST"} {
		mux.HandleFunc(method+" /bridge/{path...}", raw)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		connected := c.IsConnected()
		w.Header().Set("Content-Type", "application/json")
		if !connected {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		respond(w, map[string]bool{"connected": connected, "upstreamConnected": c.UpstreamConnected()}, nil)
	})
	api := func(fn func(context.Context, *http.Request) (any, error)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			result, err := fn(ctx, r)
			respond(w, result, err)
		}
	}
	mux.HandleFunc("GET /api/status", api(func(ctx context.Context, r *http.Request) (any, error) {
		return c.Status(ctx, r.URL.Query().Get("outdoor") == "true")
	}))
	mux.HandleFunc("GET /api/pressure", api(func(ctx context.Context, _ *http.Request) (any, error) { return c.Pressure(ctx) }))
	mux.HandleFunc("GET /api/hot-water", api(func(ctx context.Context, _ *http.Request) (any, error) { return c.HotWaterSupply(ctx) }))
	for _, entry := range []struct {
		path string
		put  func(context.Context, json.RawMessage) error
	}{
		{"temperature", func(ctx context.Context, b json.RawMessage) error {
			var value float64
			if err := json.Unmarshal(b, &value); err != nil {
				return inputError("temperature must be a number")
			}
			return c.SetTemperature(ctx, value)
		}},
		{"user-mode", func(ctx context.Context, b json.RawMessage) error {
			var value string
			if err := json.Unmarshal(b, &value); err != nil {
				return inputError("user mode must be a string")
			}
			if value != "manual" && value != "clock" {
				return inputError("user mode must be manual or clock")
			}
			return c.SetUserMode(ctx, value)
		}},
		{"hot-water", func(ctx context.Context, b json.RawMessage) error {
			var value bool
			if err := json.Unmarshal(b, &value); err != nil {
				return inputError("hot water value must be a boolean")
			}
			return c.SetHotWaterSupply(ctx, value)
		}},
	} {
		mux.HandleFunc("PUT /api/"+entry.path, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Value json.RawMessage `json:"value"`
			}
			if !decode(w, r, &body) {
				return
			}
			if len(body.Value) == 0 || string(body.Value) == "null" {
				http.Error(w, "value is required", http.StatusBadRequest)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			respond(w, map[string]bool{"ok": true}, entry.put(ctx, body.Value))
		})
	}
	return mux
}
