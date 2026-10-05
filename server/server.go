package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/kradalby/nefit-go/client"
)

type Mode = client.ServerMode

const (
	Offline = client.ModeOffline
	Both    = client.ModeBoth
)

type UpdatePolicy = client.UpdatePolicy

const (
	AllowUpdates = client.UpdatesAllow
	BlockUpdates = client.UpdatesBlock
)

var ErrClosed = errors.New("server closed")

// Config embeds the same device credentials and API used by cloud clients.
// UpstreamAddress may specify a resolved cloud IP to bypass a DNS override.
type Config struct {
	Device            client.Config
	Mode              Mode
	ListenAddress     string
	DeviceIP          net.IP
	UpstreamAddress   string
	UpdatePolicy      UpdatePolicy
	UpdateServices    []string
	RequestTimeout    time.Duration
	ReconnectInterval time.Duration
	Service           client.ServiceHandler
	DNS               *DNSConfig
}

// Server owns its listener and device session. Its embedded Client exposes the
// same Get/Put, typed commands, subscriptions and lifecycle as a cloud client.
// New binds the listener; Run maintains connectivity until cancellation. Embedded
// applications may instead call Connect and watch Done using their existing loop.
type Server struct {
	*client.Client
	mode      Mode
	dns       *DNS
	closed    chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func New(cfg Config) (*Server, error) {
	c, err := client.NewLocalClient(cfg.Device, client.LocalOptions{ListenAddress: cfg.ListenAddress, DeviceIP: cfg.DeviceIP, Mode: cfg.Mode, UpstreamAddress: cfg.UpstreamAddress, UpdatePolicy: cfg.UpdatePolicy, UpdateServices: append([]string(nil), cfg.UpdateServices...), RequestTimeout: cfg.RequestTimeout, ReconnectInterval: cfg.ReconnectInterval, Service: cfg.Service})
	if err != nil {
		return nil, err
	}
	mode := cfg.Mode
	if mode == "" {
		mode = Offline
	}
	s := &Server{Client: c, mode: mode, closed: make(chan struct{})}
	if cfg.DNS != nil {
		s.dns, err = NewDNS(*cfg.DNS)
		if err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return s, nil
}
func (s *Server) Mode() Mode { return s.mode }
func (s *Server) Run(ctx context.Context) error {
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	defer func() { _ = s.Close() }()
	for {
		if err := s.stopped(ctx); err != nil {
			return err
		}
		if err := s.Connect(ctx); err != nil {
			select {
			case <-ctx.Done():
			case <-s.closed:
			case <-time.After(time.Second):
			}
			continue
		}
		select {
		case <-ctx.Done():
		case <-s.closed:
		case <-s.Done():
		}
	}
}

func (s *Server) stopped(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("server stopped: %w", err)
	}
	select {
	case <-s.closed:
		return ErrClosed
	default:
		return nil
	}
}

func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.dns != nil {
			_ = s.dns.Close()
		}
		s.closeErr = s.Client.Close()
	})
	return s.closeErr
}
