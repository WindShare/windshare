package outputruntime

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ResumeStateInventoryOwnership(t *testing.T) {
	t.Parallel()
	t.Run("repeated-list-close", func(t *testing.T) {
		root, authority, pins := v3RecoveryInventoryFixture(t, false)
		for iteration := range 3 {
			inventory, err := authority.ListResumeState(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if summaries := inventory.Summaries(); len(summaries) != 1 {
				t.Fatalf("inventory %d summaries = %+v, want one session", iteration, summaries)
			}
			if actual := pins.Load(); actual != 1 {
				t.Fatalf("inventory %d live session pins = %d, want 1", iteration, actual)
			}
			if err := inventory.Close(); err != nil {
				t.Fatal(err)
			}
			if actual := pins.Load(); actual != 0 {
				t.Fatalf("inventory %d leaked %d session pins", iteration, actual)
			}
		}
	})

	t.Run("partial-error-releases-pins", func(t *testing.T) {
		root, authority, pins := v3RecoveryInventoryFixture(t, true)
		ctx := newV3RecoveryCancelAfterErrCalls(2)
		inventory, err := authority.ListResumeState(ctx, root)
		if inventory != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("partial inventory = (%v, %v), want nil and context cancellation", inventory, err)
		}
		if actual := pins.Load(); actual != 0 {
			t.Fatalf("partial inventory error leaked %d session pins", actual)
		}
	})

	t.Run("copied-reference-second-use", func(t *testing.T) {
		root, authority, _ := v3RecoveryInventoryFixture(t, false)
		inventory, err := authority.ListResumeState(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		defer v3RecoveryCloseInventory(t, inventory)
		summaries := inventory.Summaries()
		first, copied := summaries[0].Reference, summaries[0].Reference
		settlement, err := authority.DiscardResumeState(context.Background(), first)
		if err != nil || settlement.Kind != Discarded {
			t.Fatalf("first discard = (%+v, %v)", settlement, err)
		}
		if _, err := authority.DiscardResumeState(context.Background(), copied); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("copied reference second use error = %v, want invalid output binding", err)
		}
	})

	t.Run("close-idempotent", func(t *testing.T) {
		root, authority, pins := v3RecoveryInventoryFixture(t, false)
		inventory, err := authority.ListResumeState(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		reference := inventory.Summaries()[0].Reference
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
		if err := inventory.Close(); err != nil {
			t.Fatalf("second inventory close: %v", err)
		}
		if summaries := inventory.Summaries(); summaries != nil {
			t.Fatalf("closed inventory summaries = %+v, want nil", summaries)
		}
		if actual := pins.Load(); actual != 0 {
			t.Fatalf("idempotent close leaked %d session pins", actual)
		}
		if _, err := authority.DiscardResumeState(context.Background(), reference); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("discard through closed inventory error = %v, want invalid output binding", err)
		}
	})

	t.Run("discard-then-close-idempotent", func(t *testing.T) {
		root, authority, pins := v3RecoveryInventoryFixture(t, false)
		inventory, err := authority.ListResumeState(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		reference := inventory.Summaries()[0].Reference
		settlement, err := authority.DiscardResumeState(context.Background(), reference)
		if err != nil || settlement.Kind != Discarded {
			t.Fatalf("discard before inventory close = (%+v, %v)", settlement, err)
		}
		if err := inventory.Close(); err != nil {
			t.Fatal(err)
		}
		if err := inventory.Close(); err != nil {
			t.Fatalf("second close after discard: %v", err)
		}
		if actual := pins.Load(); actual != 0 {
			t.Fatalf("discard then close leaked %d session pins", actual)
		}
	})

	t.Run("discard-release-error-zeroes-settlement", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		authority, inventory, summary, journalPath := runtimeInventoryListedLegacyJournal(t, root)
		defer v3RecoveryCloseInventory(t, inventory)

		inventory.mu.Lock()
		item, found := inventory.items[summary.Reference.itemID]
		inventory.mu.Unlock()
		if !found || item.authority.legacyRoot == nil {
			t.Fatal("listed legacy journal retained no root authority")
		}
		pin := item.authority.legacyRoot
		pin.mu.Lock()
		fixedRoot := pin.directory
		if fixedRoot != nil {
			pin.directory = &v3RecoveryCloseFaultDirectory{
				Directory: fixedRoot,
				closeErr:  errResumeStateAuthorityReleaseInjected,
			}
		}
		pin.mu.Unlock()
		if fixedRoot == nil {
			t.Fatal("listed legacy journal root authority was already closed")
		}

		settlement, err := authority.DiscardResumeState(context.Background(), summary.Reference)
		if settlement != (DiscardSettlement{}) || !errors.Is(err, errResumeStateAuthorityReleaseInjected) {
			t.Fatalf("discard with authority-release failure = (%+v, %v), want zero settlement and release error",
				settlement, err)
		}
		if _, statErr := os.Stat(journalPath); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("legacy journal remained after settled discard: %v", statErr)
		}
	})
}

func v3RecoveryInventoryFixture(
	t *testing.T,
	secondIntent bool,
) (string, *Authority, *atomic.Int64) {
	t.Helper()
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	selection := v3RecoverySelection(t, false, 0)
	opened := v3RecoveryOpen(t, authority, root, selection)
	v3RecoveryCloseSession(t, opened.Session)
	if secondIntent {
		other := v3RecoverySelection(t, true, 1)
		otherOpen := v3RecoveryOpen(t, authority, root, other)
		v3RecoveryCloseSession(t, otherOpen.Session)
	}
	pins := &atomic.Int64{}
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return v3RecoveryWrapInventoryPlatform(platform, pins), nil
	}
	return root, authority, pins
}

type v3RecoveryCancelAfterErrCalls struct {
	allow int64
	calls atomic.Int64
	done  chan struct{}
	once  sync.Once
}

func newV3RecoveryCancelAfterErrCalls(allow int64) *v3RecoveryCancelAfterErrCalls {
	return &v3RecoveryCancelAfterErrCalls{allow: allow, done: make(chan struct{})}
}

func (ctx *v3RecoveryCancelAfterErrCalls) Deadline() (time.Time, bool) { return time.Time{}, false }
func (ctx *v3RecoveryCancelAfterErrCalls) Done() <-chan struct{}       { return ctx.done }
func (ctx *v3RecoveryCancelAfterErrCalls) Value(any) any               { return nil }
func (ctx *v3RecoveryCancelAfterErrCalls) Err() error {
	if ctx.calls.Add(1) <= ctx.allow {
		return nil
	}
	ctx.once.Do(func() { close(ctx.done) })
	return context.Canceled
}

type v3RecoveryInventoryPlatform struct {
	outputcap.Platform
	root outputcap.Directory
}

func v3RecoveryWrapInventoryPlatform(platform outputcap.Platform, pins *atomic.Int64) outputcap.Platform {
	return &v3RecoveryInventoryPlatform{
		Platform: platform,
		root:     v3RecoveryWrapInventoryDirectory(platform.Root(), pins),
	}
}

func (platform *v3RecoveryInventoryPlatform) Root() outputcap.Directory { return platform.root }

func (platform *v3RecoveryInventoryPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryInventoryDirectory)
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return v3RecoveryWrapInventoryDirectory(root, decorated.pins)
		},
	)
}

type v3RecoveryInventoryDirectory struct {
	outputcap.Directory
	pins *atomic.Int64
}

func v3RecoveryWrapInventoryDirectory(
	directory outputcap.Directory,
	pins *atomic.Int64,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryInventoryDirectory{Directory: directory, pins: pins}
}

func v3RecoveryUnwrapInventoryDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*v3RecoveryInventoryDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func v3RecoveryUnwrapInventoryEntry(entry outputcap.CurrentEntryReference) outputcap.CurrentEntryReference {
	if wrapped, ok := entry.(*v3RecoveryInventoryEntry); ok {
		return wrapped.CurrentEntryReference
	}
	return entry
}

func (directory *v3RecoveryInventoryDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	return v3RecoveryWrapInventoryDirectory(duplicate, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(v3RecoveryUnwrapInventoryDirectory(other))
}

func (directory *v3RecoveryInventoryDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	return v3RecoveryWrapInventoryDirectory(opened, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	return v3RecoveryWrapInventoryDirectory(created, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapInventoryDirectory(candidate), name,
	)
	return v3RecoveryWrapInventoryDirectory(installed, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(
		name, v3RecoveryUnwrapInventoryDirectory(expected),
	)
}

func (directory *v3RecoveryInventoryDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	entry, err := directory.Directory.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	if _, parseErr := resumestate.ParseSessionDirectoryName(name); parseErr != nil {
		return entry, nil
	}
	directory.pins.Add(1)
	return &v3RecoveryInventoryEntry{CurrentEntryReference: entry, pins: directory.pins}, nil
}

func (directory *v3RecoveryInventoryDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	return directory.Directory.EntryMatches(name, v3RecoveryUnwrapInventoryEntry(expected))
}

func (directory *v3RecoveryInventoryDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenPinnedDirectory(
		v3RecoveryUnwrapInventoryEntry(expected), private,
	)
	return v3RecoveryWrapInventoryDirectory(opened, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) RemoveEntry(
	name string,
	expected outputcap.CurrentEntryReference,
) error {
	return directory.Directory.RemoveEntry(name, v3RecoveryUnwrapInventoryEntry(expected))
}

type v3RecoveryInventoryEntry struct {
	outputcap.CurrentEntryReference
	pins *atomic.Int64
	once sync.Once
	err  error
}

var errResumeStateAuthorityReleaseInjected = errors.New("injected resume-state authority release failure")

type v3RecoveryCloseFaultDirectory struct {
	outputcap.Directory
	closeErr error
}

func (directory *v3RecoveryCloseFaultDirectory) Close() error {
	return errors.Join(directory.Directory.Close(), directory.closeErr)
}

func (entry *v3RecoveryInventoryEntry) Close() error {
	entry.once.Do(func() {
		entry.err = entry.CurrentEntryReference.Close()
		entry.pins.Add(-1)
	})
	return entry.err
}

func runtimeInventoryListedLegacyJournal(
	t *testing.T,
	root string,
) (*Authority, *ResumeStateInventory, ResumeStateSummary, string) {
	t.Helper()
	journalName := legacyOutputStatePrefix +
		strings.Repeat("11", transfer.OutputSessionIdentityBytes) + legacyOutputJournalSuffix
	journalPath := root + string(os.PathSeparator) + journalName
	if err := os.WriteFile(journalPath, []byte("runtime inventory journal"), 0o600); err != nil {
		t.Fatal(err)
	}
	authority := v3RecoveryAuthority(t, root, nil)
	inventory, err := authority.ListResumeState(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	summaries := inventory.Summaries()
	if len(summaries) != 1 || summaries[0].Reference.legacyName != journalName {
		_ = inventory.Close()
		t.Fatalf("list removable legacy journal = %+v", summaries)
	}
	return authority, inventory, summaries[0], journalPath
}
