// Package discovery finds LocalSend peers on the local network via UDP
// multicast and makes this device discoverable in return.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/zebroc/localsend-cli/internal/httpx"
	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/netif"
	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/ui"
)

// announceDelays reproduce the reference implementation: the same announcement
// goes out three times so a single lost datagram does not hide us.
var announceDelays = []time.Duration{100 * time.Millisecond, 500 * time.Millisecond, 2 * time.Second}

// Config describes how this device presents itself during discovery.
type Config struct {
	Identity *identity.Identity

	// Port is the TCP port of our HTTP API. Peers answer announcements by
	// POSTing to this port, so it must be the port we actually serve on.
	Port int

	// Protocol is "https" or "http", matching our server.
	Protocol string

	// Group is the multicast group, MulticastPort the UDP port.
	Group         string
	MulticastPort int
}

func (c Config) group() string {
	if c.Group == "" {
		return proto.DefaultMulticastGroup
	}
	return c.Group
}

func (c Config) multicastPort() int {
	if c.MulticastPort == 0 {
		return proto.DefaultPort
	}
	return c.MulticastPort
}

// Register is how we describe ourselves in POST /register bodies.
func (c Config) Register() proto.Register {
	return proto.Register{
		Alias:       c.Identity.Alias,
		Version:     proto.Version,
		DeviceModel: deviceModel(),
		DeviceType:  proto.DeviceTypeHeadless,
		Fingerprint: c.Identity.Fingerprint,
		Port:        c.Port,
		Protocol:    c.Protocol,
		Download:    false,
	}
}

func (c Config) message(announce bool) proto.Multicast {
	return proto.Multicast{
		Alias:       c.Identity.Alias,
		Version:     proto.Version,
		DeviceModel: deviceModel(),
		DeviceType:  proto.DeviceTypeHeadless,
		Fingerprint: c.Identity.Fingerprint,
		Port:        c.Port,
		Protocol:    c.Protocol,
		Download:    false,
		// Both keys carry the same value: v1 peers read "announcement",
		// v2 peers read "announce".
		Announce:     announce,
		Announcement: announce,
	}
}

// isSelf reports whether a message came from this process. Comparing the port
// as well as the fingerprint keeps a daemon and a sender that share the
// configuration directory discoverable to each other.
func (c Config) isSelf(m proto.Multicast) bool {
	return m.Fingerprint == c.Identity.Fingerprint && m.Port == c.Port
}

// maxConcurrentAnswers bounds the goroutines spent on answering announcements,
// so a flood of datagrams cannot pile up work indefinitely.
const maxConcurrentAnswers = 16

// Listener receives announcements on the multicast port.
type Listener struct {
	cfg      Config
	conn     *net.UDPConn
	packet   *ipv4.PacketConn
	onDevice func(proto.Device)
	respond  bool
	answers  chan struct{}
}

// Listen binds the multicast port and joins the group on every usable
// interface.
//
// When respond is true, announcements are answered so the peer learns about us
// — that is what makes a running daemon show up in the app.
func Listen(cfg Config, port int, respond bool, onDevice func(proto.Device)) (*Listener, error) {
	conn, err := bindMulticast(cfg.group(), port)
	if err != nil {
		return nil, err
	}

	packet := ipv4.NewPacketConn(conn)
	group := &net.UDPAddr{IP: net.ParseIP(cfg.group())}

	interfaces, err := netif.Multicast()
	if err != nil {
		conn.Close()
		return nil, err
	}
	joined := 0
	for _, ifi := range interfaces {
		if err := packet.JoinGroup(&ifi, group); err != nil {
			ui.Debugf("join %s on %s: %v", cfg.group(), ifi.Name, err)
			continue
		}
		joined++
		ui.Debugf("listening for announcements on %s (%s:%d)", ifi.Name, cfg.group(), port)
	}
	if joined == 0 {
		conn.Close()
		return nil, fmt.Errorf("could not join %s on any interface", cfg.group())
	}

	return &Listener{
		cfg:      cfg,
		conn:     conn,
		packet:   packet,
		onDevice: onDevice,
		respond:  respond,
		answers:  make(chan struct{}, maxConcurrentAnswers),
	}, nil
}

// bindMulticast opens the socket that receives announcements.
//
// It binds the multicast group address first. That is what allows us to share
// the port with the official LocalSend app: the app only sets SO_REUSEADDR, and
// on BSD/macOS two sockets may then share a port only if they bind different
// addresses. Binding the group address also limits us to the group's traffic,
// which is all the protocol sends over UDP. The wildcard is the fallback for
// platforms that reject a multicast bind address.
func bindMulticast(group string, port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: netif.ReusePort}

	addresses := []string{
		net.JoinHostPort(group, strconv.Itoa(port)),
		":" + strconv.Itoa(port),
	}
	var firstErr error
	for _, address := range addresses {
		pc, err := lc.ListenPacket(context.Background(), "udp4", address)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			ui.Debugf("bind %s: %v", address, err)
			continue
		}
		conn, ok := pc.(*net.UDPConn)
		if !ok {
			pc.Close()
			return nil, fmt.Errorf("unexpected connection type %T", pc)
		}
		ui.Debugf("bound multicast socket to %s", address)
		return conn, nil
	}
	return nil, fmt.Errorf("bind UDP port %d: %w", port, firstErr)
}

// Run reads announcements until ctx is cancelled.
func (l *Listener) Run(ctx context.Context) {
	go func() {
		<-ctx.Done()
		l.conn.Close()
	}()

	buf := make([]byte, 8192)
	for {
		n, src, err := l.conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			ui.Debugf("multicast read: %v", err)
			continue
		}

		var message proto.Multicast
		if err := json.Unmarshal(buf[:n], &message); err != nil {
			ui.Debugf("ignoring malformed announcement from %s: %v", src.IP, err)
			continue
		}
		if message.Alias == "" || l.cfg.isSelf(message) {
			continue
		}

		device := proto.DeviceFromMulticast(message, src.IP.String())
		ui.Debugf("discovered %s via multicast", device)
		if l.onDevice != nil {
			l.onDevice(device)
		}

		if l.respond && message.IsAnnouncement() {
			select {
			case l.answers <- struct{}{}:
				go func() {
					defer func() { <-l.answers }()
					l.answer(ctx, device)
				}()
			default:
				ui.Debugf("too many announcements at once, not answering %s", device)
			}
		}
	}
}

// Close releases the socket.
func (l *Listener) Close() error { return l.conn.Close() }

// answer replies to an announcement, preferring TCP as the protocol suggests
// and falling back to a multicast response.
func (l *Listener) answer(ctx context.Context, device proto.Device) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	client := httpx.New(httpx.Options{Identity: l.cfg.Identity, Timeout: 3 * time.Second})
	err := httpx.PostJSON(ctx, client, device.BaseURL()+"/register", l.cfg.Register(), nil)
	if err == nil {
		ui.Debugf("answered %s via TCP", device)
		return
	}

	ui.Debugf("answering %s via TCP failed (%v), falling back to multicast", device, err)
	if err := l.sendMessage(l.cfg.message(false)); err != nil {
		ui.Debugf("multicast answer failed: %v", err)
	}
}

// sendMessage writes one datagram to the group on every usable interface.
func (l *Listener) sendMessage(message proto.Multicast) error {
	return sendMulticast(l.cfg, message)
}

// Announce makes this device known to the network: it sends the announcement
// three times, spaced like the reference implementation, and returns once the
// last one is out.
//
// Peers answer with POST /register to Config.Port, so an HTTP server must be
// running there.
func Announce(ctx context.Context, cfg Config) {
	message := cfg.message(true)
	for _, delay := range announceDelays {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if err := sendMulticast(cfg, message); err != nil {
			ui.Debugf("announce: %v", err)
		}
	}
}

func sendMulticast(cfg Config, message proto.Multicast) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	target := &net.UDPAddr{IP: net.ParseIP(cfg.group()), Port: cfg.multicastPort()}

	interfaces, err := netif.Multicast()
	if err != nil {
		return err
	}

	sent := 0
	for _, ifi := range interfaces {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: 0})
		if err != nil {
			ui.Debugf("announce socket for %s: %v", ifi.Name, err)
			continue
		}
		packet := ipv4.NewPacketConn(conn)
		if err := packet.SetMulticastInterface(&ifi); err != nil {
			ui.Debugf("select interface %s for announcement: %v", ifi.Name, err)
			conn.Close()
			continue
		}
		// Loopback lets a daemon on this host see the sender's announcement.
		_ = packet.SetMulticastLoopback(true)
		_ = packet.SetMulticastTTL(1)

		if _, err := conn.WriteToUDP(payload, target); err != nil {
			ui.Debugf("announce on %s: %v", ifi.Name, err)
		} else {
			sent++
		}
		conn.Close()
	}

	if sent == 0 {
		return fmt.Errorf("no interface accepted the announcement")
	}
	return nil
}

// deviceModel is shown next to the alias in the app.
func deviceModel() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	case "windows":
		return "Windows"
	default:
		return runtime.GOOS
	}
}
