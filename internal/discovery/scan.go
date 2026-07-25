package discovery

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/server"
	"github.com/zebroc/localsend-cli/internal/ui"
)

// ScanConfig configures a discovery round.
type ScanConfig struct {
	Identity *identity.Identity

	// Group and MulticastPort default to the protocol defaults when empty.
	Group         string
	MulticastPort int

	// Timeout bounds the round.
	Timeout time.Duration

	// OnDevice is called for every device found, possibly more than once per
	// device and from several goroutines.
	OnDevice func(proto.Device)

	// StopWhen ends the round early when it returns true, e.g. as soon as the
	// device we want has answered.
	StopWhen func(proto.Device) bool
}

// Scan announces this device and collects the answers.
//
// Peers answer an announcement by calling POST /register on the port we
// announced, so a short-lived HTTP server is started for the duration of the
// round. As a fallback, answers sent to the multicast group are picked up too.
func Scan(ctx context.Context, sc ScanConfig) ([]proto.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, sc.Timeout)
	defer cancel()

	found := &collector{
		devices:  map[string]proto.Device{},
		onDevice: sc.OnDevice,
		stopWhen: sc.StopWhen,
		stop:     cancel,
	}

	// The callback server answers /register and /info while we wait.
	srv, err := server.New(server.Config{
		Identity: sc.Identity,
		Port:     0,
		UseTLS:   true,
		OnPeer:   found.add,
	})
	if err != nil {
		return nil, err
	}
	defer srv.Close()
	go func() {
		if err := srv.Serve(ctx); err != nil {
			ui.Debugf("discovery callback server: %v", err)
		}
	}()

	cfg := Config{
		Identity:      sc.Identity,
		Port:          srv.Port(),
		Protocol:      srv.Protocol(),
		Group:         sc.Group,
		MulticastPort: sc.MulticastPort,
	}

	// Answers may also arrive as a multicast message on the shared port. That
	// port is often taken by a local daemon or the official app; without
	// SO_REUSEPORT we simply do without this fallback.
	if listener, err := Listen(cfg, cfg.multicastPort(), false, found.add); err != nil {
		ui.Debugf("no multicast listener for this scan: %v", err)
	} else {
		defer listener.Close()
		go listener.Run(ctx)
	}

	ui.Debugf("announcing as %q on port %d", cfg.Identity.Alias, cfg.Port)
	go Announce(ctx, cfg)

	<-ctx.Done()
	return found.all(), nil
}

// collector deduplicates the devices arriving from several sources.
type collector struct {
	mu       sync.Mutex
	devices  map[string]proto.Device
	onDevice func(proto.Device)
	stopWhen func(proto.Device) bool
	stop     context.CancelFunc
}

func (c *collector) add(device proto.Device) {
	if device.Alias == "" || device.IP == "" {
		return
	}

	c.mu.Lock()
	key := device.Fingerprint
	if key == "" {
		key = device.IP
	}
	_, known := c.devices[key]
	c.devices[key] = device
	c.mu.Unlock()

	if c.onDevice != nil {
		c.onDevice(device)
	}
	if !known {
		ui.Debugf("found %s", device)
	}
	if c.stopWhen != nil && c.stopWhen(device) {
		c.stop()
	}
}

func (c *collector) all() []proto.Device {
	c.mu.Lock()
	defer c.mu.Unlock()

	devices := make([]proto.Device, 0, len(c.devices))
	for _, device := range c.devices {
		devices = append(devices, device)
	}
	sort.Slice(devices, func(i, j int) bool {
		if !strings.EqualFold(devices[i].Alias, devices[j].Alias) {
			return strings.ToLower(devices[i].Alias) < strings.ToLower(devices[j].Alias)
		}
		return devices[i].IP < devices[j].IP
	})
	return devices
}
