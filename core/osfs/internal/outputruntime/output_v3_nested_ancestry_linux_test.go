//go:build linux

package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestLinuxOutputV3DiscardStopsWhenPinnedNestedDirectoryEscapesSessionAncestry(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
	levelOne, err := opened.Session.stagesDir.CreateDirectory("aa", true)
	if err != nil {
		t.Fatal(err)
	}
	levelTwo, err := levelOne.CreateDirectory("nested", true)
	if err != nil {
		t.Fatal(err)
	}
	payloadBytes := []byte("escaped nested payload")
	payload, err := levelTwo.CreateFile("payload", true, int64(len(payloadBytes)))
	if err != nil {
		t.Fatal(err)
	}
	written, err := payload.WriteAt(payloadBytes, 0)
	if err != nil || written != len(payloadBytes) {
		t.Fatalf("write nested payload = (%d, %v)", written, err)
	}
	if err := errors.Join(
		payload.Sync(), levelTwo.Sync(), levelOne.Sync(), opened.Session.stagesDir.Sync(),
		payload.Close(), levelTwo.Close(), levelOne.Close(),
	); err != nil {
		t.Fatal(err)
	}
	v3RecoveryCloseSession(t, opened.Session)

	targetPath := filepath.Join(sessionPath, resumestate.StagesDirectoryName, "aa", "nested")
	escapedPath := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-escaped-nested")
	t.Cleanup(func() { _ = os.RemoveAll(escapedPath) })
	controller := &v3RecoveryNestedEscapeController{target: targetPath, escaped: escapedPath}
	authority.platformFactory = func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return v3RecoveryWrapNestedEscapePlatform(platform, path, controller), nil
	}
	inventory, err := authority.ListResumeState(
		context.Background(), root,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer v3RecoveryCloseInventory(t, inventory)
	summaries := inventory.Summaries()
	if len(summaries) != 1 {
		t.Fatalf("nested ancestry inventory = %+v, want one session", summaries)
	}
	controller.armed.Store(true)
	if _, err := authority.DiscardResumeState(context.Background(), summaries[0].Reference); err == nil ||
		v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
		t.Fatalf("discard after nested ancestry escape error = %v, want session-scoped abort", err)
	}
	if !controller.fired.Load() {
		t.Fatal("nested directory was not moved after its live pin opened")
	}
	actual, err := os.ReadFile(filepath.Join(escapedPath, "payload"))
	if err != nil || !bytes.Equal(actual, payloadBytes) {
		t.Fatalf("escaped nested payload changed: bytes=%q err=%v", actual, err)
	}
}

type v3RecoveryNestedEscapeController struct {
	target  string
	escaped string
	armed   atomic.Bool
	fired   atomic.Bool
}

type v3RecoveryNestedEscapePlatform struct {
	outputcap.Platform
	root outputcap.Directory
}

func v3RecoveryWrapNestedEscapePlatform(
	platform outputcap.Platform,
	rootPath string,
	controller *v3RecoveryNestedEscapeController,
) outputcap.Platform {
	return &v3RecoveryNestedEscapePlatform{
		Platform: platform,
		root:     v3RecoveryWrapNestedEscapeDirectory(platform.Root(), rootPath, controller),
	}
}

func (platform *v3RecoveryNestedEscapePlatform) Root() outputcap.Directory { return platform.root }

func (platform *v3RecoveryNestedEscapePlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryNestedEscapeDirectory)
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return v3RecoveryWrapNestedEscapeDirectory(
				root, decorated.path, decorated.controller,
			)
		},
	)
}

type v3RecoveryNestedEscapeDirectory struct {
	outputcap.Directory
	path       string
	controller *v3RecoveryNestedEscapeController
}

func v3RecoveryWrapNestedEscapeDirectory(
	directory outputcap.Directory,
	path string,
	controller *v3RecoveryNestedEscapeController,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryNestedEscapeDirectory{
		Directory: directory, path: path, controller: controller,
	}
}

func v3RecoveryUnwrapNestedEscapeDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*v3RecoveryNestedEscapeDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func v3RecoveryUnwrapNestedEscapeEntry(entry outputcap.CurrentEntryReference) outputcap.CurrentEntryReference {
	if wrapped, ok := entry.(*v3RecoveryNestedEscapeEntry); ok {
		return wrapped.CurrentEntryReference
	}
	return entry
}

func (directory *v3RecoveryNestedEscapeDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	return v3RecoveryWrapNestedEscapeDirectory(duplicate, directory.path, directory.controller), err
}

func (directory *v3RecoveryNestedEscapeDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(v3RecoveryUnwrapNestedEscapeDirectory(other))
}

func (directory *v3RecoveryNestedEscapeDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	return v3RecoveryWrapNestedEscapeDirectory(
		opened, filepath.Join(directory.path, name), directory.controller,
	), err
}

func (directory *v3RecoveryNestedEscapeDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	return v3RecoveryWrapNestedEscapeDirectory(
		created, filepath.Join(directory.path, name), directory.controller,
	), err
}

func (directory *v3RecoveryNestedEscapeDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapNestedEscapeDirectory(candidate), name,
	)
	return v3RecoveryWrapNestedEscapeDirectory(
		installed, filepath.Join(directory.path, name), directory.controller,
	), err
}

func (directory *v3RecoveryNestedEscapeDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(
		name, v3RecoveryUnwrapNestedEscapeDirectory(expected),
	)
}

func (directory *v3RecoveryNestedEscapeDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	entry, err := directory.Directory.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	return &v3RecoveryNestedEscapeEntry{
		CurrentEntryReference: entry,
		path:                  filepath.Join(directory.path, name),
	}, nil
}

func (directory *v3RecoveryNestedEscapeDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	return directory.Directory.EntryMatches(name, v3RecoveryUnwrapNestedEscapeEntry(expected))
}

func (directory *v3RecoveryNestedEscapeDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	entry, ok := expected.(*v3RecoveryNestedEscapeEntry)
	childPath := ""
	if ok {
		childPath = entry.path
	}
	opened, err := directory.Directory.OpenPinnedDirectory(
		v3RecoveryUnwrapNestedEscapeEntry(expected), private,
	)
	if err != nil {
		return nil, err
	}
	child := v3RecoveryWrapNestedEscapeDirectory(opened, childPath, directory.controller)
	if directory.controller.armed.Load() && childPath == directory.controller.target &&
		directory.controller.fired.CompareAndSwap(false, true) {
		if err := os.Rename(directory.controller.target, directory.controller.escaped); err != nil {
			_ = child.Close()
			return nil, err
		}
	}
	return child, nil
}

func (directory *v3RecoveryNestedEscapeDirectory) RemoveEntry(
	name string,
	expected outputcap.CurrentEntryReference,
) error {
	return directory.Directory.RemoveEntry(name, v3RecoveryUnwrapNestedEscapeEntry(expected))
}

type v3RecoveryNestedEscapeEntry struct {
	outputcap.CurrentEntryReference
	path string
}
