//go:build linux

package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestLinuxOutputV3DiscardStopsWhenPinnedNestedDirectoryEscapesSessionAncestry(t *testing.T) {
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
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		return v3RecoveryWrapNestedEscapePlatform(platform, path, controller), nil
	}
	inventory, err := authority.listResumeState(
		context.Background(), FilesystemResumeRoot{RootPath: root},
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
	if _, err := authority.discardResumeState(context.Background(), summaries[0].Reference); err == nil ||
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
	outputV3Platform
	root outputV3Directory
}

func v3RecoveryWrapNestedEscapePlatform(
	platform outputV3Platform,
	rootPath string,
	controller *v3RecoveryNestedEscapeController,
) outputV3Platform {
	return &v3RecoveryNestedEscapePlatform{
		outputV3Platform: platform,
		root:             v3RecoveryWrapNestedEscapeDirectory(platform.Root(), rootPath, controller),
	}
}

func (platform *v3RecoveryNestedEscapePlatform) Root() outputV3Directory { return platform.root }

func (platform *v3RecoveryNestedEscapePlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryNestedEscapeDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return v3RecoveryWrapNestedEscapeDirectory(
				root, decorated.path, decorated.controller,
			)
		},
	)
}

type v3RecoveryNestedEscapeDirectory struct {
	outputV3Directory
	path       string
	controller *v3RecoveryNestedEscapeController
}

func v3RecoveryWrapNestedEscapeDirectory(
	directory outputV3Directory,
	path string,
	controller *v3RecoveryNestedEscapeController,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryNestedEscapeDirectory{
		outputV3Directory: directory, path: path, controller: controller,
	}
}

func v3RecoveryUnwrapNestedEscapeDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*v3RecoveryNestedEscapeDirectory); ok {
		return wrapped.outputV3Directory
	}
	return directory
}

func v3RecoveryUnwrapNestedEscapeEntry(entry outputV3EntryRef) outputV3EntryRef {
	if wrapped, ok := entry.(*v3RecoveryNestedEscapeEntry); ok {
		return wrapped.outputV3EntryRef
	}
	return entry
}

func (directory *v3RecoveryNestedEscapeDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	return v3RecoveryWrapNestedEscapeDirectory(duplicate, directory.path, directory.controller), err
}

func (directory *v3RecoveryNestedEscapeDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return directory.outputV3Directory.SameDirectory(v3RecoveryUnwrapNestedEscapeDirectory(other))
}

func (directory *v3RecoveryNestedEscapeDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	return v3RecoveryWrapNestedEscapeDirectory(
		opened, filepath.Join(directory.path, name), directory.controller,
	), err
}

func (directory *v3RecoveryNestedEscapeDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	return v3RecoveryWrapNestedEscapeDirectory(
		created, filepath.Join(directory.path, name), directory.controller,
	), err
}

func (directory *v3RecoveryNestedEscapeDirectory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	installed, err := directory.outputV3Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapNestedEscapeDirectory(candidate), name,
	)
	return v3RecoveryWrapNestedEscapeDirectory(
		installed, filepath.Join(directory.path, name), directory.controller,
	), err
}

func (directory *v3RecoveryNestedEscapeDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	return directory.outputV3Directory.RemoveDirectory(
		name, v3RecoveryUnwrapNestedEscapeDirectory(expected),
	)
}

func (directory *v3RecoveryNestedEscapeDirectory) OpenEntry(name string) (outputV3EntryRef, error) {
	entry, err := directory.outputV3Directory.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	return &v3RecoveryNestedEscapeEntry{
		outputV3EntryRef: entry,
		path:             filepath.Join(directory.path, name),
	}, nil
}

func (directory *v3RecoveryNestedEscapeDirectory) EntryMatches(
	name string,
	expected outputV3EntryRef,
) (bool, error) {
	return directory.outputV3Directory.EntryMatches(name, v3RecoveryUnwrapNestedEscapeEntry(expected))
}

func (directory *v3RecoveryNestedEscapeDirectory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	entry, ok := expected.(*v3RecoveryNestedEscapeEntry)
	childPath := ""
	if ok {
		childPath = entry.path
	}
	opened, err := directory.outputV3Directory.OpenPinnedDirectory(
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
	expected outputV3EntryRef,
) error {
	return directory.outputV3Directory.RemoveEntry(name, v3RecoveryUnwrapNestedEscapeEntry(expected))
}

type v3RecoveryNestedEscapeEntry struct {
	outputV3EntryRef
	path string
}
