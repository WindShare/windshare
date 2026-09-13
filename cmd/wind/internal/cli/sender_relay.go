package cli

import (
	"context"
	"time"

	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/connectivity/relayset"
)

// The command assembles wake notifications; the connectivity lifecycle owns
// retry decisions for both initial publication and every later replacement.
func wakeSenderRelaysOnNetworkChange(ctx context.Context, set *relayset.Sender) {
	monitor := networkstate.NewMonitor(nil, networkstate.DefaultDebounce)
	ticker := time.NewTicker(networkstate.DefaultPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if _, changed, err := monitor.Poll(ctx, now); err == nil && changed {
				set.Wake()
			}
		}
	}
}
