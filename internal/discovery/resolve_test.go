package discovery

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/proto"
)

func TestParseAddress(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantPort int
		wantOK   bool
	}{
		{in: "192.168.1.5", wantHost: "192.168.1.5", wantPort: proto.DefaultPort, wantOK: true},
		{in: "192.168.1.5:53317", wantHost: "192.168.1.5", wantPort: 53317, wantOK: true},
		{in: "192.168.1.5:8080", wantHost: "192.168.1.5", wantPort: 8080, wantOK: true},
		{in: "phone.local:53317", wantHost: "phone.local", wantPort: 53317, wantOK: true},
		{in: "[fe80::1]:53317", wantHost: "fe80::1", wantPort: 53317, wantOK: true},
		{in: "fe80::1", wantHost: "fe80::1", wantPort: proto.DefaultPort, wantOK: true},

		// A bare name is an alias, not a host: aliases are the common case and
		// would otherwise be shadowed by hostname lookups.
		{in: "phone", wantOK: false},
		{in: "Nice Orange", wantOK: false},
		{in: "phone.local", wantOK: false},

		// Nonsense ports must not be mistaken for an address.
		{in: "192.168.1.5:0", wantOK: false},
		{in: "192.168.1.5:99999", wantOK: false},
		{in: "192.168.1.5:abc", wantOK: false},
	}

	for _, test := range tests {
		host, port, ok := parseAddress(test.in)
		if ok != test.wantOK {
			t.Errorf("parseAddress(%q) ok = %v, want %v", test.in, ok, test.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if host != test.wantHost || port != test.wantPort {
			t.Errorf("parseAddress(%q) = %q, %d; want %q, %d", test.in, host, port, test.wantHost, test.wantPort)
		}
	}
}

func TestPickExactBeatsPrefix(t *testing.T) {
	devices := []proto.Device{
		{Alias: "Pixel Tablet", IP: "192.168.1.21"},
		{Alias: "Pixel", IP: "192.168.1.20"},
	}
	got, err := pick(devices, "Pixel")
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.IP != "192.168.1.20" {
		t.Errorf("picked %+v, want the exact match", got)
	}
}

func TestPickIsCaseInsensitive(t *testing.T) {
	devices := []proto.Device{{Alias: "OnePlus 10T", IP: "192.168.1.22"}}
	if _, err := pick(devices, "oneplus 10t"); err != nil {
		t.Errorf("pick: %v", err)
	}
}

func TestPickPrefix(t *testing.T) {
	devices := []proto.Device{{Alias: "OnePlus 10T", IP: "192.168.1.22"}}
	got, err := pick(devices, "onep")
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	if got.Alias != "OnePlus 10T" {
		t.Errorf("picked %q", got.Alias)
	}
}

// Sending to the wrong device is worse than failing, so an ambiguous alias must
// be reported with the candidates rather than guessed.
func TestPickAmbiguous(t *testing.T) {
	devices := []proto.Device{
		{Alias: "Pixel 8", IP: "192.168.1.20"},
		{Alias: "Pixel 9", IP: "192.168.1.21"},
	}
	_, err := pick(devices, "Pixel")
	if err == nil {
		t.Fatal("an ambiguous alias must not resolve")
	}
	for _, want := range []string{"ambiguous", "Pixel 8", "Pixel 9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestPickNoMatchListsWhatWasFound(t *testing.T) {
	devices := []proto.Device{{Alias: "MacBook", IP: "192.168.1.2"}}
	_, err := pick(devices, "Fridge")
	if err == nil {
		t.Fatal("a missing alias must fail")
	}
	if !strings.Contains(err.Error(), "MacBook") {
		t.Errorf("error %q should list the devices that were found", err)
	}
}

func TestPickNothingFoundSuggestsDirectAddress(t *testing.T) {
	_, err := pick(nil, "Pixel")
	if err == nil {
		t.Fatal("an empty network must fail")
	}
	// Without discovery the user needs the escape hatch.
	if !strings.Contains(err.Error(), "<ip>") {
		t.Errorf("error %q should suggest addressing the device directly", err)
	}
}

func TestAliasMatches(t *testing.T) {
	if !aliasMatches("OnePlus 10T", "oneplus") {
		t.Error("prefix match should succeed")
	}
	if !aliasMatches("Pixel", "PIXEL") {
		t.Error("match should ignore case")
	}
	if aliasMatches("Pixel", "Tablet") {
		t.Error("unrelated aliases must not match")
	}
}

// Probe must identify a peer over HTTPS and report the alias it answers with.
func TestProbeOverHTTPS(t *testing.T) {
	id := testIdentity(t, "Prober")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proto.BasePath+"/register" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, proto.RegisterResponse{
			Alias:       "Nice Orange",
			Version:     proto.Version,
			Fingerprint: "AABBCC",
			DeviceType:  "mobile",
		})
	}))
	defer srv.Close()

	host, port := hostPort(t, srv.URL)
	device, err := Probe(context.Background(), id, host, port, "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if device.Alias != "Nice Orange" {
		t.Errorf("alias = %q", device.Alias)
	}
	if device.Protocol != proto.ProtocolHTTPS {
		t.Errorf("protocol = %q, want https", device.Protocol)
	}
	if device.Port != port {
		t.Errorf("port = %d, want %d", device.Port, port)
	}
}

// Implementations without /register must still be identifiable through /info.
func TestProbeFallsBackToInfo(t *testing.T) {
	id := testIdentity(t, "Prober")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proto.BasePath+"/info" {
			http.Error(w, "not implemented", http.StatusNotFound)
			return
		}
		writeJSON(t, w, proto.RegisterResponse{Alias: "Legacy Peer", Version: "2.0"})
	}))
	defer srv.Close()

	host, port := hostPort(t, srv.URL)
	device, err := Probe(context.Background(), id, host, port, "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if device.Alias != "Legacy Peer" {
		t.Errorf("alias = %q, want %q", device.Alias, "Legacy Peer")
	}
}

// A peer that only speaks plain HTTP must be found after the HTTPS attempt
// fails.
func TestProbeFallsBackToHTTP(t *testing.T) {
	id := testIdentity(t, "Prober")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proto.BasePath+"/register" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, proto.RegisterResponse{Alias: "Plain Peer", Version: proto.Version})
	}))
	defer srv.Close()

	host, port := hostPort(t, srv.URL)
	device, err := Probe(context.Background(), id, host, port, "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if device.Protocol != proto.ProtocolHTTP {
		t.Errorf("protocol = %q, want http", device.Protocol)
	}
	if device.Alias != "Plain Peer" {
		t.Errorf("alias = %q", device.Alias)
	}
}

// A cached device whose certificate changed must be rejected, so the caller
// rescans instead of transferring to whatever took over the address.
func TestProbeRejectsFingerprintMismatch(t *testing.T) {
	id := testIdentity(t, "Prober")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, proto.RegisterResponse{Alias: "Impostor", Version: proto.Version})
	}))
	defer srv.Close()

	host, port := hostPort(t, srv.URL)
	_, err := Probe(context.Background(), id, host, port,
		"1111111111111111111111111111111111111111111111111111111111111111")
	if err == nil {
		t.Fatal("Probe accepted a peer with a different certificate")
	}
}

func TestProbeUnreachable(t *testing.T) {
	id := testIdentity(t, "Prober")

	// Port 1 on the loopback interface has nothing listening.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Probe(ctx, id, "127.0.0.1", 1, ""); err == nil {
		t.Error("Probe should fail when nothing answers")
	}
}

// Resolve must contact a literal address directly, without any discovery round.
func TestResolveUsesLiteralAddress(t *testing.T) {
	id := testIdentity(t, "Prober")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, proto.RegisterResponse{Alias: "Direct", Version: proto.Version})
	}))
	defer srv.Close()

	host, port := hostPort(t, srv.URL)
	var seen []proto.Device
	device, err := Resolve(context.Background(), net.JoinHostPort(host, strconv.Itoa(port)), ResolveConfig{
		ScanConfig: ScanConfig{
			Identity: id,
			Timeout:  2 * time.Second,
			OnDevice: func(d proto.Device) { seen = append(seen, d) },
		},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if device.Alias != "Direct" {
		t.Errorf("alias = %q", device.Alias)
	}
	if len(seen) != 1 {
		t.Errorf("OnDevice was called %d times, want 1 so the cache learns the peer", len(seen))
	}
}

// A cached entry that no longer answers must not fail the send outright; the
// caller falls through to a scan.
func TestResolveIgnoresDeadCacheEntry(t *testing.T) {
	id := testIdentity(t, "Prober")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := Resolve(ctx, "Ghost", ResolveConfig{
		ScanConfig: ScanConfig{
			Identity: id,
			// A short round keeps the test quick; nothing will answer anyway.
			Timeout:       300 * time.Millisecond,
			MulticastPort: freePort(t),
		},
		Cached: []proto.Device{{Alias: "Ghost", IP: "127.0.0.1", Port: 1, Protocol: proto.ProtocolHTTPS}},
	})
	if err == nil {
		t.Fatal("Resolve should fail when neither the cache nor the scan finds the device")
	}
	// It must have moved past the cache into a scan, not reported a probe error.
	if !strings.Contains(err.Error(), "no device") {
		t.Errorf("error %q should come from the scan, not from probing the stale entry", err)
	}
}

func testIdentity(t *testing.T, alias string) *identity.Identity {
	t.Helper()

	t.Setenv(identity.ConfigDirEnv, t.TempDir())
	id, err := identity.Load(alias)
	if err != nil {
		t.Fatalf("identity.Load: %v", err)
	}
	return id
}

func hostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()

	trimmed := strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	host, portStr, err := net.SplitHostPort(trimmed)
	if err != nil {
		t.Fatalf("split %q: %v", rawURL, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port of %q: %v", rawURL, err)
	}
	return host, port
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// freePort returns a port that is currently unused, so a scan in a test does
// not collide with a real LocalSend instance on the default port.
func freePort(t *testing.T) int {
	t.Helper()

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).Port
}
