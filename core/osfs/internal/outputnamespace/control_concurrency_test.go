package outputnamespace

import (
	"bytes"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

const v3RecoveryLockGateTimeout = 5 * time.Second

func TestControlNamespaceNilCloseIsSafe(t *testing.T) {
	if err := (*ControlNamespace)(nil).Close(); err != nil {
		t.Fatalf("close absent control namespace: %v", err)
	}
}

func TestOutputV3BootstrapCandidateAcceptsOnlyConstructionPrefixes(t *testing.T) {
	const (
		controlChild = 1 << iota
		lockChild
		sessionsChild
	)
	wantState := map[int]resumestate.BootstrapCandidateObservation{
		0:                                        resumestate.BootstrapCandidateEmpty,
		controlChild:                             resumestate.BootstrapCandidateValidPartial,
		controlChild | lockChild:                 resumestate.BootstrapCandidateValidPartial,
		controlChild | lockChild | sessionsChild: resumestate.BootstrapCandidateComplete,
	}

	for subset := range 1 << 3 {
		t.Run(strconv.Itoa(subset), func(t *testing.T) {
			rootPath := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, rootPath, nil)
			platform, err := openOutputV3Platform(rootPath, false)
			if err != nil {
				t.Fatal(err)
			}
			control, err := authority.newControl(platform)
			if err != nil {
				t.Fatal(err)
			}
			name := v3RecoveryBootstrapCandidateName(t, byte(0xb0+subset))
			candidate, err := platform.Root().CreateDirectory(name, true)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildBootstrapSubset(t, authority, candidate, control, subset)
			if err := candidate.Close(); err != nil {
				t.Fatal(err)
			}

			inspected, actualControl, actualState, err := inspectBootstrapCandidate(
				authority, platform.Root(), name, platform,
			)
			if err != nil {
				t.Fatal(err)
			}
			expectedState, valid := wantState[subset]
			if !valid {
				expectedState = resumestate.BootstrapCandidateUnsafe
			}
			if actualState != expectedState {
				t.Fatalf("subset %03b classified %v, want %v", subset, actualState, expectedState)
			}
			if valid && subset != 0 && actualControl != control {
				t.Fatal("valid bootstrap prefix lost its root-bound control authority")
			}
			if err := errors.Join(inspected.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputV3BootstrapCandidateRejectsUnstableCoordinatorLockBeforeCompletion(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	authority := v3RecoveryAuthority(t, rootPath, nil)
	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	control, err := authority.newControl(platform)
	if err != nil {
		t.Fatal(err)
	}
	name := v3RecoveryBootstrapCandidateName(t, 0xbe)
	candidate, err := platform.Root().CreateDirectory(name, true)
	if err != nil {
		t.Fatal(err)
	}
	v3RecoveryBuildBootstrapSubset(t, authority, candidate, control, 1)
	lock, err := candidate.CreateFile(resumestate.CoordinatorLockName, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := lock.WriteAt([]byte{1}, 0); err != nil || written != 1 {
		t.Fatalf("write unstable coordinator lock = (%d, %v)", written, err)
	}
	if err := errors.Join(lock.Sync(), candidate.Sync(), lock.Close(), candidate.Close()); err != nil {
		t.Fatal(err)
	}

	inspected, _, state, err := inspectBootstrapCandidate(authority, platform.Root(), name, platform)
	if !errors.Is(err, outputfault.ErrRootUnsafe) || state != resumestate.BootstrapCandidateUnsafe {
		t.Fatalf("unstable coordinator lock classified (%v, %v), want unsafe", state, err)
	}
	if kind, observeErr := inspected.ObserveEntry(resumestate.SessionsDirectoryName); observeErr != nil ||
		kind != outputcap.EntryAbsent {
		t.Fatalf("unsafe lock allowed candidate completion: sessions=(%v, %v)", kind, observeErr)
	}
	if err := errors.Join(inspected.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestOutputV3ConcurrentBootstrapCollisionLeavesNoCandidate(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	gate := &v3RecoveryBootstrapInstallGate{
		scanReady: make(chan struct{}), ready: make(chan struct{}), release: make(chan struct{}),
	}
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	type bootstrapResult struct {
		namespace *ControlNamespace
		platform  outputcap.Platform
		created   bool
		err       error
	}
	results := make(chan bootstrapResult, 2)
	for index := range 2 {
		authority := v3RecoveryAuthority(t, rootPath, nil)
		authority.random = bytes.NewReader(bytes.Repeat([]byte{byte(0xc1 + index)}, 64*1024))
		platform, err := openOutputV3Platform(rootPath, false)
		if err != nil {
			t.Fatal(err)
		}
		wrapped := v3RecoveryWrapBootstrapPlatform(platform, gate)
		go func() {
			result, err := authority.OpenOrBootstrapControl(wrapped)
			namespace := result.Namespace
			created := result.Disposition == ControlInstalled
			results <- bootstrapResult{namespace: namespace, platform: wrapped, created: created, err: err}
		}()
	}

	select {
	case <-gate.ready:
		close(gate.release)
	case <-time.After(v3RecoveryLockGateTimeout):
		t.Fatal("concurrent bootstraps did not both reach control installation")
	}
	var controls []resumestate.Control
	for range 2 {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			if !result.created {
				t.Fatal("a bootstrap participant did not classify its collision as a successful creation")
			}
			controls = append(controls, result.namespace.control)
			if err := errors.Join(result.namespace.Close(), result.platform.Close()); err != nil {
				t.Fatal(err)
			}
		case <-time.After(v3RecoveryLockGateTimeout):
			t.Fatal("concurrent bootstrap did not settle")
		}
	}
	if controls[0] != controls[1] {
		t.Fatal("concurrent bootstrap participants adopted different root control authorities")
	}

	platform, err := openOutputV3Platform(rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := platform.Root().NamesWithPrefix(
		resumestate.BootstrapCandidatePrefix, RootInspectionLimit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("bootstrap collision left candidates behind: %v", candidates)
	}
	authority := v3RecoveryAuthority(t, rootPath, nil)
	installed, err := authority.OpenInstalledControl(platform.Root(), platform)
	if err != nil {
		t.Fatal(err)
	}
	if installed.control != controls[0] {
		t.Fatal("installed control differs from the authority adopted by bootstrap participants")
	}
	if err := errors.Join(installed.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
}

func v3RecoveryBuildBootstrapSubset(
	t *testing.T,
	authority Controller,
	candidate outputcap.Directory,
	control resumestate.Control,
	subset int,
) {
	t.Helper()
	if subset&1 != 0 {
		encoded, err := resumestate.EncodeControl(control)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (Store{random: authority.random}).CreateRecord(
			candidate, resumestate.ControlRecordName, encoded, resumestate.MaxControlStateBytes,
		); err != nil {
			t.Fatal(err)
		}
	}
	if subset&2 != 0 {
		lock, err := candidate.CreateFile(resumestate.CoordinatorLockName, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(lock.Sync(), candidate.Sync(), lock.Close()); err != nil {
			t.Fatal(err)
		}
	}
	if subset&4 != 0 {
		sessions, err := candidate.CreateDirectory(resumestate.SessionsDirectoryName, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(sessions.Sync(), candidate.Sync(), sessions.Close()); err != nil {
			t.Fatal(err)
		}
	}
	if err := candidate.Sync(); err != nil {
		t.Fatal(err)
	}
}

type v3RecoveryBootstrapInstallGate struct {
	mu           sync.Mutex
	scanArrivals int
	arrivals     int
	scanReady    chan struct{}
	ready        chan struct{}
	release      chan struct{}
}

func (gate *v3RecoveryBootstrapInstallGate) waitForInitialScan() {
	gate.mu.Lock()
	gate.scanArrivals++
	if gate.scanArrivals == 2 {
		close(gate.scanReady)
	}
	ready := gate.scanReady
	gate.mu.Unlock()
	<-ready
}

func (gate *v3RecoveryBootstrapInstallGate) wait() {
	gate.mu.Lock()
	gate.arrivals++
	if gate.arrivals == 2 {
		close(gate.ready)
	}
	release := gate.release
	gate.mu.Unlock()
	<-release
}

type v3RecoveryBootstrapPlatform struct {
	outputcap.Platform
	root outputcap.Directory
}

func v3RecoveryWrapBootstrapPlatform(
	platform outputcap.Platform,
	gate *v3RecoveryBootstrapInstallGate,
) outputcap.Platform {
	return &v3RecoveryBootstrapPlatform{
		Platform: platform,
		root:     v3RecoveryWrapBootstrapDirectory(platform.Root(), gate),
	}
}

func (platform *v3RecoveryBootstrapPlatform) Root() outputcap.Directory { return platform.root }

func (platform *v3RecoveryBootstrapPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	return platform.Platform.AcquirePublicOperationGuard()
}

type v3RecoveryBootstrapDirectory struct {
	outputcap.Directory
	gate *v3RecoveryBootstrapInstallGate
}

func v3RecoveryWrapBootstrapDirectory(
	directory outputcap.Directory,
	gate *v3RecoveryBootstrapInstallGate,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryBootstrapDirectory{Directory: directory, gate: gate}
}

func v3RecoveryUnwrapBootstrapDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*v3RecoveryBootstrapDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func (directory *v3RecoveryBootstrapDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	return v3RecoveryWrapBootstrapDirectory(duplicate, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	return directory.Directory.SameDirectory(v3RecoveryUnwrapBootstrapDirectory(other))
}

func (directory *v3RecoveryBootstrapDirectory) NamesWithPrefix(
	prefix string,
	matchLimit int,
) ([]string, error) {
	names, err := directory.Directory.NamesWithPrefix(prefix, matchLimit)
	if prefix == resumestate.BootstrapCandidatePrefix {
		// Both participants must snapshot an empty root before either is allowed
		// to construct a candidate. Platform certification cost otherwise lets
		// one participant observe and recover the other's partial construction,
		// which tests partial recovery rather than the intended install collision.
		directory.gate.waitForInitialScan()
	}
	return names, err
}

func (directory *v3RecoveryBootstrapDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	return v3RecoveryWrapBootstrapDirectory(opened, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenPinnedDirectory(expected, private)
	return v3RecoveryWrapBootstrapDirectory(opened, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	created, err := directory.Directory.CreateDirectory(name, private)
	return v3RecoveryWrapBootstrapDirectory(created, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	if name == resumestate.ControlDirectoryName {
		directory.gate.wait()
	}
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapBootstrapDirectory(candidate), name,
	)
	return v3RecoveryWrapBootstrapDirectory(installed, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	return directory.Directory.RemoveDirectory(
		name, v3RecoveryUnwrapBootstrapDirectory(expected),
	)
}

func TestOutputV3BootstrapCleanupAuthorityRacesPreserveRestartableCuts(t *testing.T) {
	tests := []struct {
		name string
		plan func(string) *outputV3ControlSessionFaultPlan
	}{
		{
			name: "inspect-before-cleanup",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSOpenDirectory, "", candidate)
			},
		},
		{
			name: "enumerate-before-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
				plan.atCall = 4
				return plan
			},
		},
		{
			name: "pin-open-before-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSOpenDirectory, "", candidate)
				plan.atCall = 2
				return plan
			},
		},
		{
			name: "pin-replaced-before-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return &outputV3ControlSessionFaultPlan{
					operation: outputV3CSSameDirectory, path: candidate, forceDifferent: true,
				}
			},
		},
		{
			name: "pin-replaced-before-root-removal",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return &outputV3ControlSessionFaultPlan{
					operation: outputV3CSSameDirectory, path: candidate,
					atCall: 4, forceDifferent: true,
				}
			},
		},
		{
			name: "enumerate-before-root-removal",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(outputV3CSNames, candidate, "")
				plan.atCall = 5
				return plan
			},
		},
		{
			name: "open-sessions-for-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(
					outputV3CSOpenDirectory, candidate, resumestate.SessionsDirectoryName,
				)
				plan.atCall = 2
				return plan
			},
		},
		{
			name: "enumerate-sessions-for-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(
					outputV3CSNames, candidate+"/"+resumestate.SessionsDirectoryName, "",
				)
			},
		},
		{
			name: "open-coordinator-for-retirement",
			plan: func(candidate string) *outputV3ControlSessionFaultPlan {
				plan := outputV3ControlSessionFailure(
					outputV3CSOpenFile, candidate, resumestate.CoordinatorLockName,
				)
				plan.atCall = 3
				return plan
			},
		},
	}

	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			authority := v3RecoveryAuthority(t, root, nil)
			platform, err := openOutputV3Platform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			installedResult, err := authority.OpenOrBootstrapControl(platform)
			installed := installedResult.Namespace
			if err != nil {
				t.Fatal(err)
			}
			expectedControl := installed.control
			if err := installed.Close(); err != nil {
				t.Fatal(err)
			}
			candidateName := v3RecoveryBootstrapCandidateName(t, byte(0x60+index))
			candidate, err := platform.Root().CreateDirectory(candidateName, true)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryBuildBootstrapPrefix(t, authority, candidate, expectedControl, 3)
			if err := errors.Join(candidate.Close(), platform.Close()); err != nil {
				t.Fatal(err)
			}

			plan := test.plan(candidateName)
			faulted := openOutputV3ControlSessionFaultPlatform(t, root, plan)
			faultResult, openErr := authority.OpenOrBootstrapControl(faulted)
			namespace := faultResult.Namespace
			if namespace != nil {
				_ = namespace.Close()
				t.Fatal("cleanup authority race returned a control namespace")
			}
			if plan.failure != nil && !errors.Is(openErr, plan.failure) {
				t.Fatalf("cleanup authority race = %v, want %v", openErr, plan.failure)
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultRoot, transfer.OutputFaultNamespaceUnsafe,
			)
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
			created := recoveryResult.Disposition == ControlInstalled
			if err != nil || created || recovered.control != expectedControl {
				_ = recoveryPlatform.Close()
				t.Fatalf("recover cleanup authority race = (created=%t, same=%t, err=%v)",
					created, recovered != nil && recovered.control == expectedControl, err)
			}
			candidates, listErr := recoveryPlatform.Root().NamesWithPrefix(
				resumestate.BootstrapCandidatePrefix, RootInspectionLimit,
			)
			if listErr != nil || len(candidates) != 0 {
				t.Fatalf("candidates after cleanup-race recovery = %v, %v", candidates, listErr)
			}
			if err := errors.Join(recovered.Close(), recoveryPlatform.Close()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
