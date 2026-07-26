package osfs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3ResumeStateInventoryOwnership(t *testing.T) {
	t.Run("repeated-list-close", func(t *testing.T) {
		root, authority, pins := v3RecoveryInventoryFixture(t, false)
		for iteration := 0; iteration < 3; iteration++ {
			inventory, err := authority.listResumeState(
				context.Background(), FilesystemResumeRoot{RootPath: root},
			)
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
		inventory, err := authority.listResumeState(ctx, FilesystemResumeRoot{RootPath: root})
		if inventory != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("partial inventory = (%v, %v), want nil and context cancellation", inventory, err)
		}
		if actual := pins.Load(); actual != 0 {
			t.Fatalf("partial inventory error leaked %d session pins", actual)
		}
	})

	t.Run("copied-reference-second-use", func(t *testing.T) {
		root, authority, _ := v3RecoveryInventoryFixture(t, false)
		inventory, err := authority.listResumeState(
			context.Background(), FilesystemResumeRoot{RootPath: root},
		)
		if err != nil {
			t.Fatal(err)
		}
		defer v3RecoveryCloseInventory(t, inventory)
		summaries := inventory.Summaries()
		first, copied := summaries[0].Reference, summaries[0].Reference
		settlement, err := authority.discardResumeState(context.Background(), first)
		if err != nil || settlement.Kind != Discarded {
			t.Fatalf("first discard = (%+v, %v)", settlement, err)
		}
		if _, err := authority.discardResumeState(context.Background(), copied); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("copied reference second use error = %v, want invalid output binding", err)
		}
	})

	t.Run("close-idempotent", func(t *testing.T) {
		root, authority, pins := v3RecoveryInventoryFixture(t, false)
		inventory, err := authority.listResumeState(
			context.Background(), FilesystemResumeRoot{RootPath: root},
		)
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
		if _, err := authority.discardResumeState(context.Background(), reference); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("discard through closed inventory error = %v, want invalid output binding", err)
		}
	})

	t.Run("discard-then-close-idempotent", func(t *testing.T) {
		root, authority, pins := v3RecoveryInventoryFixture(t, false)
		inventory, err := authority.listResumeState(
			context.Background(), FilesystemResumeRoot{RootPath: root},
		)
		if err != nil {
			t.Fatal(err)
		}
		reference := inventory.Summaries()[0].Reference
		settlement, err := authority.discardResumeState(context.Background(), reference)
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
}

func v3RecoveryInventoryFixture(
	t *testing.T,
	secondIntent bool,
) (string, *FilesystemOutputAuthority, *atomic.Int64) {
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
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
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
	outputV3Platform
	root outputV3Directory
}

func v3RecoveryWrapInventoryPlatform(platform outputV3Platform, pins *atomic.Int64) outputV3Platform {
	return &v3RecoveryInventoryPlatform{
		outputV3Platform: platform,
		root:             v3RecoveryWrapInventoryDirectory(platform.Root(), pins),
	}
}

func (platform *v3RecoveryInventoryPlatform) Root() outputV3Directory { return platform.root }

func (platform *v3RecoveryInventoryPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryInventoryDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return v3RecoveryWrapInventoryDirectory(root, decorated.pins)
		},
	)
}

type v3RecoveryInventoryDirectory struct {
	outputV3Directory
	pins *atomic.Int64
}

func v3RecoveryWrapInventoryDirectory(
	directory outputV3Directory,
	pins *atomic.Int64,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryInventoryDirectory{outputV3Directory: directory, pins: pins}
}

func v3RecoveryUnwrapInventoryDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*v3RecoveryInventoryDirectory); ok {
		return wrapped.outputV3Directory
	}
	return directory
}

func v3RecoveryUnwrapInventoryEntry(entry outputV3EntryRef) outputV3EntryRef {
	if wrapped, ok := entry.(*v3RecoveryInventoryEntry); ok {
		return wrapped.outputV3EntryRef
	}
	return entry
}

func (directory *v3RecoveryInventoryDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	return v3RecoveryWrapInventoryDirectory(duplicate, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return directory.outputV3Directory.SameDirectory(v3RecoveryUnwrapInventoryDirectory(other))
}

func (directory *v3RecoveryInventoryDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	return v3RecoveryWrapInventoryDirectory(opened, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	return v3RecoveryWrapInventoryDirectory(created, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	installed, err := directory.outputV3Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapInventoryDirectory(candidate), name,
	)
	return v3RecoveryWrapInventoryDirectory(installed, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	return directory.outputV3Directory.RemoveDirectory(
		name, v3RecoveryUnwrapInventoryDirectory(expected),
	)
}

func (directory *v3RecoveryInventoryDirectory) OpenEntry(name string) (outputV3EntryRef, error) {
	entry, err := directory.outputV3Directory.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	if _, parseErr := resumestate.ParseSessionDirectoryName(name); parseErr != nil {
		return entry, nil
	}
	directory.pins.Add(1)
	return &v3RecoveryInventoryEntry{outputV3EntryRef: entry, pins: directory.pins}, nil
}

func (directory *v3RecoveryInventoryDirectory) EntryMatches(
	name string,
	expected outputV3EntryRef,
) (bool, error) {
	return directory.outputV3Directory.EntryMatches(name, v3RecoveryUnwrapInventoryEntry(expected))
}

func (directory *v3RecoveryInventoryDirectory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenPinnedDirectory(
		v3RecoveryUnwrapInventoryEntry(expected), private,
	)
	return v3RecoveryWrapInventoryDirectory(opened, directory.pins), err
}

func (directory *v3RecoveryInventoryDirectory) RemoveEntry(
	name string,
	expected outputV3EntryRef,
) error {
	return directory.outputV3Directory.RemoveEntry(name, v3RecoveryUnwrapInventoryEntry(expected))
}

type v3RecoveryInventoryEntry struct {
	outputV3EntryRef
	pins *atomic.Int64
	once sync.Once
	err  error
}

func (entry *v3RecoveryInventoryEntry) Close() error {
	entry.once.Do(func() {
		entry.err = entry.outputV3EntryRef.Close()
		entry.pins.Add(-1)
	})
	return entry.err
}
