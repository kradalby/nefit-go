package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kradalby/nefit-go/client"
)

func TestServeOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"defaults", nil, ""},
		{"missing device", []string{"--device-ip="}, "--device-ip"},
		{"invalid device", []string{"--device-ip=invalid"}, "--device-ip"},
		{"upstream both", []string{"--mode=both", "--upstream=cloud.example:5222"}, ""},
		{"update services blocked", []string{"--updates=block", "--update-services=gservice_extra"}, ""},
		{"unused DNS address", []string{"--dns-address=192.0.2.1"}, "require --dns-listen"},
		{"unused DNS forward", []string{"--dns-forward=192.0.2.53:53"}, "require --dns-listen"},
		{"missing DNS address", []string{"--dns-listen=192.0.2.1:53"}, "--dns-address"},
		{"invalid DNS address", []string{"--dns-listen=192.0.2.1:53", "--dns-address=invalid"}, "--dns-address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("serve", flag.ContinueOnError)
			options := newServeOptions(fs)
			if err := fs.Parse(append([]string{"--device-ip=192.0.2.10"}, tc.args...)); err != nil {
				t.Fatal(err)
			}
			_, err := options.config()
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServeConfig(t *testing.T) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	options := newServeOptions(fs)
	if err := fs.Parse([]string{
		"--device-ip=192.0.2.10", "--mode=both", "--updates=block", "--update-services= a, ,b ",
		"--xmpp-listen=192.0.2.1:5222", "--dns-listen=192.0.2.1:53", "--dns-address=192.0.2.1", "--dns-forward=192.0.2.53:53",
		"--upstream=cloud.example:5222", "--domain=device.example",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := options.config()
	if err != nil {
		t.Fatal(err)
	}
	dns := cfg.DNS
	if cfg.Mode != client.ModeBoth || cfg.UpdatePolicy != client.UpdatesBlock || strings.Join(cfg.UpdateServices, ",") != "a,b" ||
		cfg.UpstreamAddress != "cloud.example:5222" || cfg.ListenAddress != "192.0.2.1:5222" || cfg.Device.Host != "device.example" ||
		dns == nil || dns.ListenAddress != "192.0.2.1:53" || dns.Address != netip.MustParseAddr("192.0.2.1") ||
		dns.ForwardAddress != "192.0.2.53:53" {
		t.Fatalf("config %+v DNS %+v", cfg, dns)
	}
	d := cfg.Device
	if d.SerialNumber != *serialNumber || d.AccessKey != *accessKey || d.Password != *password ||
		d.RetryTimeout != *timeout || cfg.RequestTimeout != *timeout || d.ConnectTimeout <= 0 || cfg.DeviceIP != netip.MustParseAddr("192.0.2.10") {
		t.Fatalf("device config %+v, options %+v", d, cfg.LocalOptions)
	}
}

func TestServeHTTPServer(t *testing.T) {
	c, err := client.NewClient(client.Config{SerialNumber: "123", AccessKey: "synthetic-key", Password: "synthetic-password"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLogger(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = c.Close() })
	if _, err := newServeHTTPServer(c, []string{"thermostat.example", "bad host"}, time.Second); err == nil || !strings.Contains(err.Error(), "--http-allowed-hosts") {
		t.Fatalf("invalid allowed host accepted: %v", err)
	}
	s, err := newServeHTTPServer(c, splitList(" thermostat.example, proxy.example:8443, , "), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.ReadTimeout != time.Second || s.WriteTimeout != time.Second+httpGracePeriod || s.MaxHeaderBytes != 16<<10 {
		t.Fatalf("timeouts read %v write %v", s.ReadTimeout, s.WriteTimeout)
	}
	local := &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 8088}
	for host, allowed := range map[string]bool{
		"localhost:8088": true, local.String(): true, "thermostat.example:8088": true, "proxy.example:8443": true,
		"proxy.example:443": false, "attacker.example:8088": false, "0.0.0.0:8088": false,
	} {
		t.Run(host, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			r.Host = host
			r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, net.Addr(local)))
			w := httptest.NewRecorder()
			s.Handler.ServeHTTP(w, r)
			want := http.StatusForbidden
			if allowed {
				want = http.StatusServiceUnavailable
			}
			if w.Code != want {
				t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
			}
		})
	}
}

func TestServeSecondSignalExits(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := cliCommand(ctx, "--timeout=20s", "serve", "--device-ip=127.0.0.1", "--xmpp-listen=127.0.0.1:0", "--http-listen=127.0.0.1:0")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	var address string
	scanner := bufio.NewScanner(stderr)
	for address == "" && scanner.Scan() {
		if _, rest, ok := strings.Cut(scanner.Text(), " http="); ok {
			address, _, _ = strings.Cut(rest, " ")
		}
	}
	go func() { _, _ = io.Copy(io.Discard, stderr); exited <- cmd.Wait() }()
	if address == "" {
		t.Fatal("serve did not report its HTTP address")
	}
	// The first answer proves the connection is served; the second request
	// then waits on the absent device and holds the drain open.
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close() //nolint:errcheck
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: %s\r\n\r\n", address); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(conn), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(conn, "GET /probe HTTP/1.1\r\nHost: %s\r\n\r\n", address); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-exited:
		t.Fatalf("serve exited before draining: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !status.Signaled() {
			t.Fatalf("second signal did not end the process: %v", cmd.ProcessState)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second signal ignored during drain")
	}
}

type fakeDeviceRunner struct {
	started chan context.Context
	failure chan error
	stopped chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func newFakeDeviceRunner() *fakeDeviceRunner {
	return &fakeDeviceRunner{started: make(chan context.Context, 1), failure: make(chan error, 1), stopped: make(chan struct{}), closed: make(chan struct{})}
}

func (c *fakeDeviceRunner) Run(ctx context.Context) error {
	c.started <- ctx
	defer close(c.stopped)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return nil
	case err := <-c.failure:
		return err
	}
}

func (c *fakeDeviceRunner) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func receiveWithin[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for server lifecycle")
		var zero T
		return zero
	}
}

func TestHTTPShutdownDrainsBeforeClosingDevice(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := newFakeDeviceRunner()
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-release:
			fmt.Fprint(w, "completed") //nolint:errcheck
		case <-r.Context().Done():
		}
	})}
	shutdownStarted := make(chan struct{})
	s.RegisterOnShutdown(func() { close(shutdownStarted) })
	t.Cleanup(func() { _ = s.Close(); _ = listener.Close(); _ = c.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runHTTPServer(ctx, c, s, listener, time.Second) }()
	runCtx := receiveWithin(t, c.started)
	response := make(chan error, 1)
	go func() {
		client := &http.Client{Timeout: 2 * time.Second}
		r, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			defer r.Body.Close() //nolint:errcheck
			var body []byte
			body, err = io.ReadAll(r.Body)
			if err == nil && (r.StatusCode != http.StatusOK || string(body) != "completed") {
				err = fmt.Errorf("response %d %q", r.StatusCode, body)
			}
		}
		response <- err
	}()
	receiveWithin(t, entered)
	cancel()
	receiveWithin(t, shutdownStarted)
	if runCtx.Err() != nil {
		t.Fatal("device context cancelled before HTTP drain")
	}
	select {
	case <-c.closed:
		t.Fatal("device closed before HTTP drain")
	case err := <-result:
		t.Fatalf("serve returned before HTTP drain: %v", err)
	default:
	}
	unblock()
	if err := receiveWithin(t, response); err != nil {
		t.Fatalf("in-flight response aborted: %v", err)
	}
	if err := receiveWithin(t, result); err != nil {
		t.Fatal(err)
	}
	receiveWithin(t, c.closed)
	receiveWithin(t, c.stopped)
	if runCtx.Err() != context.Canceled {
		t.Fatalf("device lifetime did not end: %v", runCtx.Err())
	}
}

func TestHTTPShutdownTimeoutForcesClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := newFakeDeviceRunner()
	entered := make(chan struct{})
	requestCancelled := make(chan struct{})
	s := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(requestCancelled)
	})}
	t.Cleanup(func() { _ = s.Close(); _ = listener.Close(); _ = c.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- runHTTPServer(ctx, c, s, listener, 20*time.Millisecond) }()
	response := make(chan error, 1)
	go func() {
		// No client timeout: only the forced close may end this request.
		r, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			_ = r.Body.Close()
		}
		response <- err
	}()
	receiveWithin(t, entered)
	cancel()
	// Shutdown waits 20ms; reaching the 500ms deadline means nothing forced it.
	deadline := time.After(500 * time.Millisecond)
	for _, done := range []<-chan struct{}{requestCancelled, c.closed, c.stopped} {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("stalled request was not cancelled promptly")
		}
	}
	if err := receiveWithin(t, result); err != nil {
		t.Fatalf("cancellation changed serve return behavior: %v", err)
	}
	if err := receiveWithin(t, response); err == nil {
		t.Fatal("stalled request unexpectedly succeeded")
	}
}

type failingHTTPListener struct{ err error }

func (l failingHTTPListener) Accept() (net.Conn, error) { return nil, l.err }
func (l failingHTTPListener) Close() error              { return nil }
func (l failingHTTPListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func TestHTTPServeErrorClosesDevice(t *testing.T) {
	c := newFakeDeviceRunner()
	want := errors.New("synthetic accept failure")
	err := runHTTPServer(t.Context(), c, &http.Server{}, failingHTTPListener{err: want}, time.Second)
	if !errors.Is(err, want) {
		t.Fatalf("serve error %v, want %v", err, want)
	}
	receiveWithin(t, c.closed)
	receiveWithin(t, c.stopped)
}

func TestHTTPServeAlreadyCancelled(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close() //nolint:errcheck
	c := newFakeDeviceRunner()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := make(chan error, 1)
	go func() { result <- runHTTPServer(ctx, c, &http.Server{}, listener, time.Second) }()
	if err := receiveWithin(t, result); err != nil {
		t.Fatal(err)
	}
	receiveWithin(t, c.closed)
	receiveWithin(t, c.stopped)
}

func TestDeviceRunnerFailureStopsHTTP(t *testing.T) {
	for _, failure := range []error{client.ErrListenerFailed, nil} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			runner := newFakeDeviceRunner()
			entered := make(chan struct{})
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				close(entered)
				w.WriteHeader(http.StatusOK)
			})}
			t.Cleanup(func() { _ = server.Close(); _ = listener.Close(); _ = runner.Close() })
			result := make(chan error, 1)
			go func() { result <- runHTTPServer(t.Context(), runner, server, listener, time.Second) }()
			response, err := (&http.Client{Timeout: time.Second}).Get("http://" + listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			receiveWithin(t, entered)
			runner.failure <- failure
			err = receiveWithin(t, result)
			if err == nil || failure != nil && !errors.Is(err, failure) {
				t.Fatalf("runner failure lost: %v", err)
			}
			receiveWithin(t, runner.closed)
			// Accepting, not dialling: a parallel test may reuse the port.
			_ = listener.(*net.TCPListener).SetDeadline(time.Now())
			if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
				t.Fatal("HTTP listener survived fatal device failure:", err)
			}
		})
	}
}
