package outputnamespace

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3SessionCandidateAcceptsOnlyExactChildPrefixes(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	for subset := 0; subset < 1<<len(sessionCandidateChildren); subset++ {
		t.Run(strconv.Itoa(subset), func(t *testing.T) {
			candidate, header := fixture.newCandidate(t, byte(0x30+subset))
			v3RecoveryBuildSessionCandidateSubset(t, fixture.authority, candidate, header, subset)
			before := v3RecoverySessionCandidateSnapshot(t, candidate)
			err := fixture.authority.completeOutputSessionCandidate(candidate, header)
			if v3RecoveryIsSessionCandidatePrefix(subset) {
				if err != nil {
					t.Fatal(err)
				}
				state, err := inspectOutputSessionCandidate(candidate, header)
				if err != nil || state != sessionCandidateComplete {
					t.Fatalf("completed prefix state = (%v, %v)", state, err)
				}
			} else {
				if !errors.Is(err, outputfault.ErrIntentUnsafe) {
					t.Fatalf("non-prefix subset %05b completion error = %v", subset, err)
				}
				after := v3RecoverySessionCandidateSnapshot(t, candidate)
				if after != before {
					t.Fatalf("rejected subset mutated before validation:\nbefore %s\nafter  %s", before, after)
				}
			}
			if err := candidate.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3SessionCandidateRecognizesOnlyCanonicalInitialHeaderTemporaryCuts(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	canonicalTemporary := v3RecoveryCandidateHeaderTemporaryName(t)
	tests := []struct {
		name          string
		header        bool
		temporaryName string
		temporaryDir  bool
		laterChildren []string
		accept        bool
	}{
		{name: "initial-header-temp-only", temporaryName: canonicalTemporary, accept: true},
		{name: "header-and-temp", header: true, temporaryName: canonicalTemporary, accept: true},
		{name: "malformed-temp", temporaryName: resumestate.HeaderUpdateTemporaryPrefix + "not-canonical"},
		{name: "canonical-temp-wrong-type", temporaryName: canonicalTemporary, temporaryDir: true},
		{name: "temp-and-lock-without-header", temporaryName: canonicalTemporary, laterChildren: []string{resumestate.SessionLockName}},
	}
	for _, child := range sessionCandidateChildren[1:] {
		tests = append(tests, struct {
			name          string
			header        bool
			temporaryName string
			temporaryDir  bool
			laterChildren []string
			accept        bool
		}{
			name: "header-temp-and-" + child, header: true, temporaryName: canonicalTemporary,
			laterChildren: []string{child},
		})
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, header := fixture.newCandidate(t, byte(0x70+index))
			if test.header {
				v3RecoveryCreateCandidateHeader(t, fixture.authority, candidate, header)
			}
			v3RecoveryCreateCandidateTemporary(t, candidate, test.temporaryName, test.temporaryDir)
			for _, child := range test.laterChildren {
				v3RecoveryCreateCandidateChild(t, candidate, child, false)
			}
			before := v3RecoverySessionCandidateSnapshot(t, candidate)
			err := fixture.authority.completeOutputSessionCandidate(candidate, header)
			if test.accept {
				if err != nil {
					t.Fatal(err)
				}
				state, inspectErr := inspectOutputSessionCandidate(candidate, header)
				if inspectErr != nil || state != sessionCandidateComplete {
					t.Fatalf("temporary cut completion = (%v, %v)", state, inspectErr)
				}
				if kind, observeErr := candidate.ObserveEntry(test.temporaryName); observeErr != nil ||
					kind != outputcap.EntryAbsent {
					t.Fatalf("accepted header temporary remains = (%v, %v)", kind, observeErr)
				}
			} else {
				if !errors.Is(err, outputfault.ErrIntentUnsafe) {
					t.Fatalf("unsafe temporary cut error = %v", err)
				}
				after := v3RecoverySessionCandidateSnapshot(t, candidate)
				if after != before {
					t.Fatalf("rejected temporary cut mutated:\nbefore %s\nafter  %s", before, after)
				}
			}
			if err := candidate.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3SessionCandidateRejectsEveryWrongChildTypeBeforeMutation(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	for wrongIndex, wrongName := range sessionCandidateChildren {
		t.Run(wrongName, func(t *testing.T) {
			candidate, header := fixture.newCandidate(t, byte(0xa0+wrongIndex))
			for index, child := range sessionCandidateChildren[:wrongIndex+1] {
				if index == 0 && index != wrongIndex {
					v3RecoveryCreateCandidateHeader(t, fixture.authority, candidate, header)
					continue
				}
				v3RecoveryCreateCandidateChild(t, candidate, child, index == wrongIndex)
			}
			before := v3RecoverySessionCandidateSnapshot(t, candidate)
			if err := fixture.authority.completeOutputSessionCandidate(candidate, header); !errors.Is(err, outputfault.ErrIntentUnsafe) {
				t.Fatalf("wrong type for %q completion error = %v", wrongName, err)
			}
			after := v3RecoverySessionCandidateSnapshot(t, candidate)
			if after != before {
				t.Fatalf("wrong-type rejection mutated:\nbefore %s\nafter  %s", before, after)
			}
			if err := candidate.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type v3RecoverySessionCandidateMatrix struct {
	authority Controller
	platform  outputcap.Platform
	control   resumestate.Control
	selection transfer.OutputSelection
}

func newV3RecoverySessionCandidateMatrix(t *testing.T) v3RecoverySessionCandidateMatrix {
	t.Helper()
	root := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, root, nil)
	platform, err := openOutputV3Platform(root, false)
	if err != nil {
		t.Fatal(err)
	}
	control, err := authority.newControl(platform)
	if err != nil {
		t.Fatal(err)
	}
	return v3RecoverySessionCandidateMatrix{
		authority: authority, platform: platform, control: control,
		selection: v3RecoverySelection(t, false, 0),
	}
}

func (fixture v3RecoverySessionCandidateMatrix) newCandidate(
	t *testing.T,
	value byte,
) (outputcap.Directory, resumestate.Header) {
	t.Helper()
	sessionID := v3RecoveryIdentity16[transfer.OutputSessionID](value)
	name := resumestate.SessionCandidateName(sessionID)
	candidate, err := fixture.platform.Root().CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	header, err := fixture.authority.newHeader(
		fixture.control, fixture.selection,
		v3RecoveryAncestryBinding(t, fixture.control.OutputRoot(), fixture.selection), sessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	return candidate, header
}

func (fixture v3RecoverySessionCandidateMatrix) close(t *testing.T) {
	t.Helper()
	if err := fixture.platform.Close(); err != nil {
		t.Fatal(err)
	}
}

func v3RecoveryBuildSessionCandidateSubset(
	t *testing.T,
	authority Controller,
	candidate outputcap.Directory,
	header resumestate.Header,
	subset int,
) {
	t.Helper()
	for index, child := range sessionCandidateChildren {
		if subset&(1<<index) == 0 {
			continue
		}
		if index == 0 {
			v3RecoveryCreateCandidateHeader(t, authority, candidate, header)
			continue
		}
		v3RecoveryCreateCandidateChild(t, candidate, child, false)
	}
}

func v3RecoveryCreateCandidateHeader(
	t *testing.T,
	authority Controller,
	candidate outputcap.Directory,
	header resumestate.Header,
) {
	t.Helper()
	encoded, err := resumestate.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (Store{random: authority.random}).CreateRecord(
		candidate, resumestate.HeaderRecordName, encoded, resumestate.MaxSessionHeaderBytes,
	); err != nil {
		t.Fatal(err)
	}
}

func v3RecoveryCreateCandidateChild(
	t *testing.T,
	candidate outputcap.Directory,
	name string,
	wrongType bool,
) {
	t.Helper()
	expectFile := name == resumestate.HeaderRecordName || name == resumestate.SessionLockName
	createFile := expectFile != wrongType
	if createFile {
		file, err := candidate.CreateFile(name, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(file.Sync(), candidate.Sync(), file.Close()); err != nil {
			t.Fatal(err)
		}
		return
	}
	directory, err := candidate.CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(directory.Sync(), candidate.Sync(), directory.Close()); err != nil {
		t.Fatal(err)
	}
}

func v3RecoveryCandidateHeaderTemporaryName(t *testing.T) string {
	t.Helper()
	nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0xe1}, resumestate.UpdateNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	name, err := resumestate.RecordUpdateTemporaryName(resumestate.HeaderRecordName, nonce)
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func v3RecoveryCreateCandidateTemporary(
	t *testing.T,
	candidate outputcap.Directory,
	name string,
	directory bool,
) {
	t.Helper()
	if directory {
		created, err := candidate.CreateDirectory(name, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(created.Sync(), candidate.Sync(), created.Close()); err != nil {
			t.Fatal(err)
		}
		return
	}
	created, err := candidate.CreateFile(name, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := created.WriteAt([]byte{0x5c}, 0); err != nil || written != 1 {
		t.Fatalf("write candidate temporary = (%d, %v)", written, err)
	}
	if err := errors.Join(created.Sync(), candidate.Sync(), created.Close()); err != nil {
		t.Fatal(err)
	}
}

func v3RecoveryIsSessionCandidatePrefix(subset int) bool {
	for length := 0; length <= len(sessionCandidateChildren); length++ {
		if subset == 1<<length-1 {
			return true
		}
	}
	return false
}

func v3RecoverySessionCandidateSnapshot(t *testing.T, candidate outputcap.Directory) string {
	t.Helper()
	names, err := candidate.Names(len(sessionCandidateChildren) + AllocationAttempts + 2)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	var snapshot strings.Builder
	for _, name := range names {
		kind, exact, err := candidate.ClassifyExactEntry(name)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&snapshot, "%s:%d:%t", name, kind, exact)
		switch kind {
		case outputcap.EntryRegularFile:
			file, err := candidate.OpenFile(name, true, false)
			if err != nil {
				t.Fatal(err)
			}
			size, sizeErr := file.Size()
			if err := errors.Join(sizeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&snapshot, ":%d", size)
		case outputcap.EntryDirectory:
			directory, err := candidate.OpenDirectory(name, true)
			if err != nil {
				t.Fatal(err)
			}
			children, listErr := directory.Names(2)
			if err := errors.Join(listErr, directory.Close()); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&snapshot, ":%d", len(children))
		}
		snapshot.WriteByte(';')
	}
	return snapshot.String()
}

func TestOutputV3BootstrapCandidateInspectionFailuresPreserveRecoverableAuthority(t *testing.T) {
	tests := []struct {
		name        string
		prefixCount int
		plan        func(string) *outputV3ControlSessionFaultPlan
	}{
		{
			name:        "open-candidate",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSOpenDirectory, "", candidate)
			},
		},
		{
			name:        "classify-control-record",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSClassifyEntry, candidate, resumestate.ControlRecordName,
				)
			},
		},
		{
			name:        "enumerate-control-temporaries",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSNamesWithPrefix, candidate, resumestate.ControlUpdateTemporaryPrefix,
				)
			},
		},
		{
			name:        "enumerate-structural-cut",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
			},
		},
		{
			name:        "enumerate-completed-cut",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
				plan.atCall = 2
				return plan
			},
		},
		{
			name:        "revalidate-root-binding",
			prefixCount: 3,
			plan: func(_ string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSRootBinding, "", "")
				plan.atCall = 2
				return plan
			},
		},
		{
			name:        "open-coordinator-record",
			prefixCount: 2,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSOpenFile, candidate, resumestate.CoordinatorLockName,
				)
			},
		},
		{
			name:        "validate-complete-children",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
				plan.atCall = 3
				return plan
			},
		},
		{
			name:        "open-sessions-child",
			prefixCount: 3,
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSOpenDirectory, candidate, resumestate.SessionsDirectoryName,
				)
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, root, nil)
			candidateName := v3RecoveryBootstrapCandidateName(t, byte(0x80+index))
			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			expectedControl, err := authority.newControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := platform.Root().CreateDirectory(candidateName, true)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildBootstrapPrefix(t, authority, candidate, expectedControl, test.prefixCount)
			if err := errors.Join(candidate.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}

			plan := test.plan(candidateName)
			faulted := openOutputV3ControlSessionFaultPlatform(t, root, plan)
			inspected, _, observation, inspectErr := inspectBootstrapCandidate(
				authority, faulted.Root(), candidateName, faulted,
			)
			if inspected != nil {
				_ = inspected.Close()
			}
			if observation != resumestate.BootstrapCandidateUnsafe ||
				!errors.Is(inspectErr, errOutputV3ControlSessionInjected) {
				t.Fatalf("candidate inspection = (%v, %v), want unsafe injected failure", observation, inspectErr)
			}
			plan.requireFired(t)
			if err := faulted.Close(); err != nil {
				t.Fatal(err)
			}

			recoveryPlatform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			recoveryResult, err := authority.OpenOrBootstrapControl(recoveryPlatform)
			recovered := recoveryResult.Namespace
			if err != nil || recovered.control != expectedControl {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover inspected candidate = (same=%t, err=%v)",
					recovered != nil && recovered.control == expectedControl, err)
			}
			candidates, listErr := recoveryPlatform.Root().NamesWithPrefix(
				resumestate.BootstrapCandidatePrefix, RootInspectionLimit,
			)
			if listErr != nil || len(candidates) != 0 {
				t.Fatalf("candidates after inspection recovery = %v, %v", candidates, listErr)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3DirectoryCreationSettlesNamespaceRacesBeforeReturningAuthority(t *testing.T) {
	for _, test := range []struct {
		name             string
		configure        func(*stateStoreDirectoryCreationFault)
		wantCreated      bool
		wantError        error
		wantDirectory    bool
		wantPersistedDir bool
	}{
		{name: "create-and-sync", wantCreated: true, wantDirectory: true, wantPersistedDir: true},
		{
			name: "concurrent-winner",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.createCollision = true
			},
			wantDirectory: true, wantPersistedDir: true,
		},
		{
			name: "classification-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.classifyOverride = true
				faults.classifyErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected,
		},
		{
			name: "aliased-existing-entry",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.classifyOverride = true
				faults.classifyKind = outputcap.EntryDirectory
			},
			wantError: outputcap.ErrUnsafeNamespace,
		},
		{
			name: "creation-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.createErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected,
		},
		{
			name: "child-sync-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.childSyncErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected, wantPersistedDir: true,
		},
		{
			name: "parent-sync-failed",
			configure: func(faults *stateStoreDirectoryCreationFault) {
				faults.parentSyncErr = errStateStoreInjected
			},
			wantError: errStateStoreInjected, wantPersistedDir: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			platform, parent := stateStoreEmptyDirectoryFixture(t)
			defer closeStateStoreFixture(t, platform, parent)
			faults := &stateStoreDirectoryCreationFault{Directory: parent}
			if test.configure != nil {
				test.configure(faults)
			}
			result, err := EnsureDirectory(faults, "child", true)
			directory := result.Directory
			created := result.Disposition == DirectoryCreated
			if created != test.wantCreated || (directory != nil) != test.wantDirectory || !errors.Is(err, test.wantError) {
				t.Fatalf(
					"ensure directory = (%v, created=%t, %v), want directory=%t created=%t error=%v",
					directory, created, err, test.wantDirectory, test.wantCreated, test.wantError,
				)
			}
			if directory != nil {
				if err := directory.Close(); err != nil {
					t.Fatal(err)
				}
			}
			kind, observeErr := parent.ObserveEntry("child")
			if observeErr != nil {
				t.Fatal(observeErr)
			}
			persisted := kind == outputcap.EntryDirectory
			if persisted != test.wantPersistedDir {
				t.Fatalf("persisted child directory=%t, want %t", persisted, test.wantPersistedDir)
			}
		})
	}
}
