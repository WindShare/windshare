package runtrace

import (
	"encoding/json"
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/downloadmetrics"
	"strings"
	"testing"
	"time"
)

func TestDownloadConnectivityFinalExportUsesExistingTransferSettlement(t *testing.T) {
	base := allTraceEvents(t)[9].(clievent.TransferSettled)
	now := time.Unix(0, 0)
	metrics := downloadmetrics.New("01010101010101010101010101010101", func() time.Time { return now }, true)
	metrics.Delivered("r", 0, 10, downloadmetrics.Direct)
	metrics.Availability(false)
	done := metrics.Pending()
	now = now.Add(2 * time.Second)
	metrics.Delivered("r", 10, 20, downloadmetrics.TURN)
	done()
	event, err := base.WithDownloadConnectivity(metrics.Snapshot(true))
	if err != nil {
		t.Fatal(err)
	}
	record, err := encodeV3(testRunIdentity(0x12), entryMetadata{sequence: 1, time: now}, event)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"transfer_settled"`, `"download_connectivity":{`, `"first_direct_elapsed_ms":"0"`, `"direct_bytes":"10"`, `"turn_bytes":"10"`, `"direct_fraction":0.5`, `"fallback_stall_ms":"2000"`, `"final":true`} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("%s missing %s", encoded, want)
		}
	}
	unknown := downloadmetrics.New("01010101010101010101010101010101", nil, false)
	event, err = base.WithDownloadConnectivity(unknown.Snapshot(true))
	if err != nil {
		t.Fatal(err)
	}
	record, err = encodeV3(testRunIdentity(0x12), entryMetadata{sequence: 2, time: now}, event)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(record)
	if !strings.Contains(string(encoded), `"first_direct_elapsed_ms":null`) || !strings.Contains(string(encoded), `"direct_fraction":null`) {
		t.Fatal(string(encoded))
	}
}
