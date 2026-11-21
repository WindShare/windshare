package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/networkprobe"
)

const (
	fleetInterval   = 5 * time.Minute
	fleetBudget     = 4 * time.Minute
	concurrentNodes = 8
)

func (a *agent) monitor(ctx context.Context) {
	ticks := time.NewTicker(fleetInterval)
	defer ticks.Stop()
	for {
		if err := a.fleetCycle(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("fleet cycle failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks.C:
		}
	}
}

func (a *agent) fleetCycle(ctx context.Context) error {
	var assignment struct {
		Zone    string                `json:"zone"`
		Cycle   uint64                `json:"cycle"`
		Nodes   []networkprobe.Target `json:"nodes"`
		Origins []string              `json:"origins"`
	}
	if err := a.post(ctx, "targets", "windshare/fleet-targets/v1\x00", map[string]int64{"at": time.Now().Unix()}, &assignment); err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, fleetBudget)
	defer cancel()
	var mu sync.Mutex
	results := make(map[string]networkprobe.Observation)
	jobs := make(chan networkprobe.Target, len(assignment.Nodes))
	for _, target := range assignment.Nodes {
		jobs <- target
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(concurrentNodes, len(assignment.Nodes)) {
		workers.Go(func() {
			for target := range jobs {
				if probe.Err() != nil {
					return
				}
				check, stop := context.WithTimeout(probe, checkTimeout)
				result := networkprobe.Observe(check, a.client, target, assignment.Origins)
				stop()
				// An incomplete cycle cannot supply negative evidence about a node.
				if probe.Err() != nil {
					return
				}
				mu.Lock()
				results[target.Node.NodeKey] = result
				mu.Unlock()
				slog.Info("fleet probe", "operation_id", target.OperationID, "node_id", target.Node.NodeKey, "cycle", assignment.Cycle, "https", result.HTTPS)
			}
		})
	}
	workers.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var mirror *string
	if assignment.Zone == "cn" {
		mirror = a.mirror(ctx)
	}
	return a.post(ctx, "cycle", "windshare/fleet-cycle/v1\x00", map[string]any{"cycle": assignment.Cycle, "at": time.Now().Unix(), "mirror": mirror, "nodes": results}, nil)
}
