//go:build !windows || 386

package gateway

import (
	"context"
	"github.com/windshare/windshare/connectivity/networkstate"
	r "github.com/windshare/windshare/connectivity/reachability"
)

// No portable OS DHCP cache API exposes repeated RFC7291 options. Unsupported
// platforms retain the standard default-router fallback and configured sources.
func (SystemDHCPSource) Acquire(context.Context, networkstate.Address) (DHCPOptions, error) {
	return DHCPOptions{}, r.ErrUnavailable
}
