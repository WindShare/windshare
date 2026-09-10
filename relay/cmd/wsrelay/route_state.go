package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/windshare/windshare/relay/signaling/v2route"
)

// Route state owns the database for exactly one endpoint-server lifetime.
type relayRouteState struct {
	registry   *v2route.Registry
	tombstones *v2route.FileTombstoneStore
}

func openRelayRoutes(
	ctx context.Context,
	stateDirectory string,
	config v2route.Config,
	logf func(string, ...any),
) (relayRouteState, error) {
	tombstones, err := v2route.NewFileTombstoneStore(filepath.Join(stateDirectory, tombstoneFilename))
	if err != nil {
		return relayRouteState{}, fmt.Errorf("wsrelay: initialize STOP index: %w", err)
	}
	config.Random = rand.Reader
	config.Tombstones = tombstones
	config.StopTracer = v2route.StopTraceFunc(func(event v2route.StopTrace) {
		logf("wsrelay: stop share_id=%x stop_id=%x outcome=%s active_routes=%d route_capacity=%d error=%v",
			event.ShareID, event.StopID, event.Outcome, event.ActiveRoutes, event.RouteCapacity, event.Err)
	})
	registry, err := v2route.New(ctx, config)
	if err != nil {
		return relayRouteState{}, fmt.Errorf("wsrelay: initialize route registry: %w", errors.Join(err, tombstones.Close()))
	}
	return relayRouteState{registry: registry, tombstones: tombstones}, nil
}

func (state relayRouteState) Close(logf func(string, ...any)) {
	if err := state.tombstones.Close(); err != nil {
		logf("wsrelay: close STOP index: %v", err)
	}
}
