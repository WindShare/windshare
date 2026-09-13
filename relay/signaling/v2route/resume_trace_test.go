package v2route

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestResumeTraceCorrelatesBoundAndCurrentOwnersOutsideRegistryLock(t *testing.T) {
	now := time.Unix(1_700_020_000, 0)
	registry := newRegistry(t, &now, &memoryTombstones{}, 1)
	fixture := makeFixture(t, 0x63)
	oldOwner, replacement := routeTestConnection("traced-old"), routeTestConnection("traced-new")
	publishRoute(t, registry, fixture, oldOwner)
	var events []ResumeTrace
	registry.resumeTracer = ResumeTraceFunc(func(event ResumeTrace) {
		if !registry.mu.TryLock() {
			t.Fatal("resume trace ran under ownership lock")
		}
		registry.mu.Unlock()
		events = append(events, event)
	})
	attempt, authority := beginFixtureResume(t, registry, fixture)
	if _, err := registry.Resume(context.Background(), attempt, authority, replacement); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Resume(context.Background(), attempt, authority, oldOwner); !errors.Is(err, ErrResumeStale) {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events = %+v", events)
	}
	bound, committed, stale := events[0], events[1], events[2]
	if bound.Phase != ResumeCredentialPhase || bound.Outcome != "accepted" ||
		bound.ShareID != fixture.init.ShareID || bound.ShareInstance != fixture.init.ShareInstance ||
		bound.ExpectedGeneration == 0 || bound.ExpectedOwnerGeneration != oldOwner.LocalGeneration() {
		t.Fatalf("bound event = %+v", bound)
	}
	if committed.Phase != ResumeCommitPhase || committed.Outcome != "accepted" ||
		committed.ExpectedGeneration != bound.ExpectedGeneration ||
		committed.CurrentGeneration == bound.ExpectedGeneration ||
		committed.NewOwnerGeneration != replacement.LocalGeneration() {
		t.Fatalf("commit event = %+v", committed)
	}
	if stale.Outcome != "stale" || stale.ExpectedGeneration != bound.ExpectedGeneration ||
		stale.CurrentGeneration != committed.CurrentGeneration || !errors.Is(stale.Err, ErrResumeStale) {
		t.Fatalf("stale event = %+v", stale)
	}
}

func TestResumeTraceDistinguishesAbsenceCredentialAndStoppingFailures(t *testing.T) {
	now := time.Unix(1_700_021_000, 0)
	store := newBlockingCommitStore()
	registry := newRegistry(t, &now, store, 1)
	fixture := makeFixture(t, 0x67)
	events := make(chan ResumeTrace, 16)
	registry.resumeTracer = ResumeTraceFunc(func(event ResumeTrace) { events <- event })
	attempt, authority := beginFixtureResume(t, registry, fixture)
	<-events
	if _, err := registry.Resume(context.Background(), attempt, authority, routeTestConnection("trace-missing")); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if event := <-events; event.Outcome != "absent" {
		t.Fatalf("absent event = %+v", event)
	}
	badToken := fixture.token
	badToken[0] ^= 1
	registry.BeginResume(context.Background(), attempt.init, badToken)
	if event := <-events; event.Outcome != "invalid_credential" {
		t.Fatalf("credential event = %+v", event)
	}
	owner := routeTestConnection("trace-starting")
	if err := registry.BeginRegistration(fixture.init, owner); err != nil {
		t.Fatal(err)
	}
	registry.BeginResume(context.Background(), attempt.init, fixture.token)
	if event := <-events; event.Outcome != "starting" {
		t.Fatalf("starting event = %+v", event)
	}
	done := startStop(registry, fixture)
	<-store.entered
	registry.BeginResume(context.Background(), attempt.init, fixture.token)
	if event := <-events; event.Outcome != "stopping" {
		t.Fatalf("stopping event = %+v", event)
	}
	store.replies <- commitReply{outcome: CommitCommitted}
	<-done
	registry.BeginResume(context.Background(), attempt.init, fixture.token)
	if event := <-events; event.Outcome != "stopped" {
		t.Fatalf("stopped event = %+v", event)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	registry.BeginResume(ctx, attempt.init, fixture.token)
	if event := <-events; event.Outcome != "failed" {
		t.Fatalf("cancelled event = %+v", event)
	}
	var noTrace ResumeTraceFunc
	noTrace.TraceResume(ResumeTrace{})
}
