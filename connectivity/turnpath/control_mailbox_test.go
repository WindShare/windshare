package turnpath

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/session/turncontrol"
)

func fillCommands(t *testing.T, mailbox *controlMailbox) {
	t.Helper()
	for sequence := uint64(1); sequence <= controlCommandCapacity; sequence++ {
		if err := mailbox.put(t.Context(), turncontrol.Demand{Authority: turncontrol.Header{Sequence: sequence}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestControlMailboxBackpressureRetainsCommandOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mailbox := newControlMailbox()
		fillCommands(t, mailbox)
		ready := turncontrol.Ready{PathID: [16]byte{9}, Lane: turncontrol.Lane{ID: 2, Epoch: 3}}
		accepted := make(chan error, 1)
		go func() { accepted <- mailbox.put(t.Context(), ready) }()
		synctest.Wait()
		select {
		case err := <-accepted:
			t.Fatalf("full mailbox acknowledged an unowned command: %v", err)
		default:
		}
		first, ok := mailbox.take()
		if !ok || first.Header().Sequence != 1 {
			t.Fatal("oldest command was displaced")
		}
		synctest.Wait()
		if err := <-accepted; err != nil {
			t.Fatal(err)
		}
		for sequence := uint64(2); sequence <= controlCommandCapacity; sequence++ {
			message, ok := mailbox.take()
			if !ok || message.Header().Sequence != sequence {
				t.Fatalf("command %d was lost or reordered", sequence)
			}
		}
		if message, ok := mailbox.take(); !ok || message != ready {
			t.Fatal("accepted readiness was lost")
		}
	})
}

func TestControlMailboxCancellationAndShutdownUnblockIngress(t *testing.T) {
	for _, stopOwner := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller canceled", true: "owner stopped"}[stopOwner], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				mailbox := newControlMailbox()
				fillCommands(t, mailbox)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- mailbox.put(ctx, turncontrol.Retire{}) }()
				synctest.Wait()
				want := error(context.Canceled)
				if stopOwner {
					mailbox.close()
					want = errControlStopped
				} else {
					cancel()
				}
				synctest.Wait()
				if err := <-result; !errors.Is(err, want) {
					t.Fatalf("unaccepted command returned %v, want %v", err, want)
				}
				if stopOwner {
					mailbox.close()
					if err := mailbox.put(t.Context(), turncontrol.Ready{}); !errors.Is(err, errControlStopped) {
						t.Fatalf("stopped owner accepted another command: %v", err)
					}
				} else if len(mailbox.queue) != controlCommandCapacity {
					t.Fatal("canceled command changed retained commands")
				}
			})
		})
	}
}

func TestInventoryCoalescingPreservesLifecycleBarriers(t *testing.T) {
	mailbox := newControlMailbox()
	header := turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 1}
	for sequence := uint64(1); sequence <= controlCommandCapacity*2; sequence++ {
		header.Sequence = sequence
		if err := mailbox.put(t.Context(), turncontrol.Available{Authority: header, Profiles: []string{"primary"}}); err != nil {
			t.Fatal(err)
		}
	}
	firstRevision := header.Sequence
	grant := turncontrol.Grant{Authority: header, PathID: [16]byte{3}}
	if err := mailbox.put(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	header.Sequence++
	if err := mailbox.put(t.Context(), turncontrol.Available{Authority: header}); err != nil {
		t.Fatal(err)
	}
	if len(mailbox.queue) != 3 {
		t.Fatalf("snapshot burst retained %d entries", len(mailbox.queue))
	}
	first, _ := mailbox.take()
	second, _ := mailbox.take()
	last, _ := mailbox.take()
	if first.Header().Sequence != firstRevision || second.(turncontrol.Grant).PathID != grant.PathID ||
		last.Header().Sequence != header.Sequence {
		t.Fatal("coalescing crossed the grant lifecycle barrier")
	}
}
