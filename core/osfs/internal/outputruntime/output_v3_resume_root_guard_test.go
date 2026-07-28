package outputruntime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/transfer"
)

var errResumeRootGuardCloseInjected = errors.New("injected resume root guard close failure")

func TestResumeManagementUsesGuardedRootWithoutPrimaryFallback(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	base := v3RecoveryAuthority(t, root, nil)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, base, root, selection)
	v3RecoveryCloseSession(t, opened.Session)

	stats := &resumeRootGuardStats{}
	authority := v3RecoveryAuthority(t, root, nil)
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &resumeRootGuardPlatform{Platform: platform, stats: stats}, nil
	}

	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		_ = inventory.Close()
		t.Fatalf("guarded resume inventory = %+v, want one session", summaries)
	}
	if got := stats.snapshot(); got != (resumeRootGuardSnapshot{acquired: 2, closed: 2}) {
		_ = inventory.Close()
		t.Fatalf("listing root-guard lifecycle = %+v, want two guarded opens and no primary-root access", got)
	}

	settlement, err := authority.DiscardResumeState(context.Background(), summaries[0].Reference)
	if err != nil || settlement.Kind != Discarded {
		_ = inventory.Close()
		t.Fatalf("guarded resume discard = (%+v, %v)", settlement, err)
	}
	if got := stats.snapshot(); got != (resumeRootGuardSnapshot{acquired: 4, closed: 4}) {
		_ = inventory.Close()
		t.Fatalf("list/discard root-guard lifecycle = %+v, want four guarded opens and no primary-root access", got)
	}
	if err := inventory.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestResumeListingReleasesInventoryWhenGuardCleanupFails(t *testing.T) {
	t.Parallel()
	root, authority, pins := v3RecoveryInventoryFixture(t, false)
	baseFactory := authority.platformFactory
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := baseFactory(path, create)
		if err != nil {
			return nil, err
		}
		return &resumeRootGuardClosePlatform{
			Platform: platform,
			closeErr: errResumeRootGuardCloseInjected,
		}, nil
	}

	inventory, err := authority.ListResumeState(context.Background(), root)
	if inventory != nil || !errors.Is(err, errResumeRootGuardCloseInjected) {
		t.Fatalf("listing with guard cleanup failure = (%v, %v), want nil inventory and injected error", inventory, err)
	}
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultRoot ||
		fault.Code() != transfer.OutputFaultStateIO {
		t.Fatalf("guard cleanup failure = %v, want Root/StateIO", err)
	}
	if live := pins.Load(); live != 0 {
		t.Fatalf("guard cleanup failure leaked %d inventory pins", live)
	}
}

func TestResumeDiscardClearsSettlementWhenGuardCleanupFails(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	base := v3RecoveryAuthority(t, root, nil)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, base, root, selection)
	v3RecoveryCloseSession(t, opened.Session)
	inventory, err := base.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("discard cleanup fixture inventory = %+v", summaries)
	}

	discardAuthority := v3RecoveryAuthority(t, root, nil)
	discardAuthority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &resumeRootGuardClosePlatform{
			Platform: platform,
			closeErr: errResumeRootGuardCloseInjected,
		}, nil
	}
	settlement, err := discardAuthority.DiscardResumeState(
		context.Background(), summaries[0].Reference,
	)
	if settlement != (DiscardSettlement{}) || !errors.Is(err, errResumeRootGuardCloseInjected) {
		t.Fatalf("discard with guard cleanup failure = (%+v, %v), want zero settlement and injected error", settlement, err)
	}
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultRoot ||
		fault.Code() != transfer.OutputFaultStateIO {
		t.Fatalf("discard guard cleanup failure = %v, want Root/StateIO", err)
	}

	remaining, listErr := base.ListResumeState(context.Background(), root)
	if listErr != nil {
		t.Fatal(listErr)
	}
	defer v3RecoveryCloseInventory(t, remaining)
	if summaries := remaining.Summaries(); len(summaries) != 0 {
		t.Fatalf("discard adopted before guard cleanup failure left resume state: %+v", summaries)
	}
}

func TestResumeDiscardClearsSettlementWhenRootRevalidationFails(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	base := v3RecoveryAuthority(t, root, nil)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, base, root, selection)
	v3RecoveryCloseSession(t, opened.Session)
	inventory, err := base.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("discard revalidation fixture inventory = %+v", summaries)
	}

	discardAuthority := v3RecoveryAuthority(t, root, nil)
	discardAuthority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return &resumeRootRevalidationMismatchPlatform{Platform: platform}, nil
	}
	settlement, err := discardAuthority.DiscardResumeState(
		context.Background(), summaries[0].Reference,
	)
	if settlement != (DiscardSettlement{}) || !errors.Is(err, outputfault.ErrRootUnsafe) {
		t.Fatalf("discard with root revalidation failure = (%+v, %v), want zero settlement and unsafe root", settlement, err)
	}
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultRoot ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe {
		t.Fatalf("discard root revalidation failure = %v, want Root/NamespaceUnsafe", err)
	}

	remaining, listErr := base.ListResumeState(context.Background(), root)
	if listErr != nil {
		t.Fatal(listErr)
	}
	defer v3RecoveryCloseInventory(t, remaining)
	if summaries := remaining.Summaries(); len(summaries) != 0 {
		t.Fatalf("discard adopted before root revalidation failure left resume state: %+v", summaries)
	}
}

func TestResumePublicRootOperationSeparatesRevalidationFromCleanup(t *testing.T) {
	t.Parallel()
	t.Run("matched", func(t *testing.T) {
		platform := newOutputAncestryTestPlatform(t)
		operation, acquireErr, cleanupErr := acquireResumePublicRootOperation(platform)
		if acquireErr != nil || cleanupErr != nil {
			t.Fatalf("acquire operation = (%v, %v)", acquireErr, cleanupErr)
		}
		revalidateErr, reboundCloseErr, guardCloseErr := operation.finish()
		if err := errors.Join(revalidateErr, reboundCloseErr, guardCloseErr); err != nil {
			t.Fatal(err)
		}
		if platform.guardAcquires != 2 || platform.guardCloses != 2 {
			t.Fatalf("guard lifecycle = (%d, %d), want (2, 2)", platform.guardAcquires, platform.guardCloses)
		}
	})

	t.Run("mismatch", func(t *testing.T) {
		platform := newOutputAncestryTestPlatform(t)
		operation, acquireErr, cleanupErr := acquireResumePublicRootOperation(platform)
		if acquireErr != nil || cleanupErr != nil {
			t.Fatalf("acquire operation = (%v, %v)", acquireErr, cleanupErr)
		}
		platform.root = &outputAncestryTestDirectory{node: platform.nodes["empty"]}
		revalidateErr, reboundCloseErr, guardCloseErr := operation.finish()
		if !errors.Is(revalidateErr, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("root mismatch = %v, want unsafe revalidation", revalidateErr)
		}
		if err := errors.Join(reboundCloseErr, guardCloseErr); err != nil {
			t.Fatalf("root mismatch cleanup = %v", err)
		}
	})

	t.Run("cleanup", func(t *testing.T) {
		platform := newOutputAncestryTestPlatform(t)
		platform.guardCloseErr = errResumeRootGuardCloseInjected
		operation, acquireErr, cleanupErr := acquireResumePublicRootOperation(platform)
		if acquireErr != nil || cleanupErr != nil {
			t.Fatalf("acquire operation = (%v, %v)", acquireErr, cleanupErr)
		}
		var resultErr error
		finishResumePublicRootOperation(operation, "test", &resultErr)
		var fault *transfer.OutputFault
		if !errors.As(resultErr, &fault) || fault.Scope() != transfer.OutputFaultRoot ||
			fault.Code() != transfer.OutputFaultStateIO {
			t.Fatalf("guard close classification = %v, want Root/StateIO", resultErr)
		}
	})
}

type resumeRootGuardStats struct {
	rootCalls atomic.Int64
	acquired  atomic.Int64
	closed    atomic.Int64
}

type resumeRootGuardSnapshot struct {
	rootCalls int64
	acquired  int64
	closed    int64
}

func (stats *resumeRootGuardStats) snapshot() resumeRootGuardSnapshot {
	return resumeRootGuardSnapshot{
		rootCalls: stats.rootCalls.Load(),
		acquired:  stats.acquired.Load(),
		closed:    stats.closed.Load(),
	}
}

type resumeRootGuardPlatform struct {
	outputcap.Platform
	stats *resumeRootGuardStats
}

func (platform *resumeRootGuardPlatform) Root() outputcap.Directory {
	platform.stats.rootCalls.Add(1)
	return platform.Platform.Root()
}

func (platform *resumeRootGuardPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	guard, err := platform.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	platform.stats.acquired.Add(1)
	return &resumeRootGuardCountingGuard{
		PublicOperationGuard: guard,
		stats:                platform.stats,
	}, nil
}

type resumeRootGuardCountingGuard struct {
	outputcap.PublicOperationGuard
	stats *resumeRootGuardStats
}

func (guard *resumeRootGuardCountingGuard) Close() error {
	if guard == nil || guard.PublicOperationGuard == nil {
		return nil
	}
	err := guard.PublicOperationGuard.Close()
	guard.PublicOperationGuard = nil
	guard.stats.closed.Add(1)
	return err
}

type resumeRootGuardClosePlatform struct {
	outputcap.Platform
	closeErr error
}

func (platform *resumeRootGuardClosePlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	guard, err := platform.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	return &resumeRootGuardCloseGuard{
		PublicOperationGuard: guard,
		closeErr:             platform.closeErr,
	}, nil
}

type resumeRootGuardCloseGuard struct {
	outputcap.PublicOperationGuard
	closeErr error
}

func (guard *resumeRootGuardCloseGuard) Close() error {
	if guard == nil || guard.PublicOperationGuard == nil {
		return nil
	}
	err := guard.PublicOperationGuard.Close()
	guard.PublicOperationGuard = nil
	return errors.Join(err, guard.closeErr)
}

type resumeRootRevalidationMismatchPlatform struct {
	outputcap.Platform
	guardAcquires atomic.Int64
}

func (platform *resumeRootRevalidationMismatchPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	guard, err := platform.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if platform.guardAcquires.Add(1) != 1 {
		return guard, nil
	}
	return &resumeRootRevalidationMismatchGuard{
		PublicOperationGuard: guard,
		root: &resumeRootRevalidationMismatchDirectory{
			Directory: guard.Root(),
		},
	}, nil
}

type resumeRootRevalidationMismatchGuard struct {
	outputcap.PublicOperationGuard
	root outputcap.Directory
}

func (guard *resumeRootRevalidationMismatchGuard) Root() outputcap.Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

func (guard *resumeRootRevalidationMismatchGuard) Close() error {
	if guard == nil || guard.PublicOperationGuard == nil {
		return nil
	}
	err := guard.PublicOperationGuard.Close()
	guard.PublicOperationGuard = nil
	guard.root = nil
	return err
}

type resumeRootRevalidationMismatchDirectory struct {
	outputcap.Directory
}

func (*resumeRootRevalidationMismatchDirectory) SameDirectory(outputcap.Directory) (bool, error) {
	return false, nil
}
