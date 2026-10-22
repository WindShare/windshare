package transfer

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type receiptLaneFunction func(context.Context, BlockDemand, func(uint64)) (records.BlockRecord, error)

func (f receiptLaneFunction) FetchBlock(ctx context.Context, demand BlockDemand, received func(uint64)) (records.BlockRecord, error) {
	return f(ctx, demand, received)
}

func admissionSignal(lanes *LaneSet) <-chan struct{} {
	lanes.mu.Lock()
	defer lanes.mu.Unlock()
	return lanes.admissionChanged
}

func TestLaneFragmentsAnnounceOnlyNewReceiveCapacity(t *testing.T) {
	const blockBytes = 1 << 20
	const fragmentBytes = 64 << 10
	descriptor, blocks := benchmarkTransferData(t, blockBytes, 1)
	var elapsed atomic.Int64
	now := func() time.Time { return time.Unix(100, elapsed.Load()) }
	lanes, err := NewLaneSet(LaneSetConfig{
		ProtocolSessionID: transferID[protocolsession.ProtocolSessionID](115),
		Now:               now,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lanes.Close()
	var wakeFragments []int
	if err := lanes.Add(LaneIdentity{ID: 1}, LaneRouteDirect,
		receiptLaneFunction(func(_ context.Context, _ BlockDemand, received func(uint64)) (records.BlockRecord, error) {
			for fragment := 1; fragment <= blockBytes/fragmentBytes; fragment++ {
				before := admissionSignal(lanes)
				elapsed.Add(int64(500 * time.Millisecond))
				received(fragmentBytes)
				if admissionSignal(lanes) != before {
					wakeFragments = append(wakeFragments, fragment)
				}
			}
			return blocks[0], nil
		})); err != nil {
		t.Fatal(err)
	}
	demand := validDemand(t, descriptor, 0)
	if _, err := lanes.fetch(context.Background(), blockRequest{demand: demand, validate: validateTransferRecord(demand)}); err != nil {
		t.Fatal(err)
	}
	if len(wakeFragments) != 1 || wakeFragments[0] != 15 {
		t.Fatalf("fragment notifications = %v; want only fragment 15, before block completion", wakeFragments)
	}
}

func TestLaneWaiterResumesOnReceiptOrCancellation(t *testing.T) {
	for _, trigger := range []string{"receipt", "cancellation"} {
		t.Run(trigger, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				descriptor := transferDescriptor(t, 2)
				lanes := newBoundaryLaneSet(t, 116)
				firstContext, cancelFirst := context.WithCancel(context.Background())
				defer cancelFirst()
				firstReceipt := make(chan func(uint64), 1)
				finishFirst := make(chan struct{})
				defer close(finishFirst)
				if err := lanes.Add(LaneIdentity{ID: 1}, LaneRouteDirect,
					receiptLaneFunction(func(ctx context.Context, input BlockDemand, received func(uint64)) (records.BlockRecord, error) {
						if input.Index == 0 {
							firstReceipt <- received
							select {
							case <-finishFirst:
							case <-ctx.Done():
								return records.BlockRecord{}, ctx.Err()
							}
						}
						return transferRecord(t, descriptor, input.Index), nil
					})); err != nil {
					t.Fatal(err)
				}
				firstDone := make(chan error, 1)
				first := validDemand(t, descriptor, 0)
				go func() {
					_, err := lanes.fetch(firstContext, blockRequest{demand: first, validate: validateTransferRecord(first)})
					firstDone <- err
				}()
				received := <-firstReceipt
				nextDone := make(chan error, 1)
				next := validDemand(t, descriptor, 1)
				go func() {
					_, err := lanes.fetch(context.Background(), blockRequest{demand: next, validate: validateTransferRecord(next)})
					nextDone <- err
				}()
				synctest.Wait()
				select {
				case err := <-nextDone:
					t.Fatalf("cold lane admitted waiting work: %v", err)
				default:
				}
				if trigger == "receipt" {
					time.Sleep(time.Millisecond)
					received(blockDemandBytes(first) / 2)
				} else {
					cancelFirst()
				}
				synctest.Wait()
				select {
				case err := <-nextDone:
					if err != nil {
						t.Fatal(err)
					}
				default:
					t.Fatal("waiting work missed the admission notification")
				}
				if trigger == "receipt" {
					select {
					case err := <-firstDone:
						t.Fatalf("receipt required first block completion: %v", err)
					default:
					}
				}
				cancelFirst()
				if err := <-firstDone; err == nil {
					t.Fatal("cancelled block succeeded")
				}
			})
		})
	}
}
