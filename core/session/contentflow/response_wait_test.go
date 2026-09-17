package contentflow

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestBlockResponseWaitAllowsProductivePredecessors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var queue BlockResponseQueue
		earlier := queue.Begin(t.Context(), FragmentInactivityTimeout)
		defer earlier.Close()
		waiting := queue.Begin(t.Context(), FragmentInactivityTimeout)
		defer waiting.Close()
		for range 4 {
			time.Sleep(10 * time.Second)
			earlier.Progress()
			synctest.Wait()
			if waiting.Context().Err() != nil {
				t.Fatal("productive queue was mistaken for fragment inactivity")
			}
		}
		earlier.Close()
		waiting.Progress()
		for range 4 {
			time.Sleep(10 * time.Second)
			waiting.Progress()
			synctest.Wait()
			if waiting.Context().Err() != nil {
				t.Fatal("active assembly was given a total-duration deadline")
			}
		}
		waiting.Close()
		waiting.Progress()
		if queue.pending.Len() != 0 {
			t.Fatal("completed response waits retained queue entries")
		}
	})
}

func TestBlockResponseWaitDoesNotBorrowUnrelatedProgress(t *testing.T) {
	for _, scenario := range []string{"later request", "another lane", "active assembly"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var queue, another BlockResponseQueue
				otherQueue := &queue
				if scenario == "another lane" {
					otherQueue = &another
				}
				earlier := otherQueue.Begin(t.Context(), FragmentInactivityTimeout)
				defer earlier.Close()
				wait := queue.Begin(t.Context(), FragmentInactivityTimeout)
				defer wait.Close()
				productive := earlier
				phase, cause := BlockAwaitingFirstFragment, ErrBlockResponseInactivity
				if scenario == "later request" {
					productive, wait = wait, earlier
				}
				if scenario == "active assembly" {
					wait.Progress()
					phase, cause = BlockReceivingFragments, ErrFragmentInactivity
				}
				time.Sleep(10 * time.Second)
				productive.Progress()
				time.Sleep(5 * time.Second)
				synctest.Wait()
				var failure *BlockWaitTimeout
				if !errors.As(context.Cause(wait.Context()), &failure) || !errors.Is(failure, cause) ||
					failure.Phase != phase || failure.QueueProgress != 0 {
					t.Fatalf("unrelated progress prolonged the wait: %v", context.Cause(wait.Context()))
				}
				if failure.Waited != FragmentInactivityTimeout || failure.Error() != cause.Error() {
					t.Fatalf("timeout lost its decision: %+v", failure)
				}
			})
		})
	}
}

func TestBlockResponseWaitExpiresAfterQueueStopsProgressing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var queue BlockResponseQueue
		earlier := queue.Begin(t.Context(), FragmentInactivityTimeout)
		wait := queue.Begin(t.Context(), FragmentInactivityTimeout)
		defer wait.Close()
		time.Sleep(10 * time.Second)
		earlier.Progress()
		earlier.Close()
		time.Sleep(15 * time.Second)
		synctest.Wait()
		var failure *BlockWaitTimeout
		if !errors.As(context.Cause(wait.Context()), &failure) ||
			failure.Phase != BlockAwaitingFirstFragment || failure.QueueProgress != 1 ||
			failure.Waited != 25*time.Second {
			t.Fatalf("queue allowance outlived its progress: %v", context.Cause(wait.Context()))
		}
		earlier.Progress()
		if queue.pending.Len() != 1 {
			t.Fatal("retired predecessor rejoined the queue")
		}
	})
}

func TestBlockResponseWaitHonorsCancellationAndDoesNotReviveExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var queue BlockResponseQueue
		ctx, cancel := context.WithCancel(t.Context())
		wait := queue.Begin(ctx, FragmentInactivityTimeout)
		cancel()
		wait.Progress()
		time.Sleep(FragmentInactivityTimeout)
		synctest.Wait()
		if !errors.Is(context.Cause(wait.Context()), context.Canceled) {
			t.Fatal("caller cancellation was overwritten")
		}
		wait.Close()
		wait.Close()
		earlier := queue.Begin(t.Context(), FragmentInactivityTimeout)
		defer earlier.Close()
		expired := queue.Begin(t.Context(), FragmentInactivityTimeout)
		defer expired.Close()
		time.Sleep(FragmentInactivityTimeout)
		synctest.Wait()
		expired.Progress()
		earlier.Progress()
		if !errors.Is(context.Cause(expired.Context()), ErrBlockResponseInactivity) {
			t.Fatal("an expired request was revived")
		}
	})
}
