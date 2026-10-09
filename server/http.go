// Package server exposes local thermostat XMPP, HTTP control and scoped DNS.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/kradalby/nefit-go/client"
	"github.com/kradalby/nefit-go/protocol"
)

// maxBodyBytes is far above any thermostat value; encrypted, a body this size
// still fits the largest stanza the encoder writes.
const maxBodyBytes = 64 << 10

type inputError string

func (e inputError) Error() string { return string(e) }

// NewHandler serves raw thermostat paths (GET/PUT), /bridge paths (GET/PUT/POST),
// and the high-level status, pressure, temperature, user-mode and hot-water APIs.
// It does not authenticate HTTP callers; bind it to loopback or protect it with
// an authenticated reverse proxy. Requests share the client's serialized queue.
//
// Loopback hosts and the connection's local IP are trusted. allowedHosts adds
// hostnames or host:port authorities, such as a LAN DNS name or a reverse
// proxy's public name; an entry without a port permits any port. Forwarded
// headers never grant access. An invalid entry is an error.
//
// timeout (default 10s) bounds each request's device work, not how long a
// client may take to send it: serve the handler from an http.Server with
// ReadHeaderTimeout, ReadTimeout and a small MaxHeaderBytes set.
func NewHandler(c *client.Client, timeout time.Duration, allowedHosts ...string) (http.Handler, error) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	hosts := make(trustedHosts)
	for _, authority := range allowedHosts {
		host, port, ok := parseAuthority(authority)
		if !ok {
			return nil, fmt.Errorf("invalid allowed HTTP host %q", authority)
		}
		hosts[host+"\x00"+port] = true
	}
	decode := func(w http.ResponseWriter, r *http.Request, result any) bool {
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return false
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		err = decoder.Decode(result)
		if err == nil {
			// Only EOF proves the body complete; a stalled tail is not a value.
			switch err = decoder.Decode(&json.RawMessage{}); err {
			case io.EOF:
				err = nil
			case nil:
				err = inputError("expected one JSON value")
			}
		}
		var tooLarge *http.MaxBytesError
		var network net.Error
		var input inputError
		switch {
		case errors.As(err, &tooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d KiB", maxBodyBytes>>10))
		case r.Context().Err() != nil || errors.As(err, &network) && network.Timeout():
			// A late body must not start device work after its deadline.
			writeError(w, http.StatusRequestTimeout, "request body timed out")
		case errors.As(err, &input):
			writeError(w, http.StatusBadRequest, input.Error())
		case err != nil:
			writeError(w, http.StatusBadRequest, "invalid JSON")
		default:
			return true
		}
		return false
	}
	ok := map[string]bool{"ok": true}
	raw := func(w http.ResponseWriter, r *http.Request) {
		uri := r.URL.EscapedPath()
		if rest, found := strings.CutPrefix(uri, "/bridge/"); found {
			uri = "/" + rest
		}
		if r.URL.RawQuery != "" {
			uri += "?" + r.URL.RawQuery
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			data, err := c.Get(r.Context(), uri)
			respond(w, data, err)
			return
		}
		var body json.RawMessage
		if !decode(w, r, &body) {
			return
		}
		// A string is sent verbatim; marshalling would rewrite <, > and &.
		var value bytes.Buffer
		_ = json.Compact(&value, body)
		respond(w, ok, c.Put(r.Context(), uri, value.String()))
	}
	device := http.NewServeMux()
	for _, method := range []string{"GET", "PUT"} {
		device.HandleFunc(method+" /{path...}", raw)
	}
	for _, method := range []string{"GET", "PUT", "POST"} {
		device.HandleFunc(method+" /bridge/{path...}", raw)
	}

	// The API has its own mux so wrong methods and unknown paths get 405/404
	// instead of falling through to the device.
	api := http.NewServeMux()
	api.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		connected := c.IsConnected()
		status := http.StatusOK
		if !connected {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]bool{"connected": connected, "upstreamConnected": c.UpstreamConnected()})
	})
	api.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		result, err := c.Status(r.Context(), r.URL.Query().Get("outdoor") == "true")
		respond(w, result, err)
	})
	api.HandleFunc("GET /api/pressure", func(w http.ResponseWriter, r *http.Request) {
		result, err := c.Pressure(r.Context())
		respond(w, result, err)
	})
	api.HandleFunc("GET /api/hot-water", func(w http.ResponseWriter, r *http.Request) {
		result, err := c.HotWaterSupply(r.Context())
		respond(w, result, err)
	})
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
		api.HandleFunc("PUT /api/"+entry.path, func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Value json.RawMessage `json:"value"`
			}
			if !decode(w, r, &body) {
				return
			}
			if len(body.Value) == 0 || string(body.Value) == "null" {
				writeError(w, http.StatusBadRequest, "value is required")
				return
			}
			respond(w, ok, entry.put(r.Context(), body.Value))
		})
	}

	protection := http.NewCrossOriginProtection()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Otherwise net/http reads an unread body before writing any response,
		// so a rejection would wait on a stalled client.
		_ = http.NewResponseController(w).EnableFullDuplex()
		if !hosts.allow(r) {
			writeError(w, http.StatusForbidden, "untrusted HTTP host")
			return
		}
		// CrossOriginProtection exempts GET and HEAD, yet a blind cross-site
		// read still makes the device work.
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeError(w, http.StatusForbidden, "cross-site request")
			return
		}
		if err := protection.Check(r); err != nil {
			writeError(w, http.StatusForbidden, err.Error())
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		r = r.WithContext(ctx)
		if r.URL.Path == "/healthz" || r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		device.ServeHTTP(w, r)
	}), nil
}

func respond(w http.ResponseWriter, result any, err error) {
	if err != nil {
		writeError(w, errorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func errorStatus(err error) int {
	var input inputError
	switch {
	case errors.As(err, &input), errors.Is(err, protocol.ErrInvalidURI), errors.Is(err, client.ErrInvalidValue):
		return http.StatusBadRequest
	case errors.Is(err, client.ErrUpdateBlocked):
		return http.StatusForbidden
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	default:
		return http.StatusBadGateway
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	// Device text passes through as sent rather than with \u-escaped <, > and &.
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
}

// trustedHosts keys are host+"\x00"+port; an empty port permits any port.
type trustedHosts map[string]bool

// allow is the DNS-rebinding boundary: a hostile name resolving to this
// listener still arrives with its own Host.
func (hosts trustedHosts) allow(r *http.Request) bool {
	host, port, ok := parseAuthority(r.Host)
	if !ok {
		return false
	}
	if host == "localhost" || hosts[host+"\x00"] || hosts[host+"\x00"+port] {
		return true
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	if address.IsLoopback() {
		return true
	}
	if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		localHost, _, valid := parseAuthority(local.String())
		return valid && host == localHost
	}
	return false
}

func parseAuthority(authority string) (host, port string, ok bool) {
	if authority == "" || strings.ContainsAny(authority, "/\\@?# \t\r\n") {
		return "", "", false
	}
	host = authority
	if strings.Contains(authority, ":") {
		var err error
		host, port, err = net.SplitHostPort(authority)
		if err != nil {
			if strings.HasPrefix(authority, "[") && strings.HasSuffix(authority, "]") {
				host = authority[1 : len(authority)-1]
			} else {
				return "", "", false
			}
		} else {
			number, err := strconv.ParseUint(port, 10, 16)
			if err != nil {
				return "", "", false
			}
			port = strconv.FormatUint(number, 10)
		}
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if address, err := netip.ParseAddr(host); err == nil {
		address = address.Unmap()
		// Zones are interface-local, and browsers reach the unspecified address
		// as loopback, so neither names this listener.
		if address.Zone() != "" || address.IsUnspecified() {
			return "", "", false
		}
		return address.String(), port, true
	}
	if host == "" || len(host) > 253 {
		return "", "", false
	}
	for _, c := range host {
		valid := c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_'
		if !valid {
			return "", "", false
		}
	}
	return host, port, true
}
