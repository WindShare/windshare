package turnpath

import (
	"context"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/turncontrol"
)

func TestReceiverQualityMeasuresExactIncarnationAndKeepsCumulativeAccounting(t *testing.T) {
	var qualities []turncontrol.Quality
	var reports []turncontrol.Report
	network := [16]byte{9}
	r := NewReceiver(ReceiverConfig{SessionID: [16]byte{1}, NetworkGeneration: func() [16]byte { return network },
		Send: func(_ context.Context, body []byte) error {
			message, err := turncontrol.Decode(body)
			switch value := message.(type) {
			case turncontrol.Quality:
				qualities = append(qualities, value)
			case turncontrol.Report:
				reports = append(reports, value)
			}
			return err
		},
		Measure: func(lane turncontrol.Lane) Measurement {
			bytes := uint64(2048)
			if lane.Epoch == 2 {
				bytes = 512
			}
			return Measurement{UsefulBytes: bytes, BusyTime: time.Second, PendingBytes: 4096}
		},
	})
	path := &changingReceiverPath{done: make(chan struct{}),
		lane: turncontrol.Lane{ID: 7, Epoch: 1}, live: true}
	entry := &receiverPath{path: path, grant: turncontrol.Grant{
		Authority: turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1}, PathID: [16]byte{3}}}
	r.notifyReady(t.Context(), entry)
	r.measure(t.Context(), entry)
	path.lane = turncontrol.Lane{ID: 8, Epoch: 2}
	// A provider change between readiness and measurement cannot relabel the
	// new lane's counters with the previous authenticated incarnation.
	r.measure(t.Context(), entry)
	if qualities[1].Lane.Epoch != 1 || qualities[1].ContentBytes != 2048 {
		t.Fatal("measurement crossed the authenticated lane boundary")
	}
	network[0]++
	r.notifyReady(t.Context(), entry)
	r.measure(t.Context(), entry)
	last := qualities[len(qualities)-1]
	if last.Lane != path.lane || last.ContentBytes != 512 || last.SampleSequence != 3 ||
		last.NetworkGeneration != network || last.BusyMilliseconds != 1000 || last.PendingBytes != 4096 {
		t.Fatalf("incorrect incarnation measurement: %+v", last)
	}
	if len(reports) != 2 || reports[1].UsefulBytes != 2560 {
		t.Fatalf("cumulative accounting lost an incarnation: %+v", reports)
	}
}
