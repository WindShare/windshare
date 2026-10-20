package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/windshare/windshare/engine/relayconfig"
	"github.com/windshare/windshare/engine/turnaccount"
	"github.com/windshare/windshare/engine/turnconfig"
	"github.com/windshare/windshare/engine/turnprovider"
)

// ConfigureTURN publishes local settings without contacting issuers. Account IDs
// retain their Engine-lifetime usage; existing leases keep their issuing source.
func (engine *Engine) ConfigureTURN(config turnconfig.Config) error {
	config = config.Clone()
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closing {
		return ErrClosed
	}
	before, err := json.Marshal(engine.config.TURN)
	if err != nil {
		return err
	}
	after, err := json.Marshal(config)
	if err != nil {
		return err
	}
	if bytes.Equal(before, after) {
		return nil
	}
	dependencies := engine.config.TURNDependencies
	if dependencies.Now == nil {
		dependencies.Now = engine.config.Now
	}
	prepared, err := config.Build(dependencies)
	if err != nil {
		return err
	}
	if err = engine.turn.Reconfigure(prepared); err != nil {
		return err
	}
	engine.config.TURN = config
	return nil
}

// ConfigureManagedTURN applies relay authority and user budgets; discovery remains asynchronous.
func (engine *Engine) ConfigureManagedTURN(config relayconfig.Config) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.closing {
		return ErrClosed
	}
	return engine.turnServices.Configure(config)
}

func (engine *Engine) QueryTURNUsage(ctx context.Context, account string, firstDate, lastDate time.Time) (turnprovider.Usage, error) {
	engine.mu.Lock()
	owner := engine.turn
	engine.mu.Unlock()
	return owner.QueryUsage(ctx, account, firstDate, lastDate)
}
func (engine *Engine) TURNStatus() []turnaccount.Snapshot {
	engine.mu.Lock()
	owner := engine.turn
	engine.mu.Unlock()
	return owner.Snapshot()
}
