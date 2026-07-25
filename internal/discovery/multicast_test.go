package discovery

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/server"
)

// multicastEnv opts into the tests that need real multicast delivery. They are
// off by default because container and CI networks frequently drop it.
const multicastEnv = "LOCALSEND_CLI_MULTICAST_TESTS"

// v1 peers read "announcement", v2 peers read "announce"; the reference
// implementation writes both and so must we.
func TestMessageCarriesBothAnnounceKeys(t *testing.T) {
	cfg := Config{Identity: testIdentity(t, "Us"), Port: 53317, Protocol: proto.ProtocolHTTPS}

	encoded, err := json.Marshal(cfg.message(true))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"announce":true`, `"announcement":true`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("announcement %s is missing %s", encoded, want)
		}
	}

	answer, err := json.Marshal(cfg.message(false))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"announce":false`, `"announcement":false`} {
		if !strings.Contains(string(answer), want) {
			t.Errorf("answer %s is missing %s", answer, want)
		}
	}
}

func TestMessageDescribesUs(t *testing.T) {
	id := testIdentity(t, "Headless Box")
	cfg := Config{Identity: id, Port: 12345, Protocol: proto.ProtocolHTTP}

	message := cfg.message(true)
	if message.Alias != "Headless Box" {
		t.Errorf("alias = %q", message.Alias)
	}
	if message.Fingerprint != id.Fingerprint {
		t.Errorf("fingerprint = %q, want the certificate's", message.Fingerprint)
	}
	// The announced port is where peers send their answer, so it must be the
	// port we actually serve on, not the protocol default.
	if message.Port != 12345 {
		t.Errorf("port = %d, want 12345", message.Port)
	}
	if message.Protocol != proto.ProtocolHTTP {
		t.Errorf("protocol = %q", message.Protocol)
	}
	if message.DeviceType != proto.DeviceTypeHeadless {
		t.Errorf("deviceType = %q, want %q", message.DeviceType, proto.DeviceTypeHeadless)
	}
	if message.Version != proto.Version {
		t.Errorf("version = %q, want %q", message.Version, proto.Version)
	}
}

// Self-filtering compares fingerprint *and* port. A daemon and a sender that
// share the configuration directory have the same fingerprint but different
// ports, and they must still discover each other.
func TestIsSelf(t *testing.T) {
	id := testIdentity(t, "Us")
	cfg := Config{Identity: id, Port: 53317, Protocol: proto.ProtocolHTTPS}

	own := cfg.message(true)
	if !cfg.isSelf(own) {
		t.Error("our own announcement must be filtered out")
	}

	sameIdentityOtherPort := own
	sameIdentityOtherPort.Port = 49152
	if cfg.isSelf(sameIdentityOtherPort) {
		t.Error("a sibling process on another port must not be filtered out")
	}

	stranger := own
	stranger.Fingerprint = "0000"
	if cfg.isSelf(stranger) {
		t.Error("a foreign device must not be filtered out")
	}
}

// Answering an announcement means calling the peer's register route over
// (m)TLS. This is the path that makes a daemon show up in the official app.
func TestAnswerPeerRegistersWithThePeer(t *testing.T) {
	peerID := testIdentity(t, "Peer")
	usID := testIdentity(t, "Us")

	var mu sync.Mutex
	var seen []proto.Device
	peer, err := server.New(server.Config{
		Identity: peerID,
		Port:     0,
		UseTLS:   true,
		OnPeer: func(d proto.Device) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, d)
		},
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	serveUntilCleanup(t, peer)

	cfg := Config{Identity: usID, Port: 53317, Protocol: proto.ProtocolHTTPS}
	device := proto.Device{
		Alias:    "Peer",
		IP:       "127.0.0.1",
		Port:     peer.Port(),
		Protocol: proto.ProtocolHTTPS,
	}
	if err := cfg.answerPeer(context.Background(), device); err != nil {
		t.Fatalf("answerPeer: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("peer saw %d registrations, want 1", len(seen))
	}
	if seen[0].Alias != "Us" {
		t.Errorf("peer learned alias %q, want %q", seen[0].Alias, "Us")
	}
	// Over TLS the fingerprint is taken from the client certificate, which
	// cannot be spoofed, and must match our identity.
	if seen[0].Fingerprint != usID.Fingerprint {
		t.Errorf("peer learned fingerprint %q, want %q", seen[0].Fingerprint, usID.Fingerprint)
	}
	if seen[0].Port != 53317 {
		t.Errorf("peer learned port %d, want the announced 53317", seen[0].Port)
	}
}

// Answers arrive from several interfaces and sources, so the collector must
// deduplicate by device and stop the scan as soon as the wanted one shows up.
func TestCollectorDeduplicatesAndStops(t *testing.T) {
	stopped := false
	found := &collector{
		devices:  map[string]proto.Device{},
		stopWhen: func(d proto.Device) bool { return d.Alias == "Wanted" },
		stop:     func() { stopped = true },
	}

	device := proto.Device{Alias: "Other", IP: "192.168.1.20", Fingerprint: "AAAA"}
	found.add(device)
	found.add(device)
	sameDeviceOtherAddress := device
	sameDeviceOtherAddress.IP = "10.0.0.5"
	found.add(sameDeviceOtherAddress)

	if all := found.all(); len(all) != 1 {
		t.Errorf("collector holds %d devices, want 1: %+v", len(all), all)
	}
	if stopped {
		t.Error("the scan stopped although the wanted device was not seen")
	}

	found.add(proto.Device{Alias: "Wanted", IP: "192.168.1.21", Fingerprint: "BBBB"})
	if !stopped {
		t.Error("the scan should stop as soon as the wanted device answers")
	}
}

func TestCollectorIgnoresIncompleteDevices(t *testing.T) {
	found := &collector{devices: map[string]proto.Device{}, stop: func() {}}
	found.add(proto.Device{Alias: "", IP: "192.168.1.20"})
	found.add(proto.Device{Alias: "NoAddress"})

	if all := found.all(); len(all) != 0 {
		t.Errorf("collector holds %d devices, want 0", len(all))
	}
}

func TestCollectorSortsByAlias(t *testing.T) {
	found := &collector{devices: map[string]proto.Device{}, stop: func() {}}
	found.add(proto.Device{Alias: "zeta", IP: "192.168.1.3", Fingerprint: "C"})
	found.add(proto.Device{Alias: "Alpha", IP: "192.168.1.1", Fingerprint: "A"})
	found.add(proto.Device{Alias: "beta", IP: "192.168.1.2", Fingerprint: "B"})

	all := found.all()
	want := []string{"Alpha", "beta", "zeta"}
	for i, alias := range want {
		if all[i].Alias != alias {
			t.Errorf("position %d is %q, want %q", i, all[i].Alias, alias)
		}
	}
}

// The full discovery loop: we announce, a listening daemon answers by calling
// our register route, and the scan returns it. Needs working multicast, so it
// is opt-in — run with LOCALSEND_CLI_MULTICAST_TESTS=1.
func TestMulticastRoundTrip(t *testing.T) {
	if os.Getenv(multicastEnv) != "1" {
		t.Skipf("set %s=1 to run the multicast round-trip test", multicastEnv)
	}

	daemonID := testIdentity(t, "RoundTripDaemon")
	scannerID := testIdentity(t, "RoundTripScanner")
	port := freePort(t)

	// The daemon needs an HTTP server so it can be reached, and a listener that
	// answers announcements.
	daemonSrv, err := server.New(server.Config{Identity: daemonID, Port: 0, UseTLS: true})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	serveUntilCleanup(t, daemonSrv)

	daemonCfg := Config{
		Identity:      daemonID,
		Port:          daemonSrv.Port(),
		Protocol:      daemonSrv.Protocol(),
		MulticastPort: port,
	}
	listener, err := Listen(daemonCfg, port, true, nil)
	if err != nil {
		t.Skipf("cannot listen for multicast on this machine: %v", err)
	}
	defer listener.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go listener.Run(ctx)

	devices, err := Scan(ctx, ScanConfig{
		Identity:      scannerID,
		MulticastPort: port,
		Timeout:       6 * time.Second,
		StopWhen:      func(d proto.Device) bool { return d.Alias == "RoundTripDaemon" },
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}

	for _, device := range devices {
		if device.Alias == "RoundTripDaemon" {
			if device.Port != daemonSrv.Port() {
				t.Errorf("discovered port %d, want %d", device.Port, daemonSrv.Port())
			}
			return
		}
	}
	t.Fatalf("the daemon was not discovered; found %+v", devices)
}

// serveUntilCleanup runs the server for the duration of the test. t.Context is
// cancelled just before cleanup, so waiting for the goroutine here guarantees
// it never logs after the test finished.
func serveUntilCleanup(t *testing.T, srv *server.Server) {
	t.Helper()

	ctx := t.Context()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx); err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	t.Cleanup(func() { <-done })
}
