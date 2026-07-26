package osfs

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3SessionCandidateAcceptsOnlyExactChildPrefixes(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	for subset := 0; subset < 1<<len(outputSessionCandidateChildren); subset++ {
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
				if err != nil || state != outputSessionCandidateComplete {
					t.Fatalf("completed prefix state = (%v, %v)", state, err)
				}
			} else {
				if !errors.Is(err, errOutputIntentUnsafe) {
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
	for _, child := range outputSessionCandidateChildren[1:] {
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
				if inspectErr != nil || state != outputSessionCandidateComplete {
					t.Fatalf("temporary cut completion = (%v, %v)", state, inspectErr)
				}
				if kind, observeErr := candidate.ObserveEntry(test.temporaryName); observeErr != nil ||
					kind != outputV3EntryAbsent {
					t.Fatalf("accepted header temporary remains = (%v, %v)", kind, observeErr)
				}
			} else {
				if !errors.Is(err, errOutputIntentUnsafe) {
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
	for wrongIndex, wrongName := range outputSessionCandidateChildren {
		t.Run(wrongName, func(t *testing.T) {
			candidate, header := fixture.newCandidate(t, byte(0xa0+wrongIndex))
			for index, child := range outputSessionCandidateChildren[:wrongIndex+1] {
				if index == 0 && index != wrongIndex {
					v3RecoveryCreateCandidateHeader(t, fixture.authority, candidate, header)
					continue
				}
				v3RecoveryCreateCandidateChild(t, candidate, child, index == wrongIndex)
			}
			before := v3RecoverySessionCandidateSnapshot(t, candidate)
			if err := fixture.authority.completeOutputSessionCandidate(candidate, header); !errors.Is(err, errOutputIntentUnsafe) {
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
	authority *FilesystemOutputAuthority
	platform  outputV3Platform
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
) (outputV3Directory, resumestate.Header) {
	t.Helper()
	sessionID := v3RecoveryIdentity16[transfer.OutputSessionID](value)
	name := resumestate.SessionCandidateName(sessionID)
	candidate, err := fixture.platform.Root().CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	header, err := newOutputSessionHeader(
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
	authority *FilesystemOutputAuthority,
	candidate outputV3Directory,
	header resumestate.Header,
	subset int,
) {
	t.Helper()
	for index, child := range outputSessionCandidateChildren {
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
	authority *FilesystemOutputAuthority,
	candidate outputV3Directory,
	header resumestate.Header,
) {
	t.Helper()
	encoded, err := resumestate.EncodeHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (outputStateStore{random: authority.random}).createRecord(
		candidate, resumestate.HeaderRecordName, encoded, resumestate.MaxSessionHeaderBytes,
	); err != nil {
		t.Fatal(err)
	}
}

func v3RecoveryCreateCandidateChild(
	t *testing.T,
	candidate outputV3Directory,
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
	candidate outputV3Directory,
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
	for length := 0; length <= len(outputSessionCandidateChildren); length++ {
		if subset == 1<<length-1 {
			return true
		}
	}
	return false
}

func v3RecoverySessionCandidateSnapshot(t *testing.T, candidate outputV3Directory) string {
	t.Helper()
	names, err := candidate.Names(len(outputSessionCandidateChildren) + outputStateAllocationAttempts + 2)
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
		case outputV3EntryRegularFile:
			file, err := candidate.OpenFile(name, true, false)
			if err != nil {
				t.Fatal(err)
			}
			size, sizeErr := file.Size()
			if err := errors.Join(sizeErr, file.Close()); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&snapshot, ":%d", size)
		case outputV3EntryDirectory:
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
