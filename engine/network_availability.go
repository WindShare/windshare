package engine

import (
	"github.com/windshare/windshare/engine/internal/networkruntime"
	"github.com/windshare/windshare/engine/networkdirectory"
)

var ErrNetworkUnavailable = networkdirectory.ErrUnavailable

type NetworkUnavailableError = networkdirectory.UnavailableError
type NetworkBlockingReason = networkdirectory.BlockingReason

const (
	NetworkUnprovisioned        = networkdirectory.Unprovisioned
	NetworkInvalidConfiguration = networkdirectory.InvalidConfiguration
	NetworkTrustStateUnreadable = networkdirectory.TrustStateUnreadable
	NetworkTrustStateInvalid    = networkdirectory.TrustStateInvalid
	NetworkTrustStateUnwritable = networkdirectory.TrustStateUnwritable
	NetworkDirectoryUnavailable = networkdirectory.SnapshotUnavailable
	NetworkNoRelayCandidates    = networkruntime.NoRelayCandidates
	NetworkAliasUnavailable     = networkruntime.AliasUnavailable
)

// NetworkUnavailability reports the default network's current blocking reason
// and permitted retry time. It neither starts maintenance nor judges explicit
// relays, which remain independent of the built-in network.
func (engine *Engine) NetworkUnavailability() *NetworkUnavailableError {
	return engine.network.Unavailability()
}
