package networkprobe

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

func TestSTUNRequiresMappedAddressFromTheRequestedTransaction(t *testing.T) {
	for _, kind := range []string{"binding", "different_transaction", "missing_mapping"} {
		t.Run(kind, func(t *testing.T) {
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			_ = client.SetDeadline(time.Now().Add(time.Second))
			done := make(chan error, 1)
			go func() {
				var raw [2048]byte
				n, err := server.Read(raw[:])
				if err != nil {
					done <- err
					return
				}
				request := &stun.Message{Raw: raw[:n]}
				if err := request.Decode(); err != nil {
					done <- err
					return
				}
				response := stun.MustBuild(request, stun.BindingSuccess)
				if kind == "different_transaction" {
					response.NewTransactionID()
				}
				if kind != "missing_mapping" {
					err = (&stun.XORMappedAddress{IP: net.IPv4(192, 0, 2, 1), Port: 3478}).AddTo(response)
					if err != nil {
						done <- err
						return
					}
				}
				_, err = server.Write(response.Raw)
				done <- err
			}()
			err := exchangeBinding(client, bindingRTO)
			if setup := <-done; setup != nil {
				t.Fatal(setup)
			}
			if (err == nil) != (kind == "binding") {
				t.Fatalf("binding result %v", err)
			}
		})
	}
}
func TestBindingRetriesLossAndFallsBackToReachableFamily(t *testing.T) {
	socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	end, _ := ctx.Deadline()
	_ = socket.SetDeadline(end)
	done := make(chan error, 1)
	go func() {
		var packet [2048]byte
		n, _, err := socket.ReadFrom(packet[:])
		if err != nil {
			done <- err
			return
		}
		first := append([]byte(nil), packet[:n]...)
		n, addr, err := socket.ReadFrom(packet[:])
		if err != nil {
			done <- err
			return
		}
		if !bytes.Equal(first, packet[:n]) {
			done <- errors.New("retransmission changed transaction")
			return
		}
		req := &stun.Message{Raw: packet[:n]}
		if err := req.Decode(); err != nil {
			done <- err
			return
		}
		ip := addr.(*net.UDPAddr)
		response, err := stun.Build(req, stun.BindingSuccess, &stun.XORMappedAddress{IP: ip.IP, Port: ip.Port})
		if err == nil {
			_, err = socket.WriteTo(response.Raw, addr)
		}
		done <- err
	}()
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "[::1]:3478" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	winner, err := raceFamilies(ctx, "udp", []string{"[::1]:3478"}, []string{socket.LocalAddr().String()}, bindingDial(dial, 10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer winner.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func newTestShares(t *testing.T) [OriginCount]SyntheticShare {
	t.Helper()
	var shares [OriginCount]SyntheticShare
	for i := range shares {
		share, err := NewShare()
		if err != nil {
			t.Fatal(err)
		}
		shares[i] = share
	}
	return shares
}

func TestCheckRelayOriginsRunsInParallel(t *testing.T) {
	t.Parallel()
	origins := []string{"https://one.example", "https://two.example", "https://three.example"}
	tickets := []string{"ticket-0", "ticket-1", "ticket-2"}
	shares := newTestShares(t)

	started := make(chan struct{}, OriginCount)
	release := make(chan struct{})
	var (
		mu   sync.Mutex
		seen = make(map[string]string, OriginCount)
	)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- checkRelayOrigins(ctx, origins, tickets, shares, func(originCtx context.Context, share SyntheticShare, origin, ticket string) error {
			mu.Lock()
			seen[origin] = ticket
			mu.Unlock()
			var matched bool
			for i, wantOrigin := range origins {
				if origin == wantOrigin && share.ID() == shares[i].ID() && ticket == tickets[i] {
					matched = true
				}
			}
			if !matched {
				return errors.New("mismatched origin, share, or ticket")
			}
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-originCtx.Done():
				return originCtx.Err()
			}
		})
	}()

	// All three exchanges must be in flight simultaneously before any completes.
	for range OriginCount {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("origin probes did not start concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("checkRelayOrigins failed: %v", err)
	}
	if len(seen) != OriginCount {
		t.Fatalf("checked %d origins, want %d", len(seen), OriginCount)
	}
}

func TestCheckRelayOriginsCancelsSiblingsWaitsForCleanupAndPreservesRootCause(t *testing.T) {
	t.Parallel()
	origins := []string{"https://one.example", "https://two.example", "https://three.example"}
	tickets := []string{"ticket-0", "ticket-1", "ticket-2"}
	shares := newTestShares(t)
	rootErr := errors.New("origin policy rejected")

	siblingsReady := make(chan struct{}, OriginCount-1)
	var cleanupsFinished atomic.Int32
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := checkRelayOrigins(ctx, origins, tickets, shares, func(originCtx context.Context, _ SyntheticShare, origin, _ string) error {
		if origin == origins[1] {
			for range OriginCount - 1 {
				select {
				case <-siblingsReady:
				case <-originCtx.Done():
					return originCtx.Err()
				}
			}
			return rootErr
		}
		siblingsReady <- struct{}{}
		<-originCtx.Done()
		// Simulate bounded STOP cleanup running after sibling cancellation.
		time.Sleep(15 * time.Millisecond)
		cleanupsFinished.Add(1)
		return originCtx.Err()
	})
	if !errors.Is(err, rootErr) {
		t.Fatalf("error = %v, want root cause %v", err, rootErr)
	}
	if !strings.Contains(err.Error(), origins[1]) {
		t.Fatalf("error = %q, want failing origin %q", err.Error(), origins[1])
	}
	if got := cleanupsFinished.Load(); got != OriginCount-1 {
		t.Fatalf("finished %d sibling cleanups before return, want %d", got, OriginCount-1)
	}
}
