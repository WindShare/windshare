package transfer

import (
	"context"
	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/downloadmetrics"
	"github.com/windshare/windshare/core/session/protocolsession"
	"sync/atomic"
	"testing"
	"time"
)

func metricsBroker(t *testing.T, lanes *LaneSet, descriptor content.FileRevisionDescriptor) *BlockBroker {
	t.Helper()
	budget, err := NewPlaintextBudget(DefaultProcessPlaintextBytes)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := NewBlockBroker(BlockBrokerConfig{ShareInstance: descriptor.ShareInstance(), Lanes: lanes, ProcessBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broker.Close)
	return broker
}
func metricRead(broker *BlockBroker, descriptor content.FileRevisionDescriptor, start, end uint64) error {
	return broker.ReadRange(context.Background(), transferID[content.LeaseID](77), descriptor, content.Range{Offset: start, End: end}, RangeSinkFunc(func(context.Context, uint64, []byte) error { return nil }))
}

func TestDownloadMetricsSurviveGenerationAndLaneEpochReplacement(t *testing.T) {
	var tick atomic.Int64
	now := func() time.Time { return time.UnixMilli(tick.Load()) }
	metrics := downloadmetrics.New("download", now, false)
	descriptor := transferDescriptor(t, 2)
	makeLanes := func(generation byte) *LaneSet {
		lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](generation), RaceWidth: 1, Now: now})
		if err != nil {
			t.Fatal(err)
		}
		lanes.BindDownloadMetrics(metrics)
		return lanes
	}
	direct := makeLanes(90)
	directBroker := metricsBroker(t, direct, descriptor)
	tick.Store(10)
	lane := laneFunction(func(_ context.Context, demand BlockDemand) (records.BlockRecord, error) {
		return transferRecord(t, descriptor, demand.Index), nil
	})
	if err := direct.Add(LaneIdentity{ID: 1, Epoch: 2}, LaneRouteDirect, lane); err != nil {
		t.Fatal(err)
	}
	chunk := uint64(catalog.MinChunkSize)
	if err := metricRead(directBroker, descriptor, 0, chunk); err != nil {
		t.Fatal(err)
	}
	if direct.Remove(LaneIdentity{ID: 1, Epoch: 1}) {
		t.Fatal("stale detach applied")
	}
	direct.Remove(LaneIdentity{ID: 1, Epoch: 2})
	direct.Close()
	tick.Store(1000)
	relay := makeLanes(91)
	defer relay.Close()
	relayBroker := metricsBroker(t, relay, descriptor)
	if err := relay.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteTURN, lane); err != nil {
		t.Fatal(err)
	}
	// Same immutable revision under fresh protocol: no duplicate useful credit.
	if err := metricRead(relayBroker, descriptor, 0, chunk*2); err != nil {
		t.Fatal(err)
	}
	got := metrics.Snapshot(true)
	if got.FirstDirectElapsed == nil || *got.FirstDirectElapsed != 10*time.Millisecond || got.DirectBytes != chunk ||
		got.TURNBytes != chunk || got.DirectFraction == nil || *got.DirectFraction != 0.5 || got.Incomplete {
		t.Fatal(got)
	}
}
func TestDownloadMetricsDoNotAttributeRetiredLaneCompletion(t *testing.T) {
	metrics := downloadmetrics.New("download", nil, false)
	descriptor := transferDescriptor(t, 1)
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](92), RaceWidth: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	lanes.BindDownloadMetrics(metrics)
	broker := metricsBroker(t, lanes, descriptor)
	started, finish := make(chan struct{}), make(chan struct{})
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteDirect, laneFunction(func(context.Context, BlockDemand) (records.BlockRecord, error) {
		close(started)
		<-finish
		return transferRecord(t, descriptor, 0), nil
	})); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- metricRead(broker, descriptor, 0, 1) }()
	<-started
	lanes.Remove(LaneIdentity{ID: 1, Epoch: 1})
	close(finish)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	got := metrics.Snapshot(true)
	if got.UnknownBytes != 1 || got.DirectBytes != 0 || !got.Incomplete || got.DirectFraction != nil {
		t.Fatal(got)
	}
}
func TestDownloadMetricsExcludeResumeAlignmentOverreadAndPreserveCacheProvenance(t *testing.T) {
	descriptor := transferDescriptor(t, 2)
	metrics := downloadmetrics.New("download", nil, false)
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](93), RaceWidth: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	lanes.BindDownloadMetrics(metrics)
	broker := metricsBroker(t, lanes, descriptor)
	var fetches atomic.Int32
	lane := laneFunction(func(_ context.Context, demand BlockDemand) (records.BlockRecord, error) {
		fetches.Add(1)
		return transferRecord(t, descriptor, demand.Index), nil
	})
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 1}, LaneRouteDirect, lane); err != nil {
		t.Fatal(err)
	}
	chunk := uint64(catalog.MinChunkSize)
	if err := metricRead(broker, descriptor, chunk-1, chunk); err != nil {
		t.Fatal(err)
	}
	lanes.Remove(LaneIdentity{ID: 1, Epoch: 1})
	if err := lanes.Add(LaneIdentity{ID: 1, Epoch: 2}, LaneRouteRelay, lane); err != nil {
		t.Fatal(err)
	}
	// Cached direct bytes keep original provenance; they are not relabeled relay.
	if err := metricRead(broker, descriptor, chunk-1, chunk); err != nil {
		t.Fatal(err)
	}
	if err := metricRead(broker, descriptor, chunk, chunk+1024); err != nil {
		t.Fatal(err)
	}
	got := metrics.Snapshot(true)
	if got.DirectBytes != 1 || got.ApplicationRelayBytes != 1024 || got.DirectFraction == nil || *got.DirectFraction != 1.0/1025 || got.Incomplete || fetches.Load() != 2 {
		t.Fatal(got, fetches.Load())
	}
}

func TestDownloadMetricsExcludeOutputWaitWithActivePrefetch(t *testing.T) {
	var tick atomic.Int64
	now := func() time.Time { return time.UnixMilli(tick.Load()) }
	metrics := downloadmetrics.New("download-output", now, false)
	descriptor := transferDescriptor(t, 2)
	lanes, err := NewLaneSet(LaneSetConfig{ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](99), RaceWidth: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	lanes.BindDownloadMetrics(metrics)
	secondStarted, releaseSecond := make(chan struct{}), make(chan struct{})
	lane := laneFunction(func(_ context.Context, demand BlockDemand) (records.BlockRecord, error) {
		if demand.Index == 1 {
			close(secondStarted)
			<-releaseSecond
		}
		return transferRecord(t, descriptor, demand.Index), nil
	})
	identity := LaneIdentity{ID: 1, Epoch: 1}
	if err = lanes.Add(identity, LaneRouteDirect, lane); err != nil {
		t.Fatal(err)
	}
	broker := metricsBroker(t, lanes, descriptor)
	err = broker.ReadRange(context.Background(), transferID[content.LeaseID](77), descriptor, content.Range{Offset: 0, End: uint64(catalog.MinChunkSize) * 2}, RangeSinkFunc(func(_ context.Context, offset uint64, _ []byte) error {
		if offset == 0 {
			<-secondStarted
			lanes.Remove(identity)
			tick.Store(10000)
			close(releaseSecond)
		}
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	got := metrics.Snapshot(true)
	if got.FallbackStall != 0 {
		t.Fatalf("output-only delay charged fallback stall: %v", got.FallbackStall)
	}
}
