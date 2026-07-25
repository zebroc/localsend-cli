//go:build unix

package netif

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// ReusePort makes a UDP socket share its port with other listeners.
//
// SO_REUSEPORT is what actually allows a second process (e.g. the official
// LocalSend app) to bind UDP 53317 at the same time on macOS and BSD; Linux
// needs it too for multiple receivers of the same multicast group.
func ReusePort(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			serr = err
			return
		}
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			serr = err
		}
	})
	if err != nil {
		return err
	}
	return serr
}
