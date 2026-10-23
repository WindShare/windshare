package turnpath

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/core/connectionpath"
	"github.com/windshare/windshare/core/session/turncontrol"
)

type selectionAccounts struct{ closed int }

func (*selectionAccounts) Inventory() Inventory {
	return Inventory{Revision: 1, Profiles: []Profile{{Identity: iceconfig.Identity{Provider: "test", Account: "owner", Profile: "primary"}}}}
}

func (a *selectionAccounts) Acquire(context.Context, string, Subject) (*Lease, error) {
	credential, _ := iceconfig.Fixed("user", "password")
	relay, _ := iceconfig.NewRelay(a.Inventory().Profiles[0].Identity,
		[]string{"turn:relay.example?transport=udp", "turns:relay.example:443", "turn:relay.example?transport=tcp"}, credential)
	return NewLease(LeaseConfig{Relay: relay, Release: func() { a.closed++ }}), nil
}

type selectionHarness struct {
	sender   *Sender
	now      time.Time
	network  uint64
	sequence map[[16]byte]uint64
	accounts *selectionAccounts
	events   []Event
}

func newSelectionHarness(t *testing.T) *selectionHarness {
	t.Helper()
	h := &selectionHarness{now: time.Unix(1700000000, 0), network: 1, sequence: make(map[[16]byte]uint64), accounts: &selectionAccounts{}}
	h.sender = NewSender(SenderConfig{Accounts: h.accounts, SessionID: [16]byte{1}, Now: func() time.Time { return h.now },
		Observe:           func(event Event) { h.events = append(h.events, event) },
		NetworkGeneration: func() uint64 { return h.network }, Send: func(context.Context, []byte) error { return nil }})
	t.Cleanup(h.sender.close)
	h.sender.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(h.sender, 1), HoldFor: turncontrol.MaximumDemandLifetime})
	issueNext(t, h.sender)
	return h
}

func (h *selectionHarness) admit(t *testing.T, path *delegation, id uint32) *Admission {
	t.Helper()
	lane := turncontrol.Lane{ID: id, Epoch: id}
	admission := h.sender.Admit(t.Context(), path.grant.PathID, lane, make(chan struct{}))
	h.sender.apply(t.Context(), turncontrol.Ready{Authority: path.grant.Authority, PathID: path.grant.PathID, Lane: lane})
	return admission
}

func (h *selectionHarness) sample(path *delegation, seconds, bytes, pending uint64) {
	h.sequence[path.grant.PathID]++
	h.sender.retainQuality(turncontrol.Quality{Authority: path.grant.Authority, PathID: path.grant.PathID, Lane: path.readyLane,
		NetworkGeneration: [16]byte{9}, SampleSequence: h.sequence[path.grant.PathID], ContentBytes: bytes,
		BusyMilliseconds: seconds * 1000, PendingBytes: pending})
}

func (h *selectionHarness) startEvaluation(t *testing.T) *delegation {
	t.Helper()
	old := h.sender.services[0].pending
	h.admit(t, old, 1)
	for second := uint64(0); second <= 10; second += 2 {
		h.now = old.created.Add(time.Duration(second) * time.Second)
		h.sample(old, second, second*64*1024, 1<<20)
	}
	h.sender.maintain(t.Context())
	return old
}

func (h *selectionHarness) startTrial(t *testing.T) (*delegation, *delegation) {
	t.Helper()
	old := h.startEvaluation(t)
	issueNext(t, h.sender)
	trial := h.sender.services[0].pending
	if trial == nil || trial.trial == nil || trial.transport != iceconfig.RelayTLS {
		t.Fatalf("missing bounded TLS contender: %+v", trial)
	}
	h.admit(t, trial, 2)
	if h.sender.services[0].current != old || h.accounts.closed != 0 {
		t.Fatal("readiness retired the predecessor before measuring improvement")
	}
	return old, trial
}

func TestEvaluationYieldsToContinuity(t *testing.T) {
	for _, reason := range []string{"renewal", "recovery"} {
		t.Run(reason, func(t *testing.T) {
			h := newSelectionHarness(t)
			old, _ := h.startTrial(t)
			want := recoveryPath
			if reason == "renewal" {
				want = renewalPath
				credential, _ := iceconfig.Expiring("user", "password", h.now.Add(time.Second))
				old.lease.config.Relay, _ = iceconfig.NewRelay(old.relay.Identity(), old.lease.Relay().URLs(), credential)
				old.lease.config.Renew = func(ctx context.Context, subject Subject) (*Lease, error) {
					return h.accounts.Acquire(ctx, "primary", subject)
				}
			} else {
				h.sender.Recover(old.grant.PathID, turncontrol.ChannelUnavailable)
			}
			h.sender.maintain(t.Context())
			service := h.sender.services[0]
			if service.current != old || service.pending != nil || h.accounts.closed != 1 ||
				service.issuance == nil || service.issuance.intent != want {
				t.Fatal("performance evaluation blocked continuity or released the working path")
			}
			issueNext(t, h.sender)
			if service.pending.intent != want || service.pending.transport != iceconfig.RelayTCP {
				t.Fatal("continuity inherited the speculative transport")
			}
		})
	}
}

func TestCanceledEvaluationCannotInstallAnAlreadyIssuedCredential(t *testing.T) {
	h := newSelectionHarness(t)
	old := h.startEvaluation(t)
	result := <-h.sender.completed
	h.sender.Recover(old.grant.PathID, turncontrol.ChannelUnavailable)
	h.sender.maintain(t.Context())
	h.sender.install(t.Context(), result)
	service := h.sender.services[0]
	if service.current != old || service.pending != nil || h.accounts.closed != 1 ||
		service.issuance == nil || service.issuance.intent != recoveryPath {
		t.Fatal("completed speculative issuance escaped cancellation")
	}
	issueNext(t, h.sender)
}

func TestEvaluationFailureDoesNotBackOffWorkingServiceRecovery(t *testing.T) {
	for _, phase := range []string{"issuance", "receiver"} {
		t.Run(phase, func(t *testing.T) {
			h := newSelectionHarness(t)
			old := h.startEvaluation(t)
			if phase == "issuance" {
				result := <-h.sender.completed
				result.err = errors.New("provider temporarily unavailable")
				h.sender.install(t.Context(), result)
				event := h.events[len(h.events)-1]
				if event.Milestone != connectionpath.MilestoneEvaluationUnavailable || event.Reason != result.err.Error() ||
					event.Subject.PathID != result.path {
					t.Fatalf("evaluation failure lost its lifecycle evidence: %+v", event)
				}
			} else {
				issueNext(t, h.sender)
				trial := h.sender.services[0].pending
				h.sender.apply(t.Context(), turncontrol.Retire{Authority: trial.grant.Authority,
					PathID: trial.grant.PathID, Reason: turncontrol.Unhelpful})
			}
			h.sender.Recover(old.grant.PathID, turncontrol.ChannelUnavailable)
			h.sender.maintain(t.Context())
			service := h.sender.services[0]
			if service.current != old || service.issuance == nil || service.issuance.intent != recoveryPath {
				t.Fatal("failed optimization delayed recovery of a previously working service")
			}
			issueNext(t, h.sender)
		})
	}
}

func TestTCPFirstDelegatesIdenticalTransportToBothPeersAndFallsBack(t *testing.T) {
	h := newSelectionHarness(t)
	delegated, selected := h.events[len(h.events)-2], h.events[len(h.events)-1]
	if delegated.Milestone != connectionpath.MilestoneCredentialsDelegated || selected.Milestone != connectionpath.MilestoneTransportSelection ||
		selected.Subject != delegated.Subject || selected.Subject.PathID == ([16]byte{}) ||
		selected.Reason != string(initialPath) {
		t.Fatalf("credential delegation lost transport-selection evidence: %+v", h.events)
	}
	service := h.sender.services[0]
	for index, transport := range []iceconfig.RelayTransport{iceconfig.RelayTCP, iceconfig.RelayTLS, iceconfig.RelayUDP} {
		pending := service.pending
		if pending.transport != transport {
			t.Fatalf("attempt %d selected %s instead of %s", index, pending.transport, transport)
		}
		authorized, _, err := h.sender.Lookup(pending.grant.PathID)
		if err != nil || !slices.Equal(authorized.Relay().URLs(), pending.grant.URLs) || len(pending.grant.URLs) != 1 {
			t.Fatalf("native/browser transport authority differs: %v", err)
		}
		if index != 2 {
			h.now = h.now.Add(TransportConnectBudget)
			h.sender.maintain(t.Context())
			issueNext(t, h.sender)
		}
	}
	h.admit(t, service.pending, 1)
	if service.current == nil || h.accounts.closed != 2 {
		t.Fatal("fallback failed to retire unsuccessful allocations")
	}
}

func TestTransportTrialRequiresSustainedUsefulImprovement(t *testing.T) {
	for _, test := range []struct {
		name   string
		rate   uint64
		winner bool
	}{{"faster", 256 * 1024, true}, {"slower", 32 * 1024, false}, {"marginal", 70 * 1024, false}} {
		t.Run(test.name, func(t *testing.T) {
			h := newSelectionHarness(t)
			old, trial := h.startTrial(t)
			for second := uint64(0); second <= 6; second++ {
				h.now = trial.created.Add(time.Duration(second) * time.Second)
				h.sample(old, second+10, (second+10)*64*1024, 1<<20)
				h.sample(trial, second, second*test.rate, 1<<20)
				h.sender.maintain(t.Context())
			}
			service := h.sender.services[0]
			if test.winner {
				if service.current != trial || service.pending != nil || h.accounts.closed != 1 || service.selection.preferred != iceconfig.RelayTLS {
					t.Fatal("measured winner did not replace exactly its authenticated predecessor")
				}
			} else {
				h.now = trial.created.Add(TransportEvaluationBudget)
				h.sender.maintain(t.Context())
				if service.current != old || service.pending != nil || h.accounts.closed != 1 {
					t.Fatal("unhelpful evaluation damaged the working path or leaked its allocation")
				}
			}
		})
	}
}

func TestTransportExplorationSkipsShortIdleAndDrainedDemand(t *testing.T) {
	h := newSelectionHarness(t)
	old := h.sender.services[0].pending
	h.admit(t, old, 1)
	for second := uint64(0); second <= 10; second += 2 {
		h.now = old.created.Add(time.Duration(second) * time.Second)
		h.sample(old, second, second*64*1024, 0)
		if h.sender.evaluationDue(h.sender.services[0], h.now) {
			t.Fatal("idle work triggered exploration")
		}
	}
	h.now = h.now.Add(time.Second)
	h.sample(old, 11, 11*64*1024, 1024)
	if h.sender.evaluationDue(h.sender.services[0], h.now) {
		t.Fatal("nearly drained work paid for exploration")
	}
	h.sender.apply(t.Context(), turncontrol.Demand{Authority: fixtureHeader(h.sender, 2), HoldFor: 0})
	if len(h.sender.paths) != 0 {
		t.Fatal("finished content retained TURN resources")
	}
}

func TestTrialScopeAndBudgetCannotRetireWorkingPredecessor(t *testing.T) {
	for _, reason := range []string{"network", "byte-budget", "admission"} {
		t.Run(reason, func(t *testing.T) {
			h := newSelectionHarness(t)
			old, trial := h.startTrial(t)
			switch reason {
			case "network":
				h.network++
			case "byte-budget":
				h.sample(trial, 1, TransportEvaluationBytes, 1<<20)
			case "admission":
				h.sender.Admit(t.Context(), old.grant.PathID, turncontrol.Lane{ID: 3, Epoch: 3}, make(chan struct{}))
				h.now = h.now.Add(TransportEvaluationBudget)
			}
			h.sender.maintain(t.Context())
			if h.sender.services[0].current != old || h.sender.services[0].pending != nil || h.accounts.closed != 1 {
				t.Fatal("evaluation crossed its resource or authentication boundary")
			}
		})
	}
}

func TestQualityRejectsReplaysAndResetsAcrossIncarnations(t *testing.T) {
	var history qualityHistory
	now := time.Unix(1700000000, 0)
	q := turncontrol.Quality{Lane: turncontrol.Lane{ID: 1, Epoch: 1}, SampleSequence: 1, ContentBytes: 100, BusyMilliseconds: 1000}
	history.retain(q, now)
	q.SampleSequence, q.ContentBytes, q.BusyMilliseconds = 2, 300, 3000
	history.retain(q, now.Add(2*time.Second))
	if measured := history.measure(now.Add(2 * time.Second)); !measured.measured || measured.rate != 100 {
		t.Fatalf("incorrect busy-interval throughput: %+v", measured)
	}
	if history.retain(q, now.Add(3*time.Second)) {
		t.Fatal("sample replay refreshed its evidence")
	}
	q.SampleSequence, q.ContentBytes = 3, 1
	if history.retain(q, now.Add(3*time.Second)) {
		t.Fatal("counter rollback created an invalid throughput")
	}
	q.Lane.Epoch++
	if !history.retain(q, now.Add(3*time.Second)) || history.measure(now.Add(3*time.Second)).measured {
		t.Fatal("new incarnation inherited old throughput")
	}
}
