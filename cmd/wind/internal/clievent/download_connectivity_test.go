package clievent

import (
	"github.com/windshare/windshare/core/downloadmetrics"
	"math"
	"testing"
	"time"
)

func TestDownloadConnectivitySealsSnapshotValues(t *testing.T) {
	id := "01010101010101010101010101010101"
	first := 2 * time.Second
	fraction := 0.5
	event, err := (TransferSettled{}).WithDownloadConnectivity(downloadmetrics.Snapshot{
		DownloadID: id, FirstDirectElapsed: &first, DirectBytes: 10, TURNBytes: 5, ApplicationRelayBytes: 5,
		DirectFraction: &fraction, FallbackStall: time.Second, Final: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	first = 0
	fraction = 0
	got, ok := event.DownloadConnectivity()
	if !ok || got.DownloadID != id || *got.FirstDirectElapsed != 2*time.Second || *got.DirectFraction != 0.5 {
		t.Fatal(got)
	}
	*got.FirstDirectElapsed = 0
	*got.DirectFraction = 0
	got, _ = event.DownloadConnectivity()
	if *got.FirstDirectElapsed != 2*time.Second || *got.DirectFraction != 0.5 {
		t.Fatal("mutable snapshot escaped")
	}
	if _, ok := (TransferSettled{}).DownloadConnectivity(); ok {
		t.Fatal("invented metrics")
	}
}
func TestDownloadConnectivityRejectsContradictoryProjection(t *testing.T) {
	base := downloadmetrics.Snapshot{DownloadID: "01010101010101010101010101010101"}
	negative := -time.Second
	for _, change := range []func(*downloadmetrics.Snapshot){
		func(s *downloadmetrics.Snapshot) { s.DownloadID = "invalid" },
		func(s *downloadmetrics.Snapshot) { s.DownloadID = "00" },
		func(s *downloadmetrics.Snapshot) { s.FallbackStall = negative },
		func(s *downloadmetrics.Snapshot) { s.FirstDirectElapsed = &negative },
		func(s *downloadmetrics.Snapshot) { value := math.NaN(); s.DirectFraction = &value },
		func(s *downloadmetrics.Snapshot) { value := 2.0; s.DirectFraction = &value },
		func(s *downloadmetrics.Snapshot) { value := 0.0; s.DirectFraction = &value; s.Incomplete = true },
	} {
		snapshot := base
		change(&snapshot)
		if _, err := (TransferSettled{}).WithDownloadConnectivity(snapshot); err == nil {
			t.Fatal(snapshot)
		}
	}
	event, err := (TransferSettled{}).WithDownloadConnectivity(base)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := event.DownloadConnectivity()
	if got.FirstDirectElapsed != nil || got.DirectFraction != nil {
		t.Fatal(got)
	}
}
