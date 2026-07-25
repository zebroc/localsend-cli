// Package netif selects the network interfaces used for multicast discovery.
package netif

import (
	"net"
)

// Multicast returns the interfaces that can carry IPv4 multicast: up,
// multicast-capable and holding at least one IPv4 address.
//
// Loopback is included so that a daemon and a sender running on the same host
// can discover each other.
func Multicast() ([]net.Interface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var usable []net.Interface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 {
			continue
		}
		if !hasIPv4(ifi) {
			continue
		}
		usable = append(usable, ifi)
	}
	return usable, nil
}

// LocalIPv4 returns the IPv4 addresses assigned to this host, loopback included.
func LocalIPv4() map[string]bool {
	out := map[string]bool{}
	all, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range all {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				if ip4 := ipnet.IP.To4(); ip4 != nil {
					out[ip4.String()] = true
				}
			}
		}
	}
	return out
}

func hasIPv4(ifi net.Interface) bool {
	addrs, err := ifi.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
			return true
		}
	}
	return false
}
