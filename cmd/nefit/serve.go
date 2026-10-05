package main

import (
	"context"
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

var (
	serveFlags          = flag.NewFlagSet("serve", flag.ExitOnError)
	serveXMPP           = serveFlags.String("xmpp-listen", "127.0.0.1:5222", "Device XMPP listener address")
	serveHTTP           = serveFlags.String("http-listen", "127.0.0.1:8088", "HTTP API listener address (use an authenticated proxy for remote access)")
	serveDevice         = serveFlags.String("device-ip", "", "Expected thermostat source IP (required)")
	serveDNSListen      = serveFlags.String("dns-listen", "", "Optional device-scoped UDP/TCP DNS listener (host:port)")
	serveDNSAddress     = serveFlags.String("dns-address", "", "Local IP returned for original Bosch hostname")
	serveDNSForward     = serveFlags.String("dns-forward", "", "Optional resolver host:port for unrelated DNS questions")
	serveMode           = serveFlags.String("mode", "offline", "Device service mode: offline or both")
	serveUpstream       = serveFlags.String("upstream", "", "Bosch host:port or IP:port; bypass local DNS override")
	serveUpdates        = serveFlags.String("updates", "allow", "Firmware update policy: allow or block (XMPP only)")
	serveUpdateServices = serveFlags.String("update-services", "", "Additional update service JIDs/localparts to block, comma separated")
	serveDomain         = serveFlags.String("domain", client.DefaultHost, "Original Bosch XMPP domain used by device")
)

var serveCmd = &ffcli.Command{
	Name:       "serve",
	ShortUsage: "nefit [credentials] serve --device-ip IP [flags]",
	ShortHelp:  "Run the device XMPP server and HTTP API, offline or with cloud forwarding",
	FlagSet:    serveFlags,
	Exec: func(ctx context.Context, args []string) error {
		if len(args) != 0 {
			return fmt.Errorf("unexpected serve arguments")
		}
		ip := net.ParseIP(*serveDevice)
		if ip == nil {
			return fmt.Errorf("--device-ip must be a valid IP address")
		}
		ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer cancel()
		var dnsConfig *server.DNSConfig
		if *serveDNSListen != "" {
			address, err := netip.ParseAddr(*serveDNSAddress)
			if err != nil {
				return fmt.Errorf("--dns-address must be a valid local IP: %w", err)
			}
			device, _ := netip.ParseAddr(*serveDevice)
			dnsConfig = &server.DNSConfig{ListenAddress: *serveDNSListen, Address: address, DeviceIP: device, Hostname: *serveDomain, ForwardAddress: *serveDNSForward}
		}
		c, err := server.New(server.Config{DNS: dnsConfig, Device: client.Config{
			SerialNumber: *serialNumber, AccessKey: *accessKey, Password: *password, Host: *serveDomain,
			ConnectTimeout: 90 * time.Second, RetryTimeout: *timeout,
		}, ListenAddress: *serveXMPP, DeviceIP: ip, Mode: server.Mode(*serveMode), UpstreamAddress: *serveUpstream, UpdatePolicy: server.UpdatePolicy(*serveUpdates), UpdateServices: splitServices(*serveUpdateServices), RequestTimeout: *timeout})
		if err != nil {
			return err
		}
		defer c.Close() //nolint:errcheck
		if *verbose {
			c.SetLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
		}
		httpListener, err := net.Listen("tcp", *serveHTTP)
		if err != nil {
			return err
		}
		httpServer := &http.Server{
			Handler: server.NewHandler(c.Client, *timeout), ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout: 10 * time.Second, WriteTimeout: *timeout + 5*time.Second, IdleTimeout: 60 * time.Second,
		}
		finished := make(chan struct{})
		go func() { defer close(finished); _ = c.Run(ctx) }()

		stop := context.AfterFunc(ctx, func() { _ = httpServer.Close() })
		defer stop()
		slog.Info("device server listening", "mode", c.Mode(), "xmpp", c.LocalAddress(), "http", httpListener.Addr())
		err = httpServer.Serve(httpListener)
		cancel()
		_ = c.Close()
		<-finished
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	},
}

func splitServices(value string) []string {
	var values []string
	for _, v := range strings.Split(value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			values = append(values, v)
		}
	}
	return values
}
