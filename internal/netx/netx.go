// Package netx holds small network predicates shared by the runtime, the CDP
// bridge and the CLI.
package netx

import (
	"net"
	"strings"
)

// IsLoopbackIP reports whether host is an IP literal in a loopback range
// (127.0.0.0/8 or ::1). Host names, including "localhost", are not literals.
func IsLoopbackIP(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsLoopbackHost reports whether host, as returned by url.URL.Hostname or
// net.SplitHostPort, is "localhost" (any case) or a loopback IP literal.
func IsLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || IsLoopbackIP(host)
}
