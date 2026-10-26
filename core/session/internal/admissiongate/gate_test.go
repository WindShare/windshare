package admissiongate_test

import (
	"context"
	"errors"
	"github.com/windshare/windshare/core/session/internal/admissiongate"
	"testing"
	"testing/synctest"
)

func TestSealCancelsAdmissionsAndCloseJoinsTheirCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gate admissiongate.Gate
		caller := context.Background()
		ctx, release, ok := gate.Begin(caller)
		if !ok {
			t.Fatal("open gate rejected admission")
		}
		defer func() { release(); gate.Close() }()

		if !gate.Seal() || gate.Seal() {
			t.Fatal("sealing did not choose exactly one shutdown owner")
		}
		if late, end, admitted := gate.Begin(caller); admitted || late != nil || end != nil {
			t.Fatal("sealed gate admitted new work")
		}
		closed := make(chan struct{}, 2)
		for range cap(closed) {
			go func() { gate.Close(); closed <- struct{}{} }()
		}
		synctest.Wait()
		if !errors.Is(ctx.Err(), context.Canceled) || caller.Err() != nil {
			t.Fatal("sealing must cancel admitted work without canceling its caller")
		}
		if len(closed) != 0 {
			t.Fatal("close returned before admitted work released its resources")
		}

		for range cap(closed) {
			go release()
		}
		synctest.Wait()
		if len(closed) != cap(closed) {
			t.Fatal("concurrent closes did not join the completed cleanup")
		}
	})
}

func TestCallerCancellationLeavesSiblingAdmissionsUsable(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gate admissiongate.Gate
		caller, cancel := context.WithCancel(context.Background())
		first, releaseFirst, ok := gate.Begin(caller)
		if !ok {
			t.Fatal("open gate rejected first admission")
		}
		defer func() { releaseFirst(); gate.Close() }()
		cancel()
		synctest.Wait()
		if !errors.Is(first.Err(), context.Canceled) {
			t.Fatal("admission lost caller cancellation")
		}
		second, releaseSecond, ok := gate.Begin(context.Background())
		if !ok {
			t.Fatal("caller cancellation sealed the gate")
		}
		defer releaseSecond()
		synctest.Wait()
		if second.Err() != nil {
			t.Fatal("caller cancellation affected a sibling admission")
		}
		releaseFirst()
		if second.Err() != nil {
			t.Fatal("releasing one admission canceled a sibling")
		}
	})
}

func TestAdmissionRacingCloseAlwaysSettles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const contenders = 16
		var gate admissiongate.Gate
		start := make(chan struct{})
		settled := make(chan struct{}, contenders)
		for range contenders {
			go func() {
				<-start
				ctx, release, ok := gate.Begin(context.Background())
				if ok {
					<-ctx.Done()
					release()
				} else if ctx != nil || release != nil {
					t.Error("rejected admission retained borrowed ownership")
				}
				settled <- struct{}{}
			}()
		}
		closed := make(chan struct{})
		go func() { <-start; gate.Close(); close(closed) }()
		close(start)
		synctest.Wait()
		if len(settled) != contenders {
			t.Fatal("close stranded a racing admission")
		}
		select {
		case <-closed:
		default:
			t.Fatal("close did not join racing admissions")
		}
		if _, _, ok := gate.Begin(context.Background()); ok {
			t.Fatal("close reopened admission")
		}
	})
}

func TestAbsentGateOrCallerDoesNotAdmitWork(t *testing.T) {
	for _, test := range []struct {
		name   string
		gate   *admissiongate.Gate
		caller context.Context
	}{
		{name: "gate", caller: context.Background()},
		{name: "caller", gate: &admissiongate.Gate{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if ctx, release, ok := test.gate.Begin(test.caller); ok || ctx != nil || release != nil {
				t.Fatal("missing admission input acquired ownership")
			}
			test.gate.Close()
		})
	}
	var missing *admissiongate.Gate
	if missing.Seal() {
		t.Fatal("missing gate claimed shutdown ownership")
	}
}
