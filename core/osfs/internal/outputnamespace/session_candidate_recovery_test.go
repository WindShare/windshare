package outputnamespace

import (
	"bytes"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3InspectSessionCandidateClassifiesAmbiguousDurableCuts(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)

	empty, header := fixture.newCandidate(t, 0xc0)
	if state, err := inspectOutputSessionCandidate(empty, header); err != nil || state != sessionCandidateEmpty {
		t.Fatalf("empty candidate inspection = (%v, %v)", state, err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		value   byte
		prepare func(outputcap.Directory, resumestate.Header)
		expect  func(resumestate.Header) resumestate.Header
	}{
		{
			name: "opaque child", value: 0xc1,
			prepare: func(candidate outputcap.Directory, _ resumestate.Header) {
				createCandidateFileWithPayload(t, candidate, "opaque", nil)
			},
		},
		{
			name: "non-prefix child", value: 0xc2,
			prepare: func(candidate outputcap.Directory, header resumestate.Header) {
				v3RecoveryCreateCandidateHeader(t, fixture.authority, candidate, header)
				v3RecoveryCreateCandidateChild(t, candidate, resumestate.FilesDirectoryName, false)
			},
		},
		{
			name: "corrupt header", value: 0xc3,
			prepare: func(candidate outputcap.Directory, _ resumestate.Header) {
				createCandidateFileWithPayload(t, candidate, resumestate.HeaderRecordName, []byte("corrupt"))
			},
		},
		{
			name: "header binds another session", value: 0xc4,
			prepare: func(candidate outputcap.Directory, header resumestate.Header) {
				v3RecoveryCreateCandidateHeader(t, fixture.authority, candidate, header)
			},
			expect: func(_ resumestate.Header) resumestate.Header {
				other, err := fixture.authority.newHeader(
					fixture.control, fixture.selection,
					v3RecoveryAncestryBinding(t, fixture.control.OutputRoot(), fixture.selection),
					v3RecoveryIdentity16[transfer.OutputSessionID](0xee),
				)
				if err != nil {
					t.Fatal(err)
				}
				return other
			},
		},
		{
			name: "nonempty lock", value: 0xc5,
			prepare: func(candidate outputcap.Directory, header resumestate.Header) {
				v3RecoveryCreateCandidateHeader(t, fixture.authority, candidate, header)
				createCandidateFileWithPayload(t, candidate, resumestate.SessionLockName, []byte{1})
			},
		},
		{
			name: "nonempty private child", value: 0xc6,
			prepare: func(candidate outputcap.Directory, header resumestate.Header) {
				v3RecoveryCreateCandidateHeader(t, fixture.authority, candidate, header)
				v3RecoveryCreateCandidateChild(t, candidate, resumestate.SessionLockName, false)
				child, err := candidate.CreateDirectory(resumestate.FilesDirectoryName, true)
				if err != nil {
					t.Fatal(err)
				}
				createCandidateFileWithPayload(t, child, "witness", nil)
				if err := errors.Join(child.Sync(), candidate.Sync(), child.Close()); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, expected := fixture.newCandidate(t, test.value)
			test.prepare(candidate, expected)
			if test.expect != nil {
				expected = test.expect(expected)
			}
			state, err := inspectOutputSessionCandidate(candidate, expected)
			if state != 0 || !errors.Is(err, outputfault.ErrIntentUnsafe) {
				t.Fatalf("ambiguous candidate inspection = (%v, %v)", state, err)
			}
			if err := candidate.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3InspectSessionCandidatePropagatesFixedAuthorityFailures(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	injected := errors.New("candidate inspection failed")

	for index, test := range []struct {
		name      string
		prefix    int
		configure func(*outputV3CandidateFaultDirectory)
	}{
		{
			name: "enumerate candidate", prefix: 0,
			configure: func(candidate *outputV3CandidateFaultDirectory) { candidate.namesErr = injected },
		},
		{
			name: "open header", prefix: 1,
			configure: func(candidate *outputV3CandidateFaultDirectory) {
				candidate.openFileName, candidate.openFileErr = resumestate.HeaderRecordName, injected
			},
		},
		{
			name: "open lock", prefix: 2,
			configure: func(candidate *outputV3CandidateFaultDirectory) {
				candidate.openFileName, candidate.openFileErr = resumestate.SessionLockName, injected
			},
		},
		{
			name: "inspect lock size", prefix: 2,
			configure: func(candidate *outputV3CandidateFaultDirectory) {
				candidate.openFileName, candidate.fileSizeErr = resumestate.SessionLockName, injected
			},
		},
		{
			name: "close lock", prefix: 2,
			configure: func(candidate *outputV3CandidateFaultDirectory) {
				candidate.openFileName, candidate.fileCloseErr = resumestate.SessionLockName, injected
			},
		},
		{
			name: "open child", prefix: 3,
			configure: func(candidate *outputV3CandidateFaultDirectory) {
				candidate.openDirectoryName, candidate.openDirectoryErr = resumestate.FilesDirectoryName, injected
			},
		},
		{
			name: "enumerate child", prefix: 3,
			configure: func(candidate *outputV3CandidateFaultDirectory) {
				candidate.childNamesErr = map[string]error{resumestate.FilesDirectoryName: injected}
			},
		},
		{
			name: "close child", prefix: 3,
			configure: func(candidate *outputV3CandidateFaultDirectory) {
				candidate.childCloseErr = map[string]error{resumestate.FilesDirectoryName: injected}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, header := fixture.newCandidate(t, byte(0xd0+index))
			v3RecoveryBuildSessionCandidateSubset(t, fixture.authority, candidate, header, 1<<test.prefix-1)
			wrapped := &outputV3CandidateFaultDirectory{Directory: candidate}
			test.configure(wrapped)
			state, err := inspectOutputSessionCandidate(wrapped, header)
			if state != 0 || !errors.Is(err, injected) {
				t.Fatalf("faulted candidate inspection = (%v, %v)", state, err)
			}
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3SessionCandidateCreationCutRejectsUnverifiableEnumeration(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	injected := errors.New("candidate creation-cut inspection failed")

	for index, configure := range []func(*outputV3CandidateFaultDirectory){
		func(candidate *outputV3CandidateFaultDirectory) { candidate.namesErr = injected },
		func(candidate *outputV3CandidateFaultDirectory) { candidate.classifyErr = injected },
		func(candidate *outputV3CandidateFaultDirectory) { candidate.forceInexact = true },
	} {
		candidate, header := fixture.newCandidate(t, byte(0xe0+index))
		v3RecoveryCreateCandidateHeader(t, fixture.authority, candidate, header)
		wrapped := &outputV3CandidateFaultDirectory{Directory: candidate}
		configure(wrapped)
		err := validateOutputSessionCandidateCreationCut(wrapped)
		if index < 2 && !errors.Is(err, injected) {
			t.Fatalf("creation-cut fault %d error = %v", index, err)
		}
		if index == 2 && !errors.Is(err, outputfault.ErrIntentUnsafe) {
			t.Fatalf("inexact creation-cut error = %v", err)
		}
		if err := wrapped.Close(); err != nil {
			t.Fatal(err)
		}
	}

	overflow, _ := fixture.newCandidate(t, 0xe4)
	for index := range AllocationAttempts + 1 {
		raw := bytes.Repeat([]byte{byte(index + 1)}, resumestate.UpdateNonceBytes)
		nonce, err := resumestate.UpdateNonceFromBytes(raw)
		if err != nil {
			t.Fatal(err)
		}
		name, err := resumestate.RecordUpdateTemporaryName(resumestate.HeaderRecordName, nonce)
		if err != nil {
			t.Fatal(err)
		}
		createCandidateFileWithPayload(t, overflow, name, nil)
	}
	if err := validateOutputSessionCandidateCreationCut(overflow); !errors.Is(err, outputfault.ErrIntentUnsafe) {
		t.Fatalf("excess header temporaries error = %v", err)
	}
	if err := overflow.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOutputV3CanonicalIntentAndSessionAllocationPropagateDurableCuts(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	injected := errors.New("intent allocation failed")
	intent := fixture.selection.ResumeIntent()

	for index, configure := range []func(*outputV3CandidateFaultDirectory){
		func(directory *outputV3CandidateFaultDirectory) { directory.namesErr = injected },
		func(directory *outputV3CandidateFaultDirectory) { directory.createErr = injected },
		func(directory *outputV3CandidateFaultDirectory) { directory.createdSyncErr = injected },
		func(directory *outputV3CandidateFaultDirectory) { directory.syncErr = injected },
	} {
		directory := createCandidateDirectory(t, fixture.platform.Root(), byte(0x40+index))
		wrapped := &outputV3CandidateFaultDirectory{Directory: directory}
		configure(wrapped)
		opened, err := OpenCanonicalIntent(wrapped, intent)
		if opened != nil {
			_ = opened.Close()
		}
		if !errors.Is(err, injected) {
			t.Fatalf("canonical intent durable cut %d error = %v", index, err)
		}
		if err := wrapped.Close(); err != nil {
			t.Fatal(err)
		}
	}

	for index, test := range []struct {
		name      string
		ids       SessionIDSource
		configure func(*outputV3CandidateFaultDirectory)
		control   resumestate.Control
		cause     error
	}{
		{
			name:  "identity generator failure",
			ids:   &outputV3CandidateSessionIDs{err: injected},
			cause: injected,
		},
		{
			name:  "zero identities exhausted",
			ids:   &outputV3CandidateSessionIDs{zero: true},
			cause: outputfault.ErrIntentUnsafe,
		},
		{
			name: "candidate collisions exhausted",
			ids:  &outputV3CandidateSessionIDs{},
			configure: func(directory *outputV3CandidateFaultDirectory) {
				directory.createErr = outputcap.ErrNamespaceCollision
			},
			cause: outputfault.ErrIntentUnsafe,
		},
		{
			name:      "candidate create failure",
			ids:       &outputV3CandidateSessionIDs{},
			configure: func(directory *outputV3CandidateFaultDirectory) { directory.createErr = injected },
			cause:     injected,
		},
		{
			name:      "candidate directory sync failure",
			ids:       &outputV3CandidateSessionIDs{},
			configure: func(directory *outputV3CandidateFaultDirectory) { directory.createdSyncErr = injected },
			cause:     injected,
		},
		{
			name:      "intent directory sync failure",
			ids:       &outputV3CandidateSessionIDs{},
			configure: func(directory *outputV3CandidateFaultDirectory) { directory.syncErr = injected },
			cause:     injected,
		},
		{
			name:  "invalid header authority",
			ids:   &outputV3CandidateSessionIDs{},
			cause: resumestate.ErrInvalidState,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := createCandidateDirectory(t, fixture.platform.Root(), byte(0x60+index))
			wrapped := &outputV3CandidateFaultDirectory{Directory: directory}
			if test.configure != nil {
				test.configure(wrapped)
			}
			authority := fixture.authority
			authority.sessionIDs = test.ids
			control := fixture.control
			if test.name == "invalid header authority" {
				control = resumestate.Control{}
			}
			result, err := authority.createSessionDirectory(
				wrapped, control, fixture.selection,
				v3RecoveryAncestryBinding(t, fixture.control.OutputRoot(), fixture.selection),
			)
			name, opened := result.Name, result.Directory
			created := result.Disposition == SessionInstalled
			if name != "" || opened != nil || created || !errors.Is(err, test.cause) {
				t.Fatalf("session allocation failure = (name=%q, opened=%v, created=%t, err=%v)", name, opened, created, err)
			}
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3OpenOrCreateSessionDirectoryRejectsAmbiguousIntentCuts(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	injected := errors.New("open session candidate failed")

	for index, test := range []struct {
		name      string
		prepare   func(*testing.T, outputcap.Directory) string
		configure func(*outputV3CandidateFaultDirectory, string)
		cause     error
	}{
		{
			name: "enumerate intent",
			configure: func(directory *outputV3CandidateFaultDirectory, _ string) {
				directory.namesErr = injected
			},
			cause: injected,
		},
		{
			name: "opaque intent child",
			prepare: func(t *testing.T, intent outputcap.Directory) string {
				createCandidateFileWithPayload(t, intent, "opaque", nil)
				return ""
			},
			cause: outputfault.ErrIntentUnsafe,
		},
		{
			name: "multiple session entries",
			prepare: func(t *testing.T, intent outputcap.Directory) string {
				createClosedNamedCandidateDirectory(t, intent, resumestate.SessionDirectoryName(
					v3RecoveryIdentity16[transfer.OutputSessionID](0xf1),
				))
				createClosedNamedCandidateDirectory(t, intent, resumestate.SessionCandidateName(
					v3RecoveryIdentity16[transfer.OutputSessionID](0xf2),
				))
				return ""
			},
			cause: outputfault.ErrIntentUnsafe,
		},
		{
			name: "open candidate",
			prepare: func(t *testing.T, intent outputcap.Directory) string {
				name := resumestate.SessionCandidateName(v3RecoveryIdentity16[transfer.OutputSessionID](0xf3))
				createClosedNamedCandidateDirectory(t, intent, name)
				return name
			},
			configure: func(directory *outputV3CandidateFaultDirectory, name string) {
				directory.openDirectoryName, directory.openDirectoryErr = name, injected
			},
			cause: injected,
		},
		{
			name: "enumerate candidate",
			prepare: func(t *testing.T, intent outputcap.Directory) string {
				name := resumestate.SessionCandidateName(v3RecoveryIdentity16[transfer.OutputSessionID](0xf4))
				createClosedNamedCandidateDirectory(t, intent, name)
				return name
			},
			configure: func(directory *outputV3CandidateFaultDirectory, name string) {
				directory.childNamesErr = map[string]error{name: injected}
			},
			cause: injected,
		},
		{
			name: "remove empty candidate",
			prepare: func(t *testing.T, intent outputcap.Directory) string {
				name := resumestate.SessionCandidateName(v3RecoveryIdentity16[transfer.OutputSessionID](0xf5))
				createClosedNamedCandidateDirectory(t, intent, name)
				return name
			},
			configure: func(directory *outputV3CandidateFaultDirectory, name string) {
				directory.removeDirectoryErr = map[string]error{name: injected}
			},
			cause: injected,
		},
		{
			name: "reject non-prefix candidate before completion",
			prepare: func(t *testing.T, intent outputcap.Directory) string {
				name := resumestate.SessionCandidateName(v3RecoveryIdentity16[transfer.OutputSessionID](0xf6))
				candidate := createNamedCandidateDirectory(t, intent, name)
				createCandidateFileWithPayload(t, candidate, "opaque", nil)
				if err := candidate.Close(); err != nil {
					t.Fatal(err)
				}
				return name
			},
			cause: outputfault.ErrIntentUnsafe,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := createCandidateDirectory(t, fixture.platform.Root(), byte(0x80+index))
			candidateName := ""
			if test.prepare != nil {
				candidateName = test.prepare(t, intent)
			}
			wrapped := &outputV3CandidateFaultDirectory{Directory: intent}
			if test.configure != nil {
				test.configure(wrapped, candidateName)
			}
			result, err := fixture.authority.OpenOrCreateSession(
				wrapped, fixture.control, fixture.selection,
				v3RecoveryAncestryBinding(t, fixture.control.OutputRoot(), fixture.selection),
			)
			name, opened := result.Name, result.Directory
			created := result.Disposition == SessionInstalled
			if opened != nil {
				_ = opened.Close()
			}
			if name != "" || created || !errors.Is(err, test.cause) {
				t.Fatalf("ambiguous session candidate = (name=%q, created=%t, err=%v)", name, created, err)
			}
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3SessionCandidateInstallAndRemovalRevalidateFixedAuthority(t *testing.T) {
	fixture := newV3RecoverySessionCandidateMatrix(t)
	defer fixture.close(t)
	injected := errors.New("candidate namespace mutation failed")

	for index, test := range []struct {
		name      string
		configure func(*outputV3CandidateFaultDirectory, string)
		cause     error
	}{
		{
			name: "install mutation",
			configure: func(directory *outputV3CandidateFaultDirectory, _ string) {
				directory.installErr = injected
			},
			cause: injected,
		},
		{
			name: "installed identity mismatch",
			configure: func(directory *outputV3CandidateFaultDirectory, _ string) {
				directory.forceInstalledMismatch = true
			},
			cause: outputfault.ErrIntentUnsafe,
		},
		{
			name: "sync installed parent",
			configure: func(directory *outputV3CandidateFaultDirectory, _ string) {
				directory.syncErr = injected
			},
			cause: injected,
		},
		{
			name: "reopen installed entry",
			configure: func(directory *outputV3CandidateFaultDirectory, installedName string) {
				directory.openDirectoryName, directory.openDirectoryErr = installedName, injected
			},
			cause: injected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			intent := createCandidateDirectory(t, fixture.platform.Root(), byte(0xa0+index))
			candidate := createNamedCandidateDirectory(t, intent, "candidate-origin")
			installedName := resumestate.SessionDirectoryName(
				v3RecoveryIdentity16[transfer.OutputSessionID](byte(0xa0 + index)),
			)
			wrapped := &outputV3CandidateFaultDirectory{Directory: intent}
			test.configure(wrapped, installedName)
			installed, err := installOutputSessionCandidate(wrapped, candidate, installedName)
			if installed != nil {
				_ = installed.Close()
			}
			if !errors.Is(err, test.cause) {
				t.Fatalf("candidate install error = %v", err)
			}
			_ = candidate.Close()
			if err := wrapped.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	for index, test := range []struct {
		name      string
		nonempty  bool
		configure func(*outputV3CandidateFaultDirectory, *outputV3CandidateFaultDirectory, string)
		cause     error
	}{
		{
			name: "verify candidate entry",
			configure: func(intent, _ *outputV3CandidateFaultDirectory, candidateName string) {
				intent.openDirectoryName, intent.openDirectoryErr = candidateName, injected
			},
			cause: injected,
		},
		{name: "nonempty candidate", nonempty: true, cause: outputfault.ErrIntentUnsafe},
		{
			name: "enumerate candidate",
			configure: func(_ *outputV3CandidateFaultDirectory, candidate *outputV3CandidateFaultDirectory, _ string) {
				candidate.namesErr = injected
			},
			cause: injected,
		},
		{
			name: "remove candidate entry",
			configure: func(intent, _ *outputV3CandidateFaultDirectory, candidateName string) {
				intent.removeDirectoryErr = map[string]error{candidateName: injected}
			},
			cause: injected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			intentDirectory := createCandidateDirectory(t, fixture.platform.Root(), byte(0xb0+index))
			candidateName := resumestate.SessionCandidateName(
				v3RecoveryIdentity16[transfer.OutputSessionID](byte(0xb0 + index)),
			)
			candidateDirectory := createNamedCandidateDirectory(t, intentDirectory, candidateName)
			if test.nonempty {
				createCandidateFileWithPayload(t, candidateDirectory, "witness", nil)
			}
			intent := &outputV3CandidateFaultDirectory{Directory: intentDirectory}
			candidate := &outputV3CandidateFaultDirectory{Directory: candidateDirectory}
			if test.configure != nil {
				test.configure(intent, candidate, candidateName)
			}
			err := removeOutputSessionCandidate(intent, candidateName, candidate)
			if !errors.Is(err, test.cause) {
				t.Fatalf("candidate removal error = %v", err)
			}
			if err := intent.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
