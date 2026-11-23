// Runs the native receive workflow with direct interface discovery unavailable.
package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/pprof"
	"testing"
	"time"

	"github.com/windshare/windshare/adapters/filesystem"
	"github.com/windshare/windshare/connectivity/nativepeer"
	"github.com/windshare/windshare/connectivity/networkstate"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/engine/internal/nativeconnectivity"
	"github.com/windshare/windshare/engine/internal/receive"
)

type noDirectNetwork struct{}

func (noDirectNetwork) Poll(context.Context, time.Time) (networkstate.Snapshot, bool, error) {
	return networkstate.Snapshot{}, false, errors.New("fixture has no direct route")
}

// This dedicated process has no other Pion owners. Inspecting their stacks
// avoids counting unrelated Go timers and HTTP connection-pool workers.
func pionWorkers() int {
	var stacks bytes.Buffer
	_ = pprof.Lookup("goroutine").WriteTo(&stacks, 2)
	count := 0
	for stack := range bytes.SplitSeq(stacks.Bytes(), []byte("\n\n")) {
		if bytes.Contains(stack, []byte("github.com/pion/")) {
			count++
		}
	}
	return count
}

func runTURNNetworkReceive(destination string, resume bool) error {
	baseline := pionWorkers()
	capability, err := link.Parse(os.Getenv("WINDSHARE_TEST_CAPABILITY"))
	if err != nil {
		return errors.New("invalid test capability")
	}
	capacity, err := liveshare.NewReceiveCapacity(liveshare.ReceiveCapacityLimits{})
	if err != nil {
		return err
	}
	defer capacity.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	request := receive.Request{Capability: capability, Connectivity: receive.ConnectivityAuto, Destination: destination,
		Output: filesystem.FilesystemOutput{RootPath: destination, CreateRoot: true}}
	if resume {
		// Let the ordinary recovery window expire while the relay is offline so
		// the test can exercise an explicit retry from the filesystem checkpoint.
		request.WaitTimeout = 4 * time.Second
		request.OnPause = func(ctx context.Context, p receive.Pause) (receive.RecoveryAction, error) {
			if p.Recovery.Retention != receive.RetentionCheckpoints || !p.Allows(receive.RecoveryRetry) || p.Transfer.Progress.VerifiedBytes == 0 {
				return receive.RecoveryExit, errors.New("network loss did not retain verified progress")
			}
			fmt.Printf("checkpoint %d\n", p.Transfer.Progress.VerifiedBytes)
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Stat(filepath.Join(destination, "reconnect")); err == nil {
					return receive.RecoveryRetry, nil
				}
				select {
				case <-ctx.Done():
					return receive.RecoveryExit, ctx.Err()
				case <-ticker.C:
				}
			}
		}
	}
	finished := receive.Run(ctx, request, receive.Dependencies{
		Capacity: capacity, Connectivity: nativeconnectivity.NewCapacity(nativepeer.AdmissionClock{}, noDirectNetwork{})})
	if finished.Err != nil {
		return finished.Err
	}
	deadline := time.Now().Add(5 * time.Second)
	for pionWorkers() > baseline {
		if time.Now().After(deadline) {
			return errors.New("native receive left Pion workers running")
		}
		time.Sleep(25 * time.Millisecond)
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		ResumedFiles uint64
		downloadmetrics.Snapshot
	}{finished.Value.Transfer.Progress.FileOutcomes.ResumedFiles, finished.Value.Connectivity})
}

func TestTURNNetworkReceive(t *testing.T) {
	destination := os.Getenv("WINDSHARE_TURN_TEST_DESTINATION")
	if destination == "" {
		t.Skip("launched in an isolated process by the parallel TURN network scenario")
	}
	if err := runTURNNetworkReceive(destination, os.Getenv("WINDSHARE_TURN_TEST_RESUME") == "true"); err != nil {
		t.Fatal(err)
	}
}
