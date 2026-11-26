package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/engine/relayprofile"
)

var errCacheFull = errors.New("object exceeds remaining aggregate cache capacity")

func TestConcurrentSharesEnforceAggregateCapacityAndReleaseIndependently(t *testing.T) {
	for _, resource := range []string{"catalog", "revision", "cache"} {
		t.Run(resource, func(t *testing.T) {
			config := Config{}
			config.CatalogLimits = catalog.DefaultProcessBudgetLimits()
			config.CatalogLimits.MemoryBytes = 10
			config.RevisionCapacity = content.DefaultHandlePoolConfig()
			config.RevisionCapacity.Limits = content.CapacityLimits{StableHandles: 1, ActiveLeases: 1}
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
			first, err := application.StartShare(context.Background(), ShareRequest{RelayIntent: relayprofile.UseExplicit(
				[]relayprofile.
					Candidate{{PublicURL: "https://relay.example"}}),
			})
			if err != nil {
				t.Fatal(err)
			}
			one := <-reached
			if one.err != nil {
				t.Fatal(one.err)
			}
			second, err := application.StartShare(context.Background(), ShareRequest{RelayIntent: relayprofile.UseExplicit(
				[]relayprofile.
					Candidate{{PublicURL: "https://relay.example"}}),
			})
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
			third, err := application.StartShare(context.Background(), ShareRequest{RelayIntent: relayprofile.UseExplicit(
				[]relayprofile.
					Candidate{{PublicURL: "https://relay.example"}}),
			})
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
		store, fileID, err := newEngineTestRevisionStore(config.RevisionCapacity, identity, content.DefaultShareCapacityLimits(), nil)
		if err != nil {
			return nil, err
		}
		session, err := store.RegisterSession(content.SessionConfig{SessionID: "session", Limits: content.DefaultSessionCapacityLimits()})
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		lease, err := session.OpenRevision(context.Background(), fileID)
		if err != nil {
			_ = session.Close()
			_ = store.Close()
			return nil, err
		}
		return func() {
			_ = session.EndLease(lease.ID(), content.LeaseRelinquished)
			_ = session.Close()
			_ = store.Close()
		}, nil
	default:
		return nil, errors.New("unknown resource")
	}
}

type engineTestCatalog map[catalog.NodeID]catalog.NodeRecord

func (c engineTestCatalog) Node(_ context.Context, id catalog.NodeID) (catalog.NodeRecord, bool, error) {
	record, ok := c[id]
	return record, ok, nil
}

type engineTestSource struct{}

func (engineTestSource) OpenStable(context.Context, catalog.NodeRecord) (content.StableFile, error) {
	return engineTestStableFile{}, nil
}

type engineTestStableFile struct{}

func (engineTestStableFile) ExactSize() uint64                                   { return 1 }
func (engineTestStableFile) ModifiedTime() catalog.ModifiedTime                  { return catalog.ModifiedTime{} }
func (engineTestStableFile) Verify(context.Context) error                        { return nil }
func (engineTestStableFile) ReadAt(context.Context, []byte, uint64) (int, error) { return 0, io.EOF }
func (engineTestStableFile) Close() error                                        { return nil }

func newEngineTestRevisionStore(
	pool *content.HandlePool,
	identity byte,
	limits content.CapacityLimits,
	tracer content.CapacityTracer,
) (*content.RevisionStore, catalog.FileID, error) {
	share := catalog.ShareInstance{identity}
	parent := catalog.DirectoryID{identity, 1}
	fileID := catalog.FileID{identity, 2}
	ref, err := catalog.NewSourceReference(fmt.Appendf(nil, "object:file-%d", identity))
	if err != nil {
		return nil, catalog.FileID{}, err
	}
	srcID, err := catalog.NewSourceIdentity(fmt.Appendf(nil, "src-%d", identity))
	if err != nil {
		return nil, catalog.FileID{}, err
	}
	ver, err := catalog.NewVersionCandidate(fmt.Appendf(nil, "ver-%d", identity))
	if err != nil {
		return nil, catalog.FileID{}, err
	}
	record, err := catalog.NewFileNodeRecord(fileID, parent, "file.bin", ref, srcID, ver, 1, catalog.ModifiedTime{})
	if err != nil {
		return nil, catalog.FileID{}, err
	}
	store, err := content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: share, ChunkSize: catalog.MinChunkSize,
		Catalog:    engineTestCatalog{fileID.NodeID(): record},
		Source:     engineTestSource{},
		HandlePool: pool, ShareLimits: limits, CapacityTracer: tracer,
	})
	return store, fileID, err
}
