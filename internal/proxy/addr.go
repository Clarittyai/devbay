package proxy

import "net/netip"

// The proxy's :80 is the one port devbay binds on all interfaces, because a
// bay URL opened on a phone or in a simulator has to reach it. The admin API
// stays on loopback: it can rewrite the entire routing table.
var (
	anyAddr      = netip.MustParseAddr("0.0.0.0")
	loopbackAddr = netip.MustParseAddr("127.0.0.1")
)
