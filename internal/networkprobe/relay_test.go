package networkprobe

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestCleanupReserveBoundsTailInsideOuterDeadline(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	if got := cleanupReserve(now, now); got != 0 {
		t.Fatalf("expired deadline reserve = %v, want 0", got)
	}
	if got := cleanupReserve(now, now.Add(-time.Second)); got != 0 {
		t.Fatalf("past deadline reserve = %v, want 0", got)
	}
	if got := cleanupReserve(now, now.Add(18*time.Second)); got != stopCleanupBudget {
		t.Fatalf("18s deadline reserve = %v, want %v", got, stopCleanupBudget)
	}
	if got := cleanupReserve(now, now.Add(45*time.Second)); got != stopCleanupBudget {
		t.Fatalf("45s deadline reserve = %v, want %v", got, stopCleanupBudget)
	}
	if got := cleanupReserve(now, now.Add(2*time.Second)); got != 500*time.Millisecond {
		t.Fatalf("2s deadline reserve = %v, want 500ms", got)
	}
}

func newTestChallenge(purpose v2.ChallengePurpose) v2.Challenge {
	challenge := v2.Challenge{
		Purpose:              purpose,
		ExpiresAtUnixSeconds: uint64(time.Now().Add(30 * time.Second).Unix()),
	}
	_, _ = rand.Read(challenge.ID[:])
	_, _ = rand.Read(challenge.Nonce[:])
	return challenge
}

func startStalledJoinRelay(t *testing.T, hangStop bool) (*httptest.Server, *atomic.Bool, *atomic.Bool, <-chan struct{}) {
	t.Helper()
	var stopAttempted atomic.Bool
	var stopCompleted atomic.Bool
	joinStalled := make(chan struct{})
	var signalJoin sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		ctx := r.Context()
		_, first, err := conn.Read(ctx)
		if err != nil || len(first) < 4 {
			return
		}
		switch string(first[:4]) {
		case "WS2R":
			init, err := v2.ParseDeliveryRegisterInit(first)
			if err != nil {
				return
			}
			challenge, err := newTestChallenge(v2.ChallengeRegister).MarshalBinary()
			if err != nil || conn.Write(ctx, websocket.MessageBinary, challenge) != nil {
				return
			}
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
			ack, err := (v2.Registered{
				ShareID:          init.Registration.ShareID,
				ShareInstance:    init.Registration.ShareInstance,
				DescriptorDigest: init.Registration.DescriptorDigest,
			}).MarshalBinary()
			if err != nil || conn.Write(ctx, websocket.MessageBinary, ack) != nil {
				return
			}
			// Keep the sender socket open until the probe tears it down.
			for {
				if _, _, err := conn.Read(ctx); err != nil {
					return
				}
			}
		case "WS2J":
			// Stall the receiver path so the probe hits its exchange deadline.
			signalJoin.Do(func() { close(joinStalled) })
			<-ctx.Done()
		case "WS2X":
			stopAttempted.Store(true)
			if hangStop {
				<-ctx.Done()
				return
			}
			stopInit, err := v2.ParseStopInit(first)
			if err != nil {
				return
			}
			challenge, err := newTestChallenge(v2.ChallengeStop).MarshalBinary()
			if err != nil || conn.Write(ctx, websocket.MessageBinary, challenge) != nil {
				return
			}
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
			stopped, err := (v2.Stopped{StopID: stopInit.StopID}).MarshalBinary()
			if err != nil || conn.Write(ctx, websocket.MessageBinary, stopped) != nil {
				return
			}
			stopCompleted.Store(true)
		}
	})
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)
	return server, &stopAttempted, &stopCompleted, joinStalled
}

func TestSyntheticShareTimeoutStopsShareWithinOuterDeadline(t *testing.T) {
	server, stopAttempted, stopCompleted, _ := startStalledJoinRelay(t, false)
	share, err := NewShare()
	if err != nil {
		t.Fatal(err)
	}
	timeout := 800 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	err = share.exchange(ctx, server.Client(), server.URL, "https://probe.example", "ticket")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exchange error = %v, want %v", err, context.DeadlineExceeded)
	}
	if !stopAttempted.Load() || !stopCompleted.Load() {
		t.Fatalf("stop attempted=%v completed=%v, want both true", stopAttempted.Load(), stopCompleted.Load())
	}
}

func TestSyntheticShareHangingStopDoesNotOverrunOuterDeadline(t *testing.T) {
	server, stopAttempted, stopCompleted, _ := startStalledJoinRelay(t, true)
	share, err := NewShare()
	if err != nil {
		t.Fatal(err)
	}
	timeout := 400 * time.Millisecond
	started := time.Now()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()

	err = share.exchange(ctx, server.Client(), server.URL, "https://probe.example", "ticket")
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("exchange error = %v, want %v", err, context.DeadlineExceeded)
	}
	if !stopAttempted.Load() {
		t.Fatal("expected cleanup to attempt STOP inside the reserved tail")
	}
	if stopCompleted.Load() {
		t.Fatal("hanging STOP unexpectedly completed")
	}
	// If defer Stop ignored the outer deadline, it would add stopCleanupBudget (3s).
	if elapsed >= timeout+stopCleanupBudget/2 {
		t.Fatalf("exchange overran outer deadline: elapsed=%v, timeout=%v", elapsed, timeout)
	}
}

func TestCheckRelayOriginsFailFastStopsRegisteredSiblings(t *testing.T) {
	server, stopAttempted, stopCompleted, joinStalled := startStalledJoinRelay(t, false)
	shares := newTestShares(t)
	origins := []string{"https://one.example", "https://two.example", "https://three.example"}
	tickets := []string{"ticket-0", "ticket-1", "ticket-2"}
	rootErr := errors.New("origin rejected")

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	// Origin 0 registers and stalls in DialReceiver; once the server observes
	// WS2J, Origin 1 fails fast so we verify canceling the sibling still runs
	// its defer STOP cleanly inside the outer deadline.
	err := checkRelayOrigins(ctx, origins, tickets, shares, func(originCtx context.Context, share SyntheticShare, origin, ticket string) error {
		switch origin {
		case origins[0]:
			return share.exchange(originCtx, server.Client(), server.URL, origin, ticket)
		case origins[1]:
			select {
			case <-joinStalled:
				return rootErr
			case <-originCtx.Done():
				return originCtx.Err()
			}
		default:
			<-originCtx.Done()
			return originCtx.Err()
		}
	})
	if !errors.Is(err, rootErr) {
		t.Fatalf("error = %v, want %v", err, rootErr)
	}
	if !stopAttempted.Load() || !stopCompleted.Load() {
		t.Fatalf("sibling stop attempted=%v completed=%v, want both true", stopAttempted.Load(), stopCompleted.Load())
	}
}
