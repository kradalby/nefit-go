package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/peterbourgon/ff/v3/ffcli"

	"github.com/kradalby/nefit-go/client"
	"github.com/kradalby/nefit-go/server"
)

type serveOptions struct {
	xmpp, http, allowedHosts, device                string
	dnsListen, dnsAddress, dnsForward               string
	mode, upstream, updates, updateServices, domain string
}

func newServeOptions(fs *flag.FlagSet) *serveOptions {
	o := &serveOptions{}
	fs.StringVar(&o.xmpp, "xmpp-listen", "127.0.0.1:5222", "Device XMPP listener address")
	fs.StringVar(&o.http, "http-listen", "127.0.0.1:8088", "HTTP API listener address (use an authenticated proxy for remote access)")
	fs.StringVar(&o.allowedHosts, "http-allowed-hosts", "", "Additional trusted HTTP Host names or host:port entries, comma separated")
	fs.StringVar(&o.device, "device-ip", "", "Expected thermostat source IP (required)")
	fs.StringVar(&o.dnsListen, "dns-listen", "", "Optional device-scoped UDP/TCP DNS listener: specific IP:port, not a wildcard")
	fs.StringVar(&o.dnsAddress, "dns-address", "", "Local IP returned for original Bosch hostname; --xmpp-listen must accept it")
	fs.StringVar(&o.dnsForward, "dns-forward", "", "Optional resolver IP:port for unrelated DNS questions")
	fs.StringVar(&o.mode, "mode", "offline", "Device service mode: offline or both")
	fs.StringVar(&o.upstream, "upstream", "", "Bosch host:port or IP:port; bypass local DNS override (--mode both)")
	fs.StringVar(&o.updates, "updates", "allow", "Firmware update policy: allow or block (XMPP only)")
	fs.StringVar(&o.updateServices, "update-services", "", "Additional update service JIDs/localparts to block, comma separated (--updates block)")
	fs.StringVar(&o.domain, "domain", client.DefaultHost, "Original Bosch XMPP domain used by device")
	return o
}

var (
	serveFlags = flag.NewFlagSet("serve", flag.ExitOnError)
	serveOpts  = newServeOptions(serveFlags)
)

var serveCmd = &ffcli.Command{
	Name:       "serve",
	ShortUsage: "nefit [credentials] serve --device-ip IP [flags]",
	ShortHelp:  "Run the device XMPP server and HTTP API, offline or with cloud forwarding",
	LongHelp: `The HTTP API accepts localhost, loopback IPs and the local IP each connection arrived on.
Use --http-allowed-hosts for trusted DNS or reverse proxy names.

On interrupt or SIGTERM, HTTP requests drain for up to --timeout plus 5s
before the device connection closes. A second signal exits immediately.

--xmpp-listen defaults to loopback; bind an address the thermostat reaches.

Examples:
  nefit serve --device-ip 192.0.2.10 --xmpp-listen 192.0.2.1:5222
  nefit --timeout 15s serve --device-ip 192.0.2.10 --xmpp-listen 192.0.2.1:5222 --http-allowed-hosts thermostat.example`,
	FlagSet: serveFlags,
	Exec: func(ctx context.Context, args []string) error {
		if len(args) != 0 {
			return usageError("unexpected serve arguments")
		}
		if err := requireCredentials(); err != nil {
			return configError{err}
		}
		cfg, err := serveOpts.config()
		if err != nil {
			return configError{err}
		}
		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
		// Restore default handling so a second signal ends a stuck drain.
		context.AfterFunc(ctx, stop)
		c, err := server.New(cfg)
		if err != nil {
			return startupError(err)
		}
		defer c.Close() //nolint:errcheck
		if *verbose {
			c.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
		httpServer, err := newServeHTTPServer(c.Client, splitList(serveOpts.allowedHosts), *timeout)
		if err != nil {
			return configError{err}
		}
		httpListener, err := net.Listen("tcp", serveOpts.http)
		if err != nil {
			return startupError(err)
		}
		slog.Info("device server listening", "mode", cfg.Mode, "xmpp", c.LocalAddress(), "http", httpListener.Addr())
		return runHTTPServer(ctx, c, httpServer, httpListener, *timeout+httpGracePeriod)
	},
}

const httpGracePeriod = 5 * time.Second

// config rejects what server.New cannot see: flags given without the flag
// that uses them, and an empty --http-listen. server.New validates the rest.
func (o *serveOptions) config() (server.Config, error) {
	device, err := netip.ParseAddr(o.device)
	if err != nil {
		return server.Config{}, fmt.Errorf("--device-ip must be a valid IP address")
	}
	if o.http == "" {
		// net.Listen would open the unauthenticated API on a random port on
		// every interface; binding all interfaces needs saying so.
		return server.Config{}, fmt.Errorf("--http-listen must not be empty")
	}
	// A port name no restart can resolve is configuration, not a startup failure.
	for flag, address := range map[string]string{"--xmpp-listen": o.xmpp, "--http-listen": o.http} {
		_, port, err := net.SplitHostPort(address)
		if err == nil {
			_, err = net.LookupPort("tcp", port)
		}
		if err != nil {
			return server.Config{}, fmt.Errorf("%s: %w", flag, err)
		}
	}
	if o.dnsListen == "" && (o.dnsAddress != "" || o.dnsForward != "") {
		return server.Config{}, fmt.Errorf("--dns-address and --dns-forward require --dns-listen")
	}
	cfg := server.Config{
		Device: client.Config{
			SerialNumber: *serialNumber, AccessKey: *accessKey, Password: *password, Host: o.domain,
			ConnectTimeout: 90 * time.Second, RetryTimeout: *timeout,
		},
		LocalOptions: client.LocalOptions{
			ListenAddress: o.xmpp, DeviceIP: device, Mode: client.ServerMode(o.mode), UpstreamAddress: o.upstream,
			UpdatePolicy: client.UpdatePolicy(o.updates), UpdateServices: splitList(o.updateServices), RequestTimeout: *timeout,
		},
	}
	if o.dnsListen == "" {
		return cfg, nil
	}
	address, err := netip.ParseAddr(o.dnsAddress)
	if err != nil {
		return server.Config{}, fmt.Errorf("--dns-address must be the IP the device reaches the XMPP listener on, got %q", o.dnsAddress)
	}
	// server.New takes the hostname and device IP from the XMPP session.
	cfg.DNS = &server.DNSConfig{ListenAddress: o.dnsListen, Address: address, ForwardAddress: o.dnsForward}
	return cfg, nil
}

func newServeHTTPServer(c *client.Client, allowedHosts []string, requestTimeout time.Duration) (*http.Server, error) {
	handler, err := server.NewHandler(c, requestTimeout, allowedHosts...)
	if err != nil {
		return nil, fmt.Errorf("--http-allowed-hosts: %w", err)
	}
	return &http.Server{
		// Headers are buffered before any check runs; the API needs few.
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10, ReadTimeout: min(10*time.Second, requestTimeout),
		WriteTimeout: requestTimeout + httpGracePeriod, IdleTimeout: 60 * time.Second,
	}, nil
}

type deviceRunner interface {
	Run(context.Context) error
	Close() error
}

// runHTTPServer serves until ctx ends or either server fails, then drains
// HTTP before closing the device: Run closes XMPP on cancellation, so it gets
// its own context.
func runHTTPServer(ctx context.Context, c deviceRunner, httpServer *http.Server, listener net.Listener, shutdownTimeout time.Duration) error {
	runCtx, cancelRun := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRun()
	var runErr error
	ran := make(chan struct{})
	go func() { defer close(ran); runErr = c.Run(runCtx) }()
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()

	var err error
	select {
	case <-ctx.Done():
	case err = <-served:
	case <-ran:
		err = fmt.Errorf("device server stopped: %w", cmp.Or(runErr, errors.New("no error reported")))
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Warn("HTTP shutdown did not drain all requests", "error", err)
		_ = httpServer.Close()
	}
	cancelRun()
	_ = c.Close()
	<-ran
	return err
}

func splitList(value string) []string {
	var values []string
	for _, v := range strings.Split(value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}
	return values
}
