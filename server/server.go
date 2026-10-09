package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/kradalby/nefit-go/client"
)

// Config holds the device credentials in Device, as for a cloud client, and
// embeds LocalOptions, which documents the listener defaults. DNS, when set,
// also starts the DNS endpoint.
type Config struct {
	Device client.Config
	client.LocalOptions
	DNS *DNSConfig
}

// Server owns its listener and device session. Its embedded Client exposes the
// same Get/Put, typed commands, subscriptions and lifecycle as a cloud client.
// New binds the listener, and devices are admitted from then until Close,
// whether or not anyone calls Run or Connect. Run reports a failed listener
// and closes the server when it returns. Call Close and SetLogger on the
// Server rather than its Client: they also cover the DNS endpoint. Like
// Client, it is safe for concurrent use.
type Server struct {
	*client.Client
	dns *dnsEndpoint
}

// New binds the device listener and, when configured, the DNS endpoint. The
// endpoint answers the device IP's queries for its XMPP host, with the
// listener's port unless DNS.XMPPPort is set; its Address must be one the
// device reaches the listener on.
func New(cfg Config) (*Server, error) {
	c, err := client.NewLocalClient(cfg.Device, cfg.LocalOptions)
	if err != nil {
		return nil, err
	}
	s := &Server{Client: c}
	if cfg.DNS != nil {
		dnsConfig := *cfg.DNS
		dnsConfig.hostname = cfg.Device.WithDefaults().Host
		dnsConfig.deviceIP = cfg.DeviceIP
		listener := c.LocalAddress().(*net.TCPAddr)
		if dnsConfig.XMPPPort == 0 {
			dnsConfig.XMPPPort = uint16(listener.Port)
		}
		// The device follows the answer, so the XMPP listener must accept there.
		bound, _ := netip.AddrFromSlice(listener.IP)
		// A loopback answer sends the device to itself, unless it is local.
		if address := dnsConfig.Address.Unmap(); address.IsUnspecified() || address.IsLoopback() && !dnsConfig.deviceIP.Unmap().IsLoopback() ||
			!bound.IsUnspecified() && bound.Unmap() != address.WithZone("") {
			_ = c.Close()
			return nil, fmt.Errorf("DNS address %s is not an address the device reaches the XMPP listener %s on", dnsConfig.Address, listener)
		}
		if s.dns, err = openDNS(dnsConfig); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return s, nil
}

// SetLogger sets the logger of the device session and the DNS endpoint; nil
// restores slog.Default().
func (s *Server) SetLogger(logger *slog.Logger) {
	s.Client.SetLogger(logger)
	if s.dns != nil {
		s.dns.logger.Store(logger)
	}
}

// Run blocks until ctx ends, the server or its client is closed
// (client.ErrClosed), or the listener fails (client.ErrListenerFailed), and
// closes the server when it returns. An application draining HTTP on the
// same signal should give Run a context it cancels after the drain.
func (s *Server) Run(ctx context.Context) error {
	defer func() { _ = s.Close() }()
	for {
		start := time.Now()
		if err := s.Connect(ctx); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("server stopped: %w", ctx.Err())
			}
			// Neither recovers: a failed listener stays failed, and a client
			// closed directly (for example through the embedded field) is done.
			if errors.Is(err, client.ErrListenerFailed) || errors.Is(err, client.ErrClosed) {
				return err
			}
			// Waiting for a device dials nothing; the floor only keeps a tiny
			// ConnectTimeout from spinning.
			select {
			case <-ctx.Done():
			case <-time.After(time.Second - time.Since(start)):
			}
			continue
		}
		select {
		case <-ctx.Done():
			// Connect would keep returning the live session until Close ends it.
			return fmt.Errorf("server stopped: %w", ctx.Err())
		case <-s.Done():
		}
	}
}

// Close releases the DNS endpoint, the listener and the session; it is safe
// to call more than once.
func (s *Server) Close() error {
	if s.dns != nil {
		_ = s.dns.Close()
	}
	return s.Client.Close()
}
