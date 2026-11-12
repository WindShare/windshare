package share

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/connectivity/relayset"
)

func TestSnapshotQueriesDoNotDependOnObservationDelivery(t *testing.T) {
	controller := NewController()
	if _, _, settled := controller.Startup(); settled {
		t.Fatal("premature startup completion")
	}
	current := relayset.SenderAvailability{Total: 2, Available: 1, EverReady: true}
	controller.bindAvailability(func() relayset.SenderAvailability { return current })
	first, ok := controller.Availability()
	if !ok || first.Available != 1 {
		t.Fatal("current availability missing")
	}
	current.Available = 0
	second, _ := controller.Availability()
	if second.Available != 0 || !second.EverReady {
		t.Fatal("availability confused with retained readiness")
	}
	failure := errors.New("preparation failed")
	controller.finish(failure)
	if _, err, settled := controller.Startup(); !settled || !errors.Is(err, failure) {
		t.Fatal("startup failure not retained")
	}
	current.Available = 2
	frozen, _ := controller.Availability()
	if frozen.Available != 0 {
		t.Fatal("terminal snapshot retained live relay owner")
	}
}
