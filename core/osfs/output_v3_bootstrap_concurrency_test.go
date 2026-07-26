package osfs

import (
	"bytes"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
)

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

	for subset := 0; subset < 1<<3; subset++ {
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
	if !errors.Is(err, errOutputRootUnsafe) || state != resumestate.BootstrapCandidateUnsafe {
		t.Fatalf("unstable coordinator lock classified (%v, %v), want unsafe", state, err)
	}
	if kind, observeErr := inspected.ObserveEntry(resumestate.SessionsDirectoryName); observeErr != nil ||
		kind != outputV3EntryAbsent {
		t.Fatalf("unsafe lock allowed candidate completion: sessions=(%v, %v)", kind, observeErr)
	}
	if err := errors.Join(inspected.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
}

func TestOutputV3ConcurrentBootstrapCollisionLeavesNoCandidate(t *testing.T) {
	rootPath := v3RecoveryRoot(t)
	gate := &v3RecoveryBootstrapInstallGate{
		ready: make(chan struct{}), release: make(chan struct{}),
	}
	defer func() {
		select {
		case <-gate.release:
		default:
			close(gate.release)
		}
	}()
	type bootstrapResult struct {
		namespace *outputControlNamespace
		platform  outputV3Platform
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
			namespace, created, err := authority.openOrBootstrapControl(wrapped)
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
		resumestate.BootstrapCandidatePrefix, outputRootInspectionLimit,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 0 {
		t.Fatalf("bootstrap collision left candidates behind: %v", candidates)
	}
	installed, err := openInstalledControl(platform.Root(), platform)
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
	authority *FilesystemOutputAuthority,
	candidate outputV3Directory,
	control resumestate.Control,
	subset int,
) {
	t.Helper()
	if subset&1 != 0 {
		encoded, err := resumestate.EncodeControl(control)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (outputStateStore{random: authority.random}).createRecord(
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
	mu       sync.Mutex
	arrivals int
	ready    chan struct{}
	release  chan struct{}
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
	outputV3Platform
	root outputV3Directory
}

func v3RecoveryWrapBootstrapPlatform(
	platform outputV3Platform,
	gate *v3RecoveryBootstrapInstallGate,
) outputV3Platform {
	return &v3RecoveryBootstrapPlatform{
		outputV3Platform: platform,
		root:             v3RecoveryWrapBootstrapDirectory(platform.Root(), gate),
	}
}

func (platform *v3RecoveryBootstrapPlatform) Root() outputV3Directory { return platform.root }

func (platform *v3RecoveryBootstrapPlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	decorated := platform.root.(*v3RecoveryBootstrapDirectory)
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return v3RecoveryWrapBootstrapDirectory(root, decorated.gate)
		},
	)
}

type v3RecoveryBootstrapDirectory struct {
	outputV3Directory
	gate *v3RecoveryBootstrapInstallGate
}

func v3RecoveryWrapBootstrapDirectory(
	directory outputV3Directory,
	gate *v3RecoveryBootstrapInstallGate,
) outputV3Directory {
	if directory == nil {
		return nil
	}
	return &v3RecoveryBootstrapDirectory{outputV3Directory: directory, gate: gate}
}

func v3RecoveryUnwrapBootstrapDirectory(directory outputV3Directory) outputV3Directory {
	if wrapped, ok := directory.(*v3RecoveryBootstrapDirectory); ok {
		return wrapped.outputV3Directory
	}
	return directory
}

func (directory *v3RecoveryBootstrapDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	return v3RecoveryWrapBootstrapDirectory(duplicate, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	return directory.outputV3Directory.SameDirectory(v3RecoveryUnwrapBootstrapDirectory(other))
}

func (directory *v3RecoveryBootstrapDirectory) OpenDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenDirectory(name, private)
	return v3RecoveryWrapBootstrapDirectory(opened, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) OpenPinnedDirectory(
	expected outputV3EntryRef,
	private bool,
) (outputV3Directory, error) {
	opened, err := directory.outputV3Directory.OpenPinnedDirectory(expected, private)
	return v3RecoveryWrapBootstrapDirectory(opened, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) CreateDirectory(
	name string,
	private bool,
) (outputV3Directory, error) {
	created, err := directory.outputV3Directory.CreateDirectory(name, private)
	return v3RecoveryWrapBootstrapDirectory(created, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) InstallDirectoryNoReplace(
	candidate outputV3Directory,
	name string,
) (outputV3Directory, error) {
	if name == resumestate.ControlDirectoryName {
		directory.gate.wait()
	}
	installed, err := directory.outputV3Directory.InstallDirectoryNoReplace(
		v3RecoveryUnwrapBootstrapDirectory(candidate), name,
	)
	return v3RecoveryWrapBootstrapDirectory(installed, directory.gate), err
}

func (directory *v3RecoveryBootstrapDirectory) RemoveDirectory(
	name string,
	expected outputV3Directory,
) error {
	return directory.outputV3Directory.RemoveDirectory(
		name, v3RecoveryUnwrapBootstrapDirectory(expected),
	)
}
