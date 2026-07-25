package netif

import (
	"net"
	"testing"
)

// Every interface we hand to the multicast code must satisfy the three
// properties a group join needs, otherwise JoinGroup fails at runtime.
func TestMulticastReturnsUsableInterfaces(t *testing.T) {
	interfaces, err := Multicast()
	if err != nil {
		t.Fatalf("Multicast: %v", err)
	}
	if len(interfaces) == 0 {
		t.Skip("this machine has no multicast-capable IPv4 interface")
	}

	for _, ifi := range interfaces {
		if ifi.Flags&net.FlagUp == 0 {
			t.Errorf("%s is down", ifi.Name)
		}
		if ifi.Flags&net.FlagMulticast == 0 {
			t.Errorf("%s cannot carry multicast", ifi.Name)
		}
		if !hasIPv4(ifi) {
			t.Errorf("%s has no IPv4 address", ifi.Name)
		}
	}
}

// The predicate must reject interfaces without an IPv4 address; discovery is
// IPv4-only because the multicast group is.
func TestHasIPv4(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}

	for _, ifi := range interfaces {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		var expected bool
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				expected = true
				break
			}
		}
		if got := hasIPv4(ifi); got != expected {
			t.Errorf("hasIPv4(%s) = %v, want %v", ifi.Name, got, expected)
		}
	}
}

// ReusePort must apply cleanly to a real socket: without it a second listener
// on the multicast port fails.
func TestReusePortAllowsSharingAPort(t *testing.T) {
	lc := net.ListenConfig{Control: ReusePort}

	first, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("first bind: %v", err)
	}
	defer first.Close()

	second, err := lc.ListenPacket(t.Context(), "udp4", first.LocalAddr().String())
	if err != nil {
		t.Fatalf("second bind on %s: %v — SO_REUSEPORT is not taking effect", first.LocalAddr(), err)
	}
	second.Close()
}
