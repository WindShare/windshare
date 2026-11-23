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
	current := relayset.SenderStatus{Availability: relayset.SenderAvailability{Total: 2, Available: 1, EverReady: true}}
	controller.bindRelayStatus(func() relayset.SenderStatus { return current })
	first, ok := controller.RelayStatus()
	if !ok || first.Availability.Available != 1 {
		t.Fatal("current availability missing")
	}
	current.Availability.Available = 0
	second, _ := controller.RelayStatus()
	if second.Availability.Available != 0 || !second.Availability.EverReady {
		t.Fatal("availability confused with retained readiness")
	}
	failure := errors.New("preparation failed")
	controller.finish(failure)
	if _, err, settled := controller.Startup(); !settled || !errors.Is(err, failure) {
		t.Fatal("startup failure not retained")
	}
	current.Availability.Available = 2
	frozen, _ := controller.RelayStatus()
	if frozen.Availability.Available != 0 {
		t.Fatal("terminal snapshot retained live relay owner")
	}
	controller.bindRelayStatus(func() relayset.SenderStatus { return current })
	if stillFrozen, _ := controller.RelayStatus(); stillFrozen.Availability.Available != 0 {
		t.Fatal("finished controller accepted a late relay status binding")
	}
}
