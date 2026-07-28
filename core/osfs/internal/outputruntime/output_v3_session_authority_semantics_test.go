package outputruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/outputfault"
	"github.com/windshare/windshare/core/osfs/internal/outputnamespace"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

var errOutputV3ControlSessionInjected = errors.New("injected control/session failure")

func TestOutputV3SessionSchemaCorruptionIsIntentScopedAndNonMutating(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{
			name: "unexpected-child",
			mutate: func(t *testing.T, sessionPath string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(sessionPath, "unexpected"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:   "files-wrong-type",
			mutate: outputV3ControlSessionReplaceDirectoryWithFile(resumestate.FilesDirectoryName),
		},
		{
			name:   "anchors-wrong-type",
			mutate: outputV3ControlSessionReplaceDirectoryWithFile(resumestate.AnchorsDirectoryName),
		},
		{
			name:   "stages-wrong-type",
			mutate: outputV3ControlSessionReplaceDirectoryWithFile(resumestate.StagesDirectoryName),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			sessionIDs := &v3RecoverySessionIDs{}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			opened := v3RecoveryOpen(t, authority, root, selection)
			sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
			headerPath := filepath.Join(sessionPath, resumestate.HeaderRecordName)
			headerBefore, err := os.ReadFile(headerPath)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, opened.Session)
			test.mutate(t, sessionPath)

			session, openErr := authority.OpenSelection(context.Background(), selection)
			if session != nil {
				_ = session.(*Session).closeHandles()
				t.Fatal("corrupt session schema returned a session")
			}
			if !errors.Is(openErr, outputfault.ErrIntentUnsafe) {
				t.Fatalf("session schema error = %v, want intent unsafe", openErr)
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
			)
			headerAfter, err := os.ReadFile(headerPath)
			if err != nil || !bytes.Equal(headerAfter, headerBefore) {
				t.Fatalf("session schema fault changed header = %x, %v; want %x", headerAfter, err, headerBefore)
			}

			other := v3RecoverySelection(t, true, 1)
			unrelated := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, other)
			v3RecoveryCloseSession(t, unrelated.Session)
		})
	}
}

func TestOutputV3RestartResumesEveryNonterminalSessionCut(t *testing.T) {
	t.Parallel()
	for _, lifecycle := range []resumestate.SessionLifecycle{
		resumestate.SessionPausing,
		resumestate.SessionPaused,
		resumestate.SessionPausedNeedsAttention,
	} {
		t.Run(lifecycle.String(), func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			sessionID := opened.Session.SessionID()
			if err := opened.Session.installLifecycle(resumestate.SessionPausing); err != nil {
				t.Fatal(err)
			}
			if lifecycle != resumestate.SessionPausing {
				if err := opened.Session.installLifecycle(lifecycle); err != nil {
					t.Fatal(err)
				}
			}
			v3RecoveryCloseSession(t, opened.Session)

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			if reopened.Session.SessionID() != sessionID ||
				reopened.Session.stateSnapshot().Header().Lifecycle() != resumestate.SessionActive {
				t.Fatalf("resume %v = (session=%s, lifecycle=%v)", lifecycle,
					reopened.Session.SessionID(), reopened.Session.stateSnapshot().Header().Lifecycle())
			}
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func TestOutputV3RestartSettlementFailurePreservesLifecycleCut(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, false, 0)
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	if err := opened.Session.installLifecycle(resumestate.SessionPausing); err != nil {
		t.Fatal(err)
	}
	sessionID := opened.Session.SessionID()
	headerPath := filepath.Join(
		v3RecoverySessionPath(root, selection, sessionID), resumestate.HeaderRecordName,
	)
	headerBefore, err := os.ReadFile(headerPath)
	if err != nil {
		t.Fatal(err)
	}
	v3RecoveryCloseSession(t, opened.Session)
	sessionNamespacePath := outputV3ControlSessionSessionPath(selection, sessionID)
	plan := outputV3ControlSessionFailure(
		outputV3CSCreateFile, sessionNamespacePath, resumestate.HeaderUpdateTemporaryPrefix,
	)
	plan.namePrefix = true
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	authority.platformFactory = outputV3ControlSessionFaultFactory(plan)
	session, openErr := authority.OpenSelection(context.Background(), selection)
	if session != nil {
		_ = session.(*Session).closeHandles()
		t.Fatal("failed lifecycle settlement returned a session")
	}
	if !errors.Is(openErr, errOutputV3ControlSessionInjected) {
		t.Fatalf("restart settlement failure = %v, want injected failure", openErr)
	}
	outputV3ControlSessionRequireFault(
		t, openErr, transfer.OutputFaultSession, transfer.OutputFaultStateIO,
	)
	plan.requireFired(t)
	headerAfter, err := os.ReadFile(headerPath)
	if err != nil || !bytes.Equal(headerAfter, headerBefore) {
		t.Fatalf("failed restart settlement changed header = %x, %v; want %x", headerAfter, err, headerBefore)
	}

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	if reopened.Session.stateSnapshot().Header().Lifecycle() != resumestate.SessionActive {
		t.Fatalf("lifecycle after settlement recovery = %v", reopened.Session.stateSnapshot().Header().Lifecycle())
	}
	v3RecoveryCloseSession(t, reopened.Session)
}

func TestOutputV3LockedSessionRevalidationRejectsReplacedAncestryWithoutMutation(t *testing.T) {
	t.Parallel()
	for _, target := range []string{"intent", "session"} {
		t.Run(target, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			sessionID := opened.Session.SessionID()
			headerPath := filepath.Join(
				v3RecoverySessionPath(root, selection, sessionID), resumestate.HeaderRecordName,
			)
			headerBefore, err := os.ReadFile(headerPath)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, opened.Session)

			intentPath := strings.Join([]string{
				resumestate.ControlDirectoryName,
				resumestate.SessionsDirectoryName,
				resumestate.ResumeNamespaceName(selection.ResumeIntent()),
			}, "/")
			path := intentPath
			if target == "session" {
				path += "/" + resumestate.SessionDirectoryName(sessionID)
			}
			plan := &outputV3ControlSessionFaultPlan{
				operation: outputV3CSSameDirectory, path: path, forceDifferent: true,
			}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			authority.platformFactory = outputV3ControlSessionFaultFactory(plan)
			session, openErr := authority.OpenSelection(context.Background(), selection)
			if session != nil {
				_ = session.(*Session).closeHandles()
				t.Fatal("replaced session ancestry returned a session")
			}
			if !errors.Is(openErr, outputfault.ErrIntentUnsafe) {
				t.Fatalf("replaced %s ancestry error = %v", target, openErr)
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe,
			)
			plan.requireFired(t)
			headerAfter, err := os.ReadFile(headerPath)
			if err != nil || !bytes.Equal(headerAfter, headerBefore) {
				t.Fatalf("revalidation failure changed header = %x, %v; want %x", headerAfter, err, headerBefore)
			}

			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func TestOutputV3SessionBoundaryContractsRemainTotal(t *testing.T) {
	t.Parallel()
	var session *Session
	if !session.SessionID().IsZero() || session.Capabilities() != (transfer.OutputCapabilities{}) ||
		session.stateSnapshot().Header().Lifecycle() != 0 {
		t.Fatal("nil session exposed nonzero identity, capabilities, or state")
	}
	if err := session.beginOperation(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("nil session begin error = %v", err)
	}
	session.poisonState()
	if err := session.closeHandles(); err != nil {
		t.Fatalf("nil session close = %v", err)
	}
	for name, valid := range map[string]bool{
		"00": true, "9f": true, "af": true, "": false, "0": false, "000": false,
		"AF": false, "g0": false, "-1": false,
	} {
		if actual := validStateShard(name); actual != valid {
			t.Fatalf("state shard %q valid=%t, want %t", name, actual, valid)
		}
	}
}

func TestOutputV3SessionOpenAuthorityFailuresRemainPreciselyScoped(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		context   func() context.Context
		plan      *outputV3ControlSessionFaultPlan
		wantScope transfer.OutputFaultScope
		wantCode  transfer.OutputFaultCode
		wantCause error
	}{
		{
			name: "canceled-before-coordinator",
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
			wantCause: context.Canceled,
		},
		{
			name:    "coordinator-lock-io",
			context: context.Background,
			plan: outputV3ControlSessionFailure(
				outputV3CSAcquireLock, resumestate.ControlDirectoryName, resumestate.CoordinatorLockName,
			),
			wantScope: transfer.OutputFaultRoot,
			wantCode:  transfer.OutputFaultStateIO,
		},
		{
			name:    "coordinator-lock-recreated",
			context: context.Background,
			plan: &outputV3ControlSessionFaultPlan{
				operation: outputV3CSAcquireLock, path: resumestate.ControlDirectoryName,
				name: resumestate.CoordinatorLockName, forceCreated: true,
			},
			wantScope: transfer.OutputFaultRoot,
			wantCode:  transfer.OutputFaultNamespaceUnsafe,
		},
		{
			name:    "intent-enumeration",
			context: context.Background,
			plan: outputV3ControlSessionFailure(
				outputV3CSNames,
				resumestate.ControlDirectoryName+"/"+resumestate.SessionsDirectoryName,
				"",
			),
			wantScope: transfer.OutputFaultSession,
			wantCode:  transfer.OutputFaultNamespaceUnsafe,
		},
		{
			name:    "session-enumeration",
			context: context.Background,
			plan: outputV3ControlSessionFailure(
				outputV3CSNames,
				resumestate.ControlDirectoryName+"/"+resumestate.SessionsDirectoryName+"/{intent}",
				"",
			),
			wantScope: transfer.OutputFaultSession,
			wantCode:  transfer.OutputFaultNamespaceUnsafe,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			if test.plan != nil && strings.Contains(test.plan.path, "{intent}") {
				test.plan.path = strings.ReplaceAll(
					test.plan.path, "{intent}", resumestate.ResumeNamespaceName(selection.ResumeIntent()),
				)
			}
			authority := v3RecoveryAuthority(t, root, nil)
			platform, err := openOutputRuntimeTestPlatform(root, false)
			if err != nil {
				t.Fatal(err)
			}
			controller := outputnamespace.NewController(outputnamespace.ControllerConfig{
				Backend: filesystemOutputBackendID,
				Random:  bytes.NewReader(bytes.Repeat([]byte{0x6e}, 256)),
			})
			openedControl, err := controller.OpenOrBootstrapControl(platform)
			if err != nil {
				_ = platform.Close()
				t.Fatal(err)
			}
			if err := openedControl.Namespace.Close(); err != nil {
				_ = platform.Close()
				t.Fatal(err)
			}
			admission, err := preflightOutputSelectionAdmission(platform, selection)
			if err != nil {
				_ = platform.Close()
				t.Fatal(err)
			}
			platform = wrapOutputV3ControlSessionFaultPlatform(platform, test.plan)
			validation, err := prepareOutputSelectionAncestry(platform, selection)
			if err != nil {
				_ = platform.Close()
				t.Fatal(err)
			}
			admission.ancestry = validation.snapshot
			admission.validation = validation
			control, err := controller.OpenInstalledControl(platform.Root(), platform)
			if err != nil {
				_ = authority.closeOutputAdmissionAncestry(&admission)
				_ = platform.Close()
				t.Fatal(err)
			}

			session, _, _, openErr := authority.openOutputSession(test.context(), platform, control, admission)
			if session != nil {
				if err := errors.Join(
					session.closeHandles(), authority.closeOutputAdmissionAncestry(&admission),
				); err != nil {
					t.Fatal(err)
				}
				t.Fatal("authority-boundary failure returned a session")
			}
			if err := errors.Join(
				authority.closeOutputAdmissionAncestry(&admission), control.Close(), platform.Close(),
			); err != nil {
				t.Fatal(err)
			}
			if test.wantCause != nil {
				if !errors.Is(openErr, test.wantCause) {
					t.Fatalf("open error = %v, want %v", openErr, test.wantCause)
				}
				return
			}
			outputV3ControlSessionRequireFault(t, openErr, test.wantScope, test.wantCode)
			test.plan.requireFired(t)
		})
	}
}

func TestOutputV3SessionLockAuthorityCutsBlockWithoutHeaderMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		plan      func(string) *outputV3ControlSessionFaultPlan
		wrongType bool
		wantCode  transfer.OutputFaultCode
	}{
		{
			name: "observation-io",
			plan: func(path string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSClassifyEntry, path, resumestate.SessionLockName)
			},
			wantCode: transfer.OutputFaultNamespaceUnsafe,
		},
		{name: "wrong-type", wrongType: true, wantCode: transfer.OutputFaultNamespaceUnsafe},
		{
			name: "acquire-io",
			plan: func(path string) *outputV3ControlSessionFaultPlan {
				return outputV3ControlSessionFailure(outputV3CSAcquireLock, path, resumestate.SessionLockName)
			},
			wantCode: transfer.OutputFaultStateIO,
		},
		{
			name: "acquire-recreated",
			plan: func(path string) *outputV3ControlSessionFaultPlan {
				return &outputV3ControlSessionFaultPlan{
					operation: outputV3CSAcquireLock, path: path,
					name: resumestate.SessionLockName, forceCreated: true,
				}
			},
			wantCode: transfer.OutputFaultNamespaceUnsafe,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := v3RecoveryRoot(t)
			selection := v3RecoverySelection(t, false, 0)
			sessionIDs := &v3RecoverySessionIDs{}
			opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			sessionID := opened.Session.SessionID()
			sessionPath := v3RecoverySessionPath(root, selection, sessionID)
			headerPath := filepath.Join(sessionPath, resumestate.HeaderRecordName)
			headerBefore, err := os.ReadFile(headerPath)
			if err != nil {
				t.Fatal(err)
			}
			v3RecoveryCloseSession(t, opened.Session)

			lockPath := filepath.Join(sessionPath, resumestate.SessionLockName)
			if test.wrongType {
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(lockPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			logicalPath := outputV3ControlSessionSessionPath(selection, sessionID)
			var plan *outputV3ControlSessionFaultPlan
			if test.plan != nil {
				plan = test.plan(logicalPath)
			}
			authority := v3RecoveryAuthority(t, root, sessionIDs)
			authority.platformFactory = outputV3ControlSessionFaultFactory(plan)
			session, openErr := authority.OpenSelection(context.Background(), selection)
			if session != nil {
				_ = session.(*Session).closeHandles()
				t.Fatal("invalid session lock cut returned a session")
			}
			outputV3ControlSessionRequireFault(
				t, openErr, transfer.OutputFaultSession, test.wantCode,
			)
			if plan != nil {
				plan.requireFired(t)
			}
			headerAfter, err := os.ReadFile(headerPath)
			if err != nil || !bytes.Equal(headerAfter, headerBefore) {
				t.Fatalf("lock authority failure changed header = %x, %v; want %x", headerAfter, err, headerBefore)
			}

			if test.wrongType {
				return
			}
			reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
			v3RecoveryCloseSession(t, reopened.Session)
		})
	}
}

func outputV3ControlSessionReplaceDirectoryWithFile(name string) func(*testing.T, string) {
	return func(t *testing.T, sessionPath string) {
		t.Helper()
		path := filepath.Join(sessionPath, name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("wrong-type"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func outputV3ControlSessionRequireFault(
	t *testing.T,
	err error,
	scope transfer.OutputFaultScope,
	code transfer.OutputFaultCode,
) {
	t.Helper()
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != scope || fault.Code() != code {
		t.Fatalf("output fault = %#v in %v, want scope=%v code=%v", fault, err, scope, code)
	}
}

func outputV3ControlSessionSessionPath(
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
) string {
	return strings.Join([]string{
		resumestate.ControlDirectoryName,
		resumestate.SessionsDirectoryName,
		resumestate.ResumeNamespaceName(selection.ResumeIntent()),
		resumestate.SessionDirectoryName(sessionID),
	}, "/")
}

const (
	outputV3CSRootBinding      = "root-binding"
	outputV3CSNames            = "names"
	outputV3CSNamesWithPrefix  = "names-with-prefix"
	outputV3CSObserveEntry     = "observe-entry"
	outputV3CSClassifyEntry    = "classify-entry"
	outputV3CSOpenDirectory    = "open-directory"
	outputV3CSCreateDirectory  = "create-directory"
	outputV3CSInstallDirectory = "install-directory"
	outputV3CSRemoveDirectory  = "remove-directory"
	outputV3CSOpenFile         = "open-file"
	outputV3CSCreateFile       = "create-file"
	outputV3CSRemoveFile       = "remove-file"
	outputV3CSAcquireLock      = "acquire-lock"
	outputV3CSSameDirectory    = "same-directory"
	outputV3CSSync             = "sync"
)

type outputV3ControlSessionFaultPlan struct {
	mu             sync.Mutex
	operation      string
	path           string
	name           string
	namePrefix     bool
	atCall         int
	seen           int
	fired          int
	failure        error
	forceDifferent bool
	forceCreated   bool
}

func outputV3ControlSessionFailure(operation, path, name string) *outputV3ControlSessionFaultPlan {
	return &outputV3ControlSessionFaultPlan{
		operation: operation, path: path, name: name, failure: errOutputV3ControlSessionInjected,
	}
}

func (plan *outputV3ControlSessionFaultPlan) trigger(operation, path, name string) (bool, error) {
	if plan == nil {
		return false, nil
	}
	plan.mu.Lock()
	defer plan.mu.Unlock()
	nameMatches := name == plan.name
	if plan.namePrefix {
		nameMatches = strings.HasPrefix(name, plan.name)
	}
	if operation != plan.operation || path != plan.path || !nameMatches {
		return false, nil
	}
	plan.seen++
	atCall := plan.atCall
	if atCall == 0 {
		atCall = 1
	}
	if plan.seen != atCall {
		return false, nil
	}
	plan.fired++
	return true, plan.failure
}

func (plan *outputV3ControlSessionFaultPlan) requireFired(t *testing.T) {
	t.Helper()
	plan.mu.Lock()
	defer plan.mu.Unlock()
	if plan.fired != 1 {
		t.Fatalf("fault %s path=%q name=%q fired %d times, want once", plan.operation, plan.path, plan.name, plan.fired)
	}
}

type outputV3ControlSessionFaultPlatform struct {
	outputcap.Platform
	root outputcap.Directory
	plan *outputV3ControlSessionFaultPlan
}

func wrapOutputV3ControlSessionFaultPlatform(
	platform outputcap.Platform,
	plan *outputV3ControlSessionFaultPlan,
) outputcap.Platform {
	return &outputV3ControlSessionFaultPlatform{
		Platform: platform,
		root:     wrapOutputV3ControlSessionFaultDirectory(platform.Root(), plan, ""),
		plan:     plan,
	}
}

func outputV3ControlSessionFaultFactory(
	plan *outputV3ControlSessionFaultPlan,
) func(string, bool) (outputcap.Platform, error) {
	return func(path string, create bool) (outputcap.Platform, error) {
		platform, err := openOutputRuntimeTestPlatform(path, create)
		if err != nil {
			return nil, err
		}
		return wrapOutputV3ControlSessionFaultPlatform(platform, plan), nil
	}
}

func (platform *outputV3ControlSessionFaultPlatform) Root() outputcap.Directory { return platform.root }

func (platform *outputV3ControlSessionFaultPlatform) AcquirePublicOperationGuard() (
	outputcap.PublicOperationGuard,
	error,
) {
	return acquireRuntimeTestDecoratedPublicOperationGuard(
		platform.Platform,
		func(root outputcap.Directory) outputcap.Directory {
			return wrapOutputV3ControlSessionFaultDirectory(root, platform.plan, "")
		},
	)
}

func (platform *outputV3ControlSessionFaultPlatform) RootBinding() (resumestate.OutputRootBinding, error) {
	if matched, err := platform.plan.trigger(outputV3CSRootBinding, "", ""); matched {
		return resumestate.OutputRootBinding{}, err
	}
	return platform.Platform.RootBinding()
}

type outputV3ControlSessionFaultDirectory struct {
	outputcap.Directory
	plan *outputV3ControlSessionFaultPlan
	path string
}

func wrapOutputV3ControlSessionFaultDirectory(
	directory outputcap.Directory,
	plan *outputV3ControlSessionFaultPlan,
	path string,
) outputcap.Directory {
	if directory == nil {
		return nil
	}
	return &outputV3ControlSessionFaultDirectory{Directory: directory, plan: plan, path: path}
}

func unwrapOutputV3ControlSessionFaultDirectory(directory outputcap.Directory) outputcap.Directory {
	if wrapped, ok := directory.(*outputV3ControlSessionFaultDirectory); ok {
		return wrapped.Directory
	}
	return directory
}

func outputV3ControlSessionChildPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "/" + name
}

func (directory *outputV3ControlSessionFaultDirectory) Duplicate() (outputcap.Directory, error) {
	duplicate, err := directory.Directory.Duplicate()
	return wrapOutputV3ControlSessionFaultDirectory(duplicate, directory.plan, directory.path), err
}

func (directory *outputV3ControlSessionFaultDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSSameDirectory, directory.path, ""); matched {
		if err != nil {
			return false, err
		}
		return !directory.plan.forceDifferent, nil
	}
	return directory.Directory.SameDirectory(unwrapOutputV3ControlSessionFaultDirectory(other))
}

func (directory *outputV3ControlSessionFaultDirectory) Names(limit int) ([]string, error) {
	if matched, err := directory.plan.trigger(outputV3CSNames, directory.path, ""); matched {
		return nil, err
	}
	return directory.Directory.Names(limit)
}

func (directory *outputV3ControlSessionFaultDirectory) NamesWithPrefix(
	prefix string,
	limit int,
) ([]string, error) {
	if matched, err := directory.plan.trigger(outputV3CSNamesWithPrefix, directory.path, prefix); matched {
		return nil, err
	}
	return directory.Directory.NamesWithPrefix(prefix, limit)
}

func (directory *outputV3ControlSessionFaultDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	if matched, err := directory.plan.trigger(outputV3CSObserveEntry, directory.path, name); matched {
		return outputcap.EntryAbsent, err
	}
	return directory.Directory.ObserveEntry(name)
}

func (directory *outputV3ControlSessionFaultDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSClassifyEntry, directory.path, name); matched {
		return outputcap.EntryAbsent, false, err
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *outputV3ControlSessionFaultDirectory) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSOpenDirectory, directory.path, name); matched {
		return nil, err
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	return wrapOutputV3ControlSessionFaultDirectory(
		opened, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenPinnedDirectory(expected, private)
	return wrapOutputV3ControlSessionFaultDirectory(opened, directory.plan, directory.path), err
}

func (directory *outputV3ControlSessionFaultDirectory) CreateDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSCreateDirectory, directory.path, name); matched {
		return nil, err
	}
	created, err := directory.Directory.CreateDirectory(name, private)
	return wrapOutputV3ControlSessionFaultDirectory(
		created, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) InstallDirectoryNoReplace(
	candidate outputcap.Directory,
	name string,
) (outputcap.Directory, error) {
	if matched, err := directory.plan.trigger(outputV3CSInstallDirectory, directory.path, name); matched {
		return nil, err
	}
	installed, err := directory.Directory.InstallDirectoryNoReplace(
		unwrapOutputV3ControlSessionFaultDirectory(candidate), name,
	)
	return wrapOutputV3ControlSessionFaultDirectory(
		installed, directory.plan, outputV3ControlSessionChildPath(directory.path, name),
	), err
}

func (directory *outputV3ControlSessionFaultDirectory) RemoveDirectory(
	name string,
	expected outputcap.Directory,
) error {
	if matched, err := directory.plan.trigger(outputV3CSRemoveDirectory, directory.path, name); matched {
		return err
	}
	return directory.Directory.RemoveDirectory(
		name, unwrapOutputV3ControlSessionFaultDirectory(expected),
	)
}

func (directory *outputV3ControlSessionFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	if matched, err := directory.plan.trigger(outputV3CSOpenFile, directory.path, name); matched {
		return nil, err
	}
	return directory.Directory.OpenFile(name, private, writable)
}

func (directory *outputV3ControlSessionFaultDirectory) CreateFile(
	name string,
	private bool,
	size int64,
) (outputcap.File, error) {
	if matched, err := directory.plan.trigger(outputV3CSCreateFile, directory.path, name); matched {
		return nil, err
	}
	return directory.Directory.CreateFile(name, private, size)
}

func (directory *outputV3ControlSessionFaultDirectory) RemoveFile(name string, expected outputcap.File) error {
	if matched, err := directory.plan.trigger(outputV3CSRemoveFile, directory.path, name); matched {
		return err
	}
	return directory.Directory.RemoveFile(name, expected)
}

func (directory *outputV3ControlSessionFaultDirectory) AcquireLock(
	name string,
	existingOnly bool,
) (outputcap.Lock, bool, error) {
	if matched, err := directory.plan.trigger(outputV3CSAcquireLock, directory.path, name); matched {
		if err != nil || !directory.plan.forceCreated {
			return nil, false, err
		}
		lock, _, lockErr := directory.Directory.AcquireLock(name, existingOnly)
		return lock, true, lockErr
	}
	return directory.Directory.AcquireLock(name, existingOnly)
}

func (directory *outputV3ControlSessionFaultDirectory) Sync() error {
	if matched, err := directory.plan.trigger(outputV3CSSync, directory.path, ""); matched {
		return err
	}
	return directory.Directory.Sync()
}

func (directory *outputV3ControlSessionFaultDirectory) ValidateCreateAuthority() error {
	if validator, ok := directory.Directory.(outputcap.CreateAuthorityValidator); ok {
		return validator.ValidateCreateAuthority()
	}
	return nil
}

func (directory *outputV3ControlSessionFaultDirectory) ValidateMetadataAuthority() error {
	if validator, ok := directory.Directory.(outputcap.MetadataAuthorityValidator); ok {
		return validator.ValidateMetadataAuthority()
	}
	return nil
}

func (directory *outputV3ControlSessionFaultDirectory) ValidatePublicEntryNames(names []string) error {
	if validator, ok := directory.Directory.(outputcap.PublicEntryNamesValidator); ok {
		return validator.ValidatePublicEntryNames(names)
	}
	for _, name := range names {
		if err := directory.Directory.ValidatePublicEntryName(name); err != nil {
			return err
		}
	}
	return nil
}
