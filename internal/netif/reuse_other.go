//go:build !unix

package netif

import "syscall"

// ReusePort is a no-op on platforms without SO_REUSEPORT. Binding the multicast
// port may then fail if another LocalSend instance already holds it.
func ReusePort(_, _ string, _ syscall.RawConn) error { return nil }
