package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

func TestConcurrentSameIDRequestsAdmitExactlyOneRouteAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
	handlerContexts := make(chan context.Context, 1)
	if err := runtime.router.RegisterHandler(
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageHandlerFunc(func(ctx context.Context, _ protocolsession.Message) error {
			handlerContexts <- ctx
			return nil
		}),
	); err != nil {
		t.Fatal(err)
	}
	secondBase, secondPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = secondPeer.Close() })
	secondIdentity := LaneIdentity{ID: runtime.initial.ID + 1, Epoch: 1}
	if _, err := runtime.lanes.add(
		secondIdentity, secondBase, permissiveInboundAuthenticator(), false,
	); err != nil {
		t.Fatal(err)
	}
	operationID := id16[protocolsession.OperationID](121)
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	request, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &operationID, body)
	type result struct {
		disposition protocolsession.OperationDisposition
		err         error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for _, identity := range []LaneIdentity{runtime.initial, secondIdentity} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			disposition, err := (laneInboundRouter{
				runtime: runtime, identity: identity,
			}).RouteInbound(context.Background(), request)
			results <- result{disposition: disposition, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	delivered, replayed := 0, 0
	for result := range results {
		switch {
		case result.err == nil && result.disposition == protocolsession.OperationDeliver:
			delivered++
		case result.err == nil && result.disposition == protocolsession.OperationDrop:
			replayed++
		default:
			t.Fatalf("same-ID admission = disposition %d, error %v", result.disposition, result.err)
		}
	}
	if delivered != 1 || replayed != 1 || runtime.operations.ActiveCount() != 1 || runtime.routes.len() != 1 {
		t.Fatalf("delivered=%d replayed=%d active=%d routes=%d",
			delivered, replayed, runtime.operations.ActiveCount(), runtime.routes.len())
	}
	route := runtime.routes.current(operationID)
	if route == nil {
		t.Fatal("winning request has no route authority")
	}
	conflictingBody, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(2)})
	conflicting, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &operationID, conflictingBody)
	if disposition, err := (laneInboundRouter{
		runtime: runtime, identity: secondIdentity,
	}).RouteInbound(context.Background(), conflicting); disposition != protocolsession.OperationDrop ||
		!errors.Is(err, protocolsession.ErrOperationIDReused) {
		t.Fatalf("conflicting same-ID request = disposition %d, error %v", disposition, err)
	}
	if runtime.routes.current(operationID) != route {
		t.Fatal("conflicting replay replaced the first physical route authority")
	}
	operationContext := dispatchTestOperationContext(t, runtime, handlerContexts)
	lane, err := runtime.lanes.selectLane(&route.preferred)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	outbound := senderOutbound{
		runtime: runtime, privateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
	}
	outcome, err := outbound.SendControl(
		operationContext,
		protocolsession.MessageOpenResults, operationID, body,
	)
	if err != nil || outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("winning authority response = %d, %v", outcome, err)
	}
	stopWriter()
	<-writerDone
	if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("final drain routes=%d active=%d tombstones=%d",
			runtime.routes.len(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func TestForcedHostileSameIDReuseRejectsDelayedOldRouteContext(t *testing.T) {
	// Honest receiver issuers never reuse an OperationID within a ProtocolSession.
	// This forced post-retention collision exercises local ABA containment only.
	var nanos atomic.Int64
	nanos.Store(time.Unix(1_800_000_000, 0).UnixNano())
	now := func() time.Time { return time.Unix(0, nanos.Load()) }
	runtime, _ := newUnstartedRuntimeWithPolicy(
		t, protocolsession.RoleSender,
		protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 4}, now,
	)
	handlerContexts := make(chan context.Context, 2)
	if err := runtime.router.RegisterHandler(
		protocolsession.MessageOpenRevisions,
		protocolsession.MessageHandlerFunc(func(ctx context.Context, _ protocolsession.Message) error {
			handlerContexts <- ctx
			return nil
		}),
	); err != nil {
		t.Fatal(err)
	}
	lane, err := runtime.lanes.selectLane(&runtime.initial)
	if err != nil {
		t.Fatal(err)
	}
	writerContext, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan error, 1)
	go func() { writerDone <- lane.writer.Run(writerContext) }()
	defer func() {
		stopWriter()
		<-writerDone
	}()
	operationID := id16[protocolsession.OperationID](122)
	body, _ := protocolsession.EncodeBody(map[uint64]any{0: uint64(1)})
	request, _ := protocolsession.NewMessage(protocolsession.MessageOpenRevisions, &operationID, body)
	inbound := laneInboundRouter{runtime: runtime, identity: runtime.initial}
	if disposition, err := inbound.RouteInbound(context.Background(), request); err != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("first request = %d, %v", disposition, err)
	}
	oldRoute := runtime.routes.current(operationID)
	oldContext := dispatchTestOperationContext(t, runtime, handlerContexts)
	outbound := senderOutbound{
		runtime: runtime, privateKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
	}
	if outcome, err := outbound.SendControl(
		oldContext, protocolsession.MessageOpenResults, operationID, body,
	); err != nil || outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("first final = %d, %v", outcome, err)
	}
	nanos.Add((protocolsession.OperationTombstoneLifetime + time.Nanosecond).Nanoseconds())
	if disposition, err := inbound.RouteInbound(context.Background(), request); err != nil ||
		disposition != protocolsession.OperationDeliver {
		t.Fatalf("forced colliding request = %d, %v", disposition, err)
	}
	newRoute := runtime.routes.current(operationID)
	if newRoute == nil || newRoute == oldRoute {
		t.Fatal("forced same-ID collision did not receive distinct route authority")
	}
	newContext := dispatchTestOperationContext(t, runtime, handlerContexts)
	if err := outbound.SendOperationError(oldContext, operationID, contentflow.OperationFailure{
		Scope:   contentflow.RevisionErrorScope,
		Code:    contentflow.RevisionCodeUnreadable,
		Message: "stale handler failure",
	}); !errors.Is(err, ErrOperationMissing) {
		t.Fatalf("delayed old generation error = %v", err)
	}
	if runtime.routes.current(operationID) != newRoute || runtime.operations.ActiveCount() != 1 {
		t.Fatalf("old context changed forced colliding generation: route=%p active=%d",
			runtime.routes.current(operationID), runtime.operations.ActiveCount())
	}
	if outcome, err := outbound.SendControl(
		newContext, protocolsession.MessageOpenResults, operationID, body,
	); err != nil || outcome != protocolsession.SendOutcomeDelivered {
		t.Fatalf("forced colliding generation final = %d, %v", outcome, err)
	}
	if runtime.routes.len() != 0 || runtime.operations.ActiveCount() != 0 || runtime.operations.TombstoneCount() != 1 {
		t.Fatalf("forced colliding generation drain routes=%d active=%d tombstones=%d",
			runtime.routes.len(), runtime.operations.ActiveCount(), runtime.operations.TombstoneCount())
	}
}

func dispatchTestOperationContext(
	t *testing.T,
	runtime *runtimeCore,
	contexts <-chan context.Context,
) context.Context {
	t.Helper()
	event, err := runtime.router.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.router.Dispatch(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	return <-contexts
}
