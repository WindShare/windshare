package cli

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/windshare/windshare/engine/relaysetup"
)

// A stopped command must not reacquire storage just to render its outcome; an
// unrelated writer may already own the lock and prevent that read from exiting.
type interruptedRelayStore struct {
	relaysetup.Store
	ctx                 context.Context
	loadsAfterInterrupt atomic.Int32
}

func (store *interruptedRelayStore) Load() (relaysetup.Snapshot, error) {
	if store.ctx.Err() != nil {
		store.loadsAfterInterrupt.Add(1)
		return relaysetup.Snapshot{}, errors.New("configuration read after interruption")
	}
	return store.Store.Load()
}
