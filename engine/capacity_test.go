package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/revisioncapacity"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/contentflow"
)

var errCacheFull = errors.New("object exceeds remaining aggregate cache capacity")

func TestConcurrentSharesEnforceAggregateCapacityAndReleaseIndependently(t *testing.T) {
	for _, resource := range []string{"catalog", "revision", "cache"} {
		t.Run(resource, func(t *testing.T) {
			config := Config{}
			config.CatalogLimits = catalog.DefaultProcessBudgetLimits()
			config.CatalogLimits.MemoryBytes = 10
			config.RevisionCapacity = revisioncapacity.DefaultProcessConfig()
			config.RevisionCapacity.Limits = revisioncapacity.CapacityLimits{StableHandles: 1, ActiveLeases: 1}
			config.CacheBytes = 10
			type acquired struct {
				config liveshare.SenderConfig
				err    error
			}
			reached := make(chan acquired, 3)
			var sequence atomic.Uint32
			config.Share.Prepare = func(ctx context.Context, sender liveshare.SenderConfig) (PreparedShare, error) {
				release, err := reserveTaskResource(resource, sender, byte(sequence.Add(1)))
				reached <- acquired{config: sender, err: err}
				if err != nil {
					return nil, err
				}
				<-ctx.Done()
				release()
				return nil, ctx.Err()
			}
			application := newTestEngine(t, config)
			first, err := application.StartShare(context.Background(), ShareRequest{})
			if err != nil {
				t.Fatal(err)
			}
			one := <-reached
			if one.err != nil {
				t.Fatal(one.err)
			}
			second, err := application.StartShare(context.Background(), ShareRequest{})
			if err != nil {
				t.Fatal(err)
			}
			two := <-reached
			if two.err == nil {
				t.Fatal("second task exceeded aggregate capacity")
			}
			if one.config.RevisionCapacity != two.config.RevisionCapacity || one.config.CatalogBudget != two.config.CatalogBudget || one.config.CacheBudget != two.config.CacheBudget {
				t.Fatal("shares received different process authorities")
			}
			failed, err := second.Wait(context.Background())
			if err != nil || failed.Err == nil {
				t.Fatalf("failed share = %+v, %v", failed, err)
			}
			if first.State() != TaskRunning {
				t.Fatal("one share's failure stopped its sibling")
			}
			first.StopShare()
			stopped, err := first.Wait(context.Background())
			if err != nil || stopped.StopReason != ShareStopped || stopped.Outcome != OutcomeStopped {
				t.Fatalf("explicit stop = %+v, %v", stopped, err)
			}
			third, err := application.StartShare(context.Background(), ShareRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if next := <-reached; next.err != nil {
				t.Fatalf("released capacity was not reusable: %v", next.err)
			}
			third.Cancel()
			if cancelled, err := third.Wait(context.Background()); err != nil || cancelled.StopReason != Cancelled || cancelled.Outcome != OutcomeCancelled {
				t.Fatalf("cancel = %+v, %v", cancelled, err)
			}
			if used := application.catalog.Snapshot().Used; used != (catalog.ResourceUsage{}) {
				t.Fatalf("catalog leak = %+v", used)
			}
			if used := application.cache.Used(); used != 0 {
				t.Fatalf("cache leak = %d", used)
			}
		})
	}
}

func reserveTaskResource(resource string, config liveshare.SenderConfig, identity byte) (func(), error) {
	switch resource {
	case "catalog":
		share, err := catalog.NewBudgetAccount(fmt.Sprintf("share-%d", identity), catalog.DefaultShareBudgetLimits())
		if err != nil {
			return nil, err
		}
		session, err := catalog.NewBudgetAccount(fmt.Sprintf("session-%d", identity), catalog.DefaultSessionBudgetLimits())
		if err != nil {
			return nil, err
		}
		reservation, err := catalog.ReserveHierarchy(catalog.BudgetHierarchy{Process: config.CatalogBudget, Share: share, Session: session}, catalog.ResourceUsage{MemoryBytes: 6})
		if err != nil {
			return nil, err
		}
		return reservation.Release, nil
	case "cache":
		shareID := catalog.ShareInstance{identity}
		cache, err := contentflow.NewSharedBlockCache(shareID, 10, config.CacheBudget)
		if err != nil {
			return nil, err
		}
		key := contentflow.BlockCacheKey{ShareInstance: shareID, FileID: catalog.FileID{1}, FileRevision: content.FileRevision{1}}
		_, err = cache.Get(context.Background(), key, func(context.Context) ([]byte, error) { return make([]byte, 6), nil })
		if err != nil {
			cache.Close()
			return nil, err
		}
		if cache.UsedBytes() == 0 {
			cache.Close()
			return nil, errCacheFull
		}
		return cache.Close, nil
	case "revision":
		store, err := config.RevisionCapacity.RegisterStore(revisioncapacity.StoreConfig{
			StoreID: revisioncapacity.StoreID(fmt.Sprintf("store-%d", identity)),
			ShareID: revisioncapacity.ShareID(fmt.Sprintf("share-%d", identity)),
			Limits:  revisioncapacity.DefaultShareLimits(),
		}, noReclaim{})
		if err != nil {
			return nil, err
		}
		session, err := store.RegisterSession(revisioncapacity.SessionConfig{SessionID: "session", Limits: revisioncapacity.DefaultSessionLimits()})
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		grant, err := store.Admit(context.Background(), revisioncapacity.AdmissionRequest{Kind: revisioncapacity.AdmissionNewRevision, RevisionID: "revision", Session: session})
		if err != nil {
			_ = session.Close()
			_ = store.Close()
			return nil, err
		}
		charges, err := grant.Commit()
		if err != nil {
			_ = grant.Abort()
			_ = session.Close()
			_ = store.Close()
			return nil, err
		}
		return func() { _ = charges.Release(); _ = session.Close(); _ = store.Close() }, nil
	default:
		return nil, errors.New("unknown resource")
	}
}

type noReclaim struct{}

func (noReclaim) ReclaimIdle(_ context.Context, claim revisioncapacity.ReclaimClaim) revisioncapacity.ReclaimResult {
	return revisioncapacity.ReclaimDeclined(claim)
}
