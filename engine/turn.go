package engine

import (
	"context"
	"time"

	"github.com/windshare/windshare/engine/turn"
)

// ConfigureTURN atomically publishes manual accounts and managed deployment
// settings without contacting issuers or resetting Engine-lifetime budgets.
func (engine *Engine) ConfigureTURN(settings turn.Settings) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closing {
		return ErrClosed
	}
	return engine.turn.Configure(settings)
}

func (engine *Engine) QueryTURNUsage(ctx context.Context, account string, firstDate, lastDate time.Time) (turn.Usage, error) {
	return engine.turn.QueryUsage(ctx, account, firstDate, lastDate)
}

func (engine *Engine) TURNStatus() []turn.Status { return engine.turn.Status() }
