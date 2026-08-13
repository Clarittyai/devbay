package proxy

import (
	"net/netip"
	"os"
)

// The proxy's :80 is the one port devbay binds on all interfaces, because a
// bay URL opened on a phone or in a simulator has to reach it. The admin API
// stays on loopback: it can rewrite the entire routing table.
var (
	anyAddr      = netip.MustParseAddr("0.0.0.0")
	loopbackAddr = netip.MustParseAddr("127.0.0.1")
)

// BindEnv names the variable that overrides what the proxy binds to.
const BindEnv = "DEVBAY_PROXY_BIND"

// bindAddr is the address the proxy publishes :80 on.
//
// All interfaces by default, for the reason above. It is overridable because
// the default is a considered trade rather than a universally right answer:
// on a network you do not control, every bay -- and whatever its services were
// given to talk to -- is reachable by anything that can route to this machine
// and knows a hostname. Setting DEVBAY_PROXY_BIND=127.0.0.1 gives that up in
// exchange for bay URLs that only open on this machine.
func bindAddr() netip.Addr {
	v := os.Getenv(BindEnv)
	if v == "" {
		return anyAddr
	}
	addr, err := netip.ParseAddr(v)
	if err != nil {
		// An unparseable value must not silently widen the binding: someone
		// who set this variable at all was trying to narrow it.
		return loopbackAddr
	}
	return addr
}
