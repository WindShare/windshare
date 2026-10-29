//go:build darwin

package outputdarwin

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

func TestPublicationRejectsInvalidNamesBeforeMutation(t *testing.T) {
	const overlongComponentBytes = 1024
	for _, test := range []struct {
		name      string
		component string
		cause     error
	}{
		{"invalid-component", "../outside", outputcap.ErrUnsafeNamespace},
		{"native-name-limit", strings.Repeat("x", overlongComponentBytes), unix.ENAMETOOLONG},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform := openAuthorityFixture(t, t.TempDir())
			source, err := platform.root.CreateFile("source", false, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			created, outcome, err := platform.root.ReservePublicDirectoryNoReplace(test.component)
			if created != nil || outcome != 0 || !errors.Is(err, test.cause) {
				t.Fatalf("directory rejection: created=%v outcome=%v err=%v", created, outcome, err)
			}
			outcome, err = platform.root.PublishFileNoReplace(source, test.component)
			if outcome != 0 || !errors.Is(err, test.cause) {
				t.Fatalf("file rejection: outcome=%v err=%v", outcome, err)
			}
			names, err := platform.root.Names(1)
			if err != nil || len(names) != 1 || names[0] != "source" {
				t.Fatalf("rejection changed the namespace: names=%v err=%v", names, err)
			}
		})
	}
}

func TestPublicationRejectsChangedSourceBeforeMutation(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		name := "missing-source"
		if replacement {
			name = "replaced-source"
		}
		t.Run(name, func(t *testing.T) {
			platform := openAuthorityFixture(t, t.TempDir())
			source, err := platform.root.CreateFile("source", false, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer source.Close()
			if err := unix.Renameat(platform.root.fd(), "source", platform.root.fd(), "moved"); err != nil {
				t.Fatal(err)
			}
			if replacement {
				foreign, err := platform.root.CreateFile("source", false, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer foreign.Close()
			}
			outcome, err := platform.root.PublishFileNoReplace(source, "final")
			if outcome != 0 || !errors.Is(err, outputcap.ErrFixedLinkSourceChanged) {
				t.Fatalf("changed-source outcome=%v err=%v", outcome, err)
			}
			if kind, err := platform.root.ObserveEntry("final"); err != nil || kind != outputcap.EntryAbsent {
				t.Fatalf("rejected source published: kind=%v err=%v", kind, err)
			}
		})
	}
}

func TestPublicationRejectsLostCreateAuthorityBeforeMutation(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged receiver")
	}
	platform := openAuthorityFixture(t, t.TempDir())
	source, err := platform.root.CreateFile("source", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	const readSearchOnly = 0o500
	if err := unix.Fchmod(platform.root.fd(), readSearchOnly); err != nil {
		t.Fatal(err)
	}
	defer unix.Fchmod(platform.root.fd(), privateDirectoryMode)
	created, outcome, err := platform.root.ReservePublicDirectoryNoReplace("reserved")
	if created != nil || outcome != 0 || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("directory authority loss: created=%v outcome=%v err=%v", created, outcome, err)
	}
	outcome, err = platform.root.PublishFileNoReplace(source, "final")
	if outcome != 0 || !errors.Is(err, os.ErrPermission) {
		t.Fatalf("file authority loss: outcome=%v err=%v", outcome, err)
	}
}

func TestPublicationFailuresPreserveMutationEvidence(t *testing.T) {
	for _, operation := range []string{"directory", "file"} {
		t.Run(operation, func(t *testing.T) {
			for _, test := range []struct {
				name        string
				mutation    error
				observation error
				sync        error
				outcome     outputcap.PublishNoReplaceOutcome
				present     bool
			}{
				{name: "create-denied", mutation: unix.EACCES},
				{name: "create-full", mutation: unix.ENOSPC},
				{name: "create-read-only", mutation: unix.EROFS},
				{name: "collision", mutation: outputcap.ErrNamespaceCollision, outcome: outputcap.PublishNoReplaceCollision},
				{name: "observe-denied", observation: unix.EACCES, outcome: outputcap.PublishNoReplaceIndeterminate, present: true},
				{name: "observe-identity", observation: outputcap.ErrUnsafeNamespace, outcome: outputcap.PublishNoReplaceIndeterminate, present: true},
				{name: "observe-collision", observation: outputcap.ErrNamespaceCollision, outcome: outputcap.PublishNoReplaceIndeterminate, present: true},
				{name: "sync-full", sync: unix.ENOSPC, outcome: outputcap.PublishNoReplaceIndeterminate, present: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					platform := openAuthorityFixture(t, t.TempDir())
					source, err := platform.root.CreateFile("source", false, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer source.Close()
					failing := publicationFaultDirectory{
						directory: platform.root, mutationErr: test.mutation,
						observationErr: test.observation, syncErr: test.sync,
					}
					before := openDescriptorCount(t)
					var outcome outputcap.PublishNoReplaceOutcome
					switch operation {
					case "directory":
						var created outputcap.Directory
						created, outcome, err = reservePublicDirectoryNoReplace(failing, "result")
						if created != nil {
							_ = created.Close()
							t.Fatal("failed reservation returned a committed handle")
						}
					case "file":
						outcome, err = publishFileNoReplace(failing, source, "result")
					}
					if outcome != test.outcome {
						t.Fatalf("outcome=%v want=%v err=%v", outcome, test.outcome, err)
					}
					if test.outcome == outputcap.PublishNoReplaceCollision {
						if err != nil {
							t.Fatalf("collision error=%v", err)
						}
					} else {
						for _, expected := range []error{test.mutation, test.observation, test.sync} {
							if expected != nil && !errors.Is(err, expected) {
								t.Fatalf("lost native cause: err=%v want=%v", err, expected)
							}
						}
					}
					kind, observeErr := platform.root.ObserveEntry("result")
					if observeErr != nil || (kind != outputcap.EntryAbsent) != test.present {
						t.Fatalf("publication evidence: kind=%v want-present=%t err=%v", kind, test.present, observeErr)
					}
					if after := openDescriptorCount(t); after != before {
						t.Fatalf("failed publication leaked handles: before=%d after=%d", before, after)
					}
				})
			}
		})
	}
}

// Each failure is injected at one semantic boundary; successful mutations still
// use the real filesystem so outcomes are checked against observable entries.
type publicationFaultDirectory struct {
	*directory
	mutationErr    error
	observationErr error
	syncErr        error
}

func (dir publicationFaultDirectory) createDirectoryEntry(name string, mode uint32) error {
	if dir.mutationErr != nil {
		return dir.mutationErr
	}
	return dir.directory.createDirectoryEntry(name, mode)
}

func (dir publicationFaultDirectory) linkFileEntryNoReplace(source outputcap.FileIdentity, name string) error {
	if dir.mutationErr != nil {
		return dir.mutationErr
	}
	return dir.directory.linkFileEntryNoReplace(source, name)
}

func (dir publicationFaultDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	if dir.observationErr != nil {
		return nil, dir.observationErr
	}
	return dir.directory.OpenDirectory(name, private)
}

func (dir publicationFaultDirectory) openLinkedFile(source outputcap.FileIdentity, name string) (outputcap.ObservedFile, error) {
	if dir.observationErr != nil {
		return nil, dir.observationErr
	}
	return dir.directory.openLinkedFile(source, name)
}

func (dir publicationFaultDirectory) Sync() error {
	if dir.syncErr != nil {
		return dir.syncErr
	}
	return dir.directory.Sync()
}
