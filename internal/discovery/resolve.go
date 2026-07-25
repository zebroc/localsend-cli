package discovery

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/zebroc/localsend-cli/internal/httpx"
	"github.com/zebroc/localsend-cli/internal/identity"
	"github.com/zebroc/localsend-cli/internal/proto"
	"github.com/zebroc/localsend-cli/internal/ui"
)

// probeTimeout bounds a single attempt to identify a peer at a known address.
const probeTimeout = 3 * time.Second

// ResolveConfig configures target resolution.
type ResolveConfig struct {
	ScanConfig

	// Cached devices are tried before a discovery round is started.
	Cached []proto.Device
}

// Resolve turns a user-supplied target into a device.
//
// An address ("192.168.1.5", "192.168.1.5:53317", "phone.local:53317") is
// contacted directly. Anything else is treated as an alias: the cache is
// verified first, then a discovery round is started.
func Resolve(ctx context.Context, target string, rc ResolveConfig) (proto.Device, error) {
	if host, port, ok := parseAddress(target); ok {
		device, err := Probe(ctx, rc.Identity, host, port, "")
		if err != nil {
			return proto.Device{}, fmt.Errorf("no LocalSend device at %s: %w", target, err)
		}
		if rc.OnDevice != nil {
			rc.OnDevice(device)
		}
		return device, nil
	}

	// A cached device only counts when it answers and still carries the same
	// certificate — otherwise something else took over the address.
	for _, cached := range rc.Cached {
		if !aliasMatches(cached.Alias, target) {
			continue
		}
		device, err := Probe(ctx, rc.Identity, cached.IP, cached.Port, cached.Fingerprint)
		if err != nil {
			ui.Debugf("cached %s did not answer: %v", cached, err)
			continue
		}
		if !aliasMatches(device.Alias, target) {
			ui.Debugf("cached %s is now called %q", cached, device.Alias)
			continue
		}
		ui.Debugf("using cached %s", device)
		if rc.OnDevice != nil {
			rc.OnDevice(device)
		}
		return device, nil
	}

	ui.Infof("looking for %q on the local network…", target)
	sc := rc.ScanConfig
	sc.StopWhen = func(device proto.Device) bool {
		return strings.EqualFold(device.Alias, target)
	}
	devices, err := Scan(ctx, sc)
	if err != nil {
		return proto.Device{}, err
	}
	return pick(devices, target)
}

// pick selects the device matching target, preferring an exact alias match.
func pick(devices []proto.Device, target string) (proto.Device, error) {
	var exact, prefix []proto.Device
	for _, device := range devices {
		switch {
		case strings.EqualFold(device.Alias, target):
			exact = append(exact, device)
		case strings.HasPrefix(strings.ToLower(device.Alias), strings.ToLower(target)):
			prefix = append(prefix, device)
		}
	}

	candidates := exact
	if len(candidates) == 0 {
		candidates = prefix
	}

	switch len(candidates) {
	case 0:
		if len(devices) == 0 {
			return proto.Device{}, fmt.Errorf("no device found — is LocalSend running and on the same network? "+
				"You can also address it directly: localsend-cli <ip>[:%d] <files>", proto.DefaultPort)
		}
		return proto.Device{}, fmt.Errorf("no device called %q; found: %s", target, aliasList(devices))
	case 1:
		return candidates[0], nil
	default:
		return proto.Device{}, fmt.Errorf("%q is ambiguous: %s — address the device directly with <ip>:<port>",
			target, aliasList(candidates))
	}
}

// Probe asks the device at host:port who it is, trying HTTPS before HTTP.
//
// expectFingerprint, when set, pins the certificate: a mismatch is reported as
// an error so the caller can fall back to a fresh discovery round.
func Probe(ctx context.Context, id *identity.Identity, host string, port int, expectFingerprint string) (proto.Device, error) {
	cfg := Config{Identity: id, Port: port, Protocol: proto.ProtocolHTTPS}

	var firstErr error
	for _, protocol := range []string{proto.ProtocolHTTPS, proto.ProtocolHTTP} {
		client := httpx.New(httpx.Options{
			Identity:          id,
			ExpectFingerprint: expectFingerprint,
			Timeout:           probeTimeout,
		})
		base := proto.Device{IP: host, Port: port, Protocol: protocol}.BaseURL()

		// /register is the canonical two-way route and also tells the peer
		// about us; /info is the fallback for implementations without it.
		var response proto.RegisterResponse
		err := httpx.PostJSON(ctx, client, base+"/register", cfg.Register(), &response)
		if err != nil {
			if getErr := httpx.GetJSON(ctx, client, base+"/info", &response); getErr != nil {
				if firstErr == nil {
					firstErr = err
				}
				ui.Debugf("probe %s over %s failed: %v", host, protocol, err)
				continue
			}
		}
		if response.Alias == "" {
			if firstErr == nil {
				firstErr = fmt.Errorf("peer at %s:%d answered without an alias", host, port)
			}
			continue
		}
		return proto.DeviceFromResponse(response, host, port, protocol), nil
	}
	return proto.Device{}, firstErr
}

// parseAddress recognises a literal address. A bare hostname is deliberately
// not recognised: it would shadow aliases, which are the common case. Use
// "host:port" to address a hostname.
func parseAddress(target string) (host string, port int, ok bool) {
	if host, portStr, err := net.SplitHostPort(target); err == nil {
		parsed, err := strconv.Atoi(portStr)
		if err != nil || parsed <= 0 || parsed > 65535 {
			return "", 0, false
		}
		return host, parsed, true
	}
	if net.ParseIP(target) != nil {
		return target, proto.DefaultPort, true
	}
	return "", 0, false
}

func aliasMatches(alias, target string) bool {
	return strings.EqualFold(alias, target) ||
		strings.HasPrefix(strings.ToLower(alias), strings.ToLower(target))
}

func aliasList(devices []proto.Device) string {
	names := make([]string, 0, len(devices))
	for _, device := range devices {
		names = append(names, fmt.Sprintf("%q (%s:%d)", device.Alias, device.IP, device.Port))
	}
	return strings.Join(names, ", ")
}
