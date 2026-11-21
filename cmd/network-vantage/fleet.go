package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/windshare/windshare/internal/networkprobe"
)

const (
	fleetInterval = 5 * time.Minute
	// Keeps worst-case receipt spacing (fleetInterval + fleetBudget + HTTP RTT)
	// comfortably inside the authority's 10-minute SILENT_SECONDS window while
	// still covering 32 simultaneously timed-out nodes across 8 workers.
	fleetBudget     = 3 * time.Minute
	mirrorBudget    = 10 * time.Second
	concurrentNodes = 8
)

func (a *agent) monitor(ctx context.Context) {
	monitorLoop(ctx, fleetInterval, a.fleetCycle)
}

// Wait a full interval after each cycle finishes instead of firing on a
// fixed-rate ticker so a slow failing cycle followed by a fast recovery cycle
// cannot compress consecutive authority receipts below MIN_CYCLE_SECONDS.
func monitorLoop(ctx context.Context, interval time.Duration, run func(context.Context) error) {
	for {
		if err := run(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("fleet cycle failed", "error", err)
		}
		if ctx.Err() != nil {
			return
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *agent) fleetCycle(ctx context.Context) error {
	return a.fleetCycleWithBudget(ctx, fleetBudget, mirrorBudget)
}

func (a *agent) fleetCycleWithBudget(ctx context.Context, cycleLimit, mirrorLimit time.Duration) error {
	var assignment struct {
		Zone    string                `json:"zone"`
		Nodes   []networkprobe.Target `json:"nodes"`
		Origins []string              `json:"origins"`
	}
	if err := a.post(ctx, "targets", "windshare/fleet-targets/v1\x00", map[string]int64{"at": time.Now().Unix()}, &assignment); err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(ctx, cycleLimit)
	defer cancel()
	// Run the mainland mirror check first with its own slice of the cycle budget
	// so black-holed fleet nodes cannot starve mirror freshness reporting.
	var mirror *string
	if assignment.Zone == "cn" {
		mirrorCtx, stopMirror := context.WithTimeout(probe, mirrorLimit)
		mirror = a.mirror(mirrorCtx)
		stopMirror()
	}
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
				slog.Info("fleet probe", "operation_id", target.OperationID, "node_id", target.Node.NodeKey, "https", result.HTTPS)
			}
		})
	}
	workers.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return a.post(ctx, "cycle", "windshare/fleet-cycle/v1\x00", map[string]any{"at": time.Now().Unix(), "mirror": mirror, "nodes": results}, nil)
}
