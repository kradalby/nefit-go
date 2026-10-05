package server

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// DNSConfig optionally supplies a device-scoped DNS takeover endpoint. Configure
// the thermostat/DHCP resolver to use it, or prefer a scoped router DNS record.
// Other names are refused unless ForwardAddress is set. It is not a LAN resolver.
type DNSConfig struct {
	ListenAddress string
	Hostname      string
	Address       netip.Addr
	DeviceIP      netip.Addr
	// AllowedPeers permits a known DNS proxy in addition to the thermostat.
	AllowedPeers   []netip.Addr
	ForwardAddress string
	XMPPPort       uint16
}
type DNS struct {
	cfg    DNSConfig
	udp    net.PacketConn
	tcp    net.Listener
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	once   sync.Once
	slots  chan struct{}
}

func NewDNS(cfg DNSConfig) (*DNS, error) {
	cfg.AllowedPeers = append([]netip.Addr(nil), cfg.AllowedPeers...)
	if !cfg.Address.IsValid() || !cfg.DeviceIP.IsValid() {
		return nil, fmt.Errorf("DNS address and device IP are required")
	}
	if cfg.Hostname == "" {
		return nil, fmt.Errorf("DNS hostname is required")
	}
	cfg.Hostname = strings.TrimSuffix(strings.ToLower(cfg.Hostname), ".") + "."
	if _, err := dnsmessage.NewName(cfg.Hostname); err != nil {
		return nil, err
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1:53"
	}
	if cfg.XMPPPort == 0 {
		cfg.XMPPPort = 5222
	}
	udp, err := net.ListenPacket("udp", cfg.ListenAddress)
	if err != nil {
		return nil, err
	}
	tcp, err := net.Listen("tcp", udp.LocalAddr().String())
	if err != nil {
		_ = udp.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &DNS{cfg: cfg, udp: udp, tcp: tcp, ctx: ctx, cancel: cancel, slots: make(chan struct{}, 16)}
	s.wg.Go(s.serveUDP)
	s.wg.Go(s.serveTCP)
	return s, nil
}
func (s *DNS) Address() net.Addr { return s.tcp.Addr() }
func (s *DNS) Close() error {
	s.once.Do(func() { s.cancel(); _ = s.udp.Close(); _ = s.tcp.Close() })
	s.wg.Wait()
	return nil
}

func (s *DNS) allowed(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	if ip == s.cfg.DeviceIP.Unmap() {
		return true
	}
	for _, a := range s.cfg.AllowedPeers {
		if ip == a.Unmap() {
			return true
		}
	}
	return false
}

func (s *DNS) serveUDP() {
	buf := make([]byte, 4096)
	for {
		n, addr, err := s.udp.ReadFrom(buf)
		if err != nil {
			return
		}
		if !s.allowed(addr) {
			continue
		}
		raw := append([]byte(nil), buf[:n]...)
		select {
		case s.slots <- struct{}{}:
			s.wg.Go(func() {
				defer func() { <-s.slots }()
				reply, err := s.answer(raw)
				if err == nil {
					_, _ = s.udp.WriteTo(reply, addr)
				}
			})
		default:
		}
	}
}

func (s *DNS) serveTCP() {
	for {
		conn, err := s.tcp.Accept()
		if err != nil {
			return
		}
		if !s.allowed(conn.RemoteAddr()) {
			_ = conn.Close()
			continue
		}
		select {
		case s.slots <- struct{}{}:
			s.wg.Go(func() {
				defer func() { <-s.slots; _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				stop := context.AfterFunc(s.ctx, func() { _ = conn.Close() })
				defer stop()
				var length [2]byte
				if _, err := io.ReadFull(conn, length[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint16(length[:]))
				if n > 4096 {
					return
				}
				raw := make([]byte, n)
				if _, err := io.ReadFull(conn, raw); err != nil {
					return
				}
				reply, err := s.answer(raw)
				if err != nil {
					return
				}
				binary.BigEndian.PutUint16(length[:], uint16(len(reply)))
				_, _ = conn.Write(length[:])
				_, _ = conn.Write(reply)
			})
		default:
			_ = conn.Close()
		}
	}
}

func (s *DNS) answer(raw []byte) ([]byte, error) {
	var query dnsmessage.Message
	if err := query.Unpack(raw); err != nil {
		return nil, err
	}
	response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionDesired: query.RecursionDesired}, Questions: query.Questions}
	if query.Response || query.OpCode != 0 || len(query.Questions) != 1 {
		response.RCode = dnsmessage.RCodeFormatError
		return response.Pack()
	}
	q := query.Questions[0]
	name := strings.ToLower(q.Name.String())
	srvName := "_xmpp-client._tcp." + s.cfg.Hostname
	if q.Class != dnsmessage.ClassINET || name != s.cfg.Hostname && name != srvName {
		if s.cfg.ForwardAddress != "" {
			ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
			defer cancel()
			var d net.Dialer
			conn, err := d.DialContext(ctx, "udp", s.cfg.ForwardAddress)
			if err == nil {
				defer conn.Close() //nolint:errcheck
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				if _, err = conn.Write(raw); err == nil {
					buf := make([]byte, 4096)
					n, err := conn.Read(buf)
					if err == nil {
						var result dnsmessage.Message
						if err = result.Unpack(buf[:n]); err == nil && result.Response && result.ID == query.ID && len(result.Questions) == 1 && result.Questions[0] == q {
							return buf[:n], nil
						}
					}
				}
			}
			response.RCode = dnsmessage.RCodeServerFailure
		} else {
			response.RCode = dnsmessage.RCodeRefused
		}
		return response.Pack()
	}
	response.Authoritative = true
	header := dnsmessage.ResourceHeader{Name: q.Name, Class: q.Class, Type: q.Type, TTL: 30}
	switch {
	case name == s.cfg.Hostname && q.Type == dnsmessage.TypeA && s.cfg.Address.Is4():
		response.Answers = []dnsmessage.Resource{{Header: header, Body: &dnsmessage.AResource{A: s.cfg.Address.As4()}}}
	case name == s.cfg.Hostname && q.Type == dnsmessage.TypeAAAA && s.cfg.Address.Is6():
		response.Answers = []dnsmessage.Resource{{Header: header, Body: &dnsmessage.AAAAResource{AAAA: s.cfg.Address.As16()}}}
	case name == srvName && q.Type == dnsmessage.TypeSRV:
		target, _ := dnsmessage.NewName(s.cfg.Hostname)
		response.Answers = []dnsmessage.Resource{{Header: header, Body: &dnsmessage.SRVResource{Port: s.cfg.XMPPPort, Target: target}}}
	}
	return response.Pack()
}
