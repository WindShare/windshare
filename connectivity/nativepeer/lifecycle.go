package nativepeer

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/connectivity/reachability"
	"github.com/windshare/windshare/connectivity/v2signal"
)

// A speculative opportunity belongs to the authenticated session, even when
// its receiver constructs another PeerSet after the first path finishes.
func (n *NativePeerConnectivity) ClaimPrewarm(session [16]byte) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed || n.prewarmed[session] || len(n.prewarmed) >= MaximumPaths {
		return false
	}
	n.prewarmed[session] = true
	return true
}

func (n *NativePeerConnectivity) SetDirect(session [16]byte, pathID v2signal.PeerPathID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	key := pathKey{session, pathID}
	path := n.paths[key]
	if path == nil {
		return
	}
	changed := !path.direct
	path.direct = true
	for attempt := range path.attempts {
		attempt.release()
	}
	if changed {
		n.observeLifecycleLocked(key, path, DemandChanged, 0)
	}
	n.refreshDemandLocked(key, path)
}

// Cleanup owns no socket reference and runs for at most the mapping grace plus
// one bounded gateway pass. Intent Close joins it before returning.
func (n *NativePeerConnectivity) scheduleCleanupLocked(authority *reachability.Authority, retired bool) {
	if authority == nil {
		return
	}
	n.cleanup.Add(1)
	go func() {
		defer n.cleanup.Done()
		ctx, cancel := context.WithTimeout(context.Background(), reachability.DefaultGrace+reachability.DefaultOperationTimeout)
		defer cancel()
		if retired {
			authority.Close(ctx)
			return
		}
		timer := time.NewTimer(reachability.DefaultGrace)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			authority.Reconcile(ctx)
		}
	}()
}
func (n *NativePeerConnectivity) Close(ctx context.Context) error {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return nil
	}
	n.closed = true
	var paths []pathKey
	for key := range n.paths {
		paths = append(paths, key)
	}
	authority := n.config.Reachability
	n.mu.Unlock()
	for _, key := range paths {
		n.ClosePath(key.session, key.path)
	}
	n.operations.Wait()
	n.config.Discovery.Close()
	if authority != nil {
		authority.Close(ctx)
	}
	n.cleanup.Wait()
	return errors.Join(n.config.Sockets.Close(), ctx.Err())
}

func (n *NativePeerConnectivity) Retired(session [16]byte, pathID v2signal.PeerPathID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	path := n.paths[pathKey{session, pathID}]
	return path != nil && path.retired
}
