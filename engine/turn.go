package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/windshare/windshare/engine/turnaccount"
	"github.com/windshare/windshare/engine/turnconfig"
	"github.com/windshare/windshare/engine/turnprovider"
)

// ConfigureTURN is local setup, not credential acquisition. An account budget
// cannot be reset underneath running shares by re-importing the same settings.
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
	if len(engine.tasks) != 0 || len(engine.config.TURN.Accounts) != 0 {
		return errors.New("TURN account settings require a new Engine instance")
	}
	dependencies := engine.config.TURNDependencies
	if dependencies.Now == nil {
		dependencies.Now = engine.config.Now
	}
	prepared, err := config.Build(dependencies)
	if err != nil {
		return err
	}
	owner, err := turnaccount.New(prepared)
	if err != nil {
		return err
	}
	if err = engine.turn.Close(); err != nil {
		_ = owner.Close()
		return err
	}
	engine.turn = owner
	engine.config.TURN = config
	return nil
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
