package osfs

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestCanonicalOutputAncestryPathsCoversSelectionClosureExactlyOnce(t *testing.T) {
	selection := outputAncestryTestSelection(t)
	paths, err := canonicalOutputAncestryPaths(selection)
	want := []string{"", "a", "a/b", "empty"}
	if err != nil || !reflect.DeepEqual(paths, want) {
		t.Fatalf("ancestry paths = %q, %v; want %q", paths, err, want)
	}
}

func TestOutputAncestryPreparationAndRebindUseExactOpaqueClaims(t *testing.T) {
	selection := outputAncestryTestSelection(t)
	platform := newOutputAncestryTestPlatform(t)
	prepared, err := prepareOutputSelectionAncestry(platform, selection)
	if err != nil {
		t.Fatal(err)
	}
	boundPaths := []string{"", "a", "a/b", "empty"}
	if got := outputAncestryEntryPaths(prepared.snapshot.entries); !reflect.DeepEqual(got, boundPaths) {
		t.Fatalf("prepared ancestry paths = %q", got)
	}
	for _, path := range boundPaths {
		node := platform.nodes[path]
		if node.prepareCalls != 1 || node.identityCalls != 0 {
			t.Fatalf("prepared node %q calls = prepare %d, identity %d", path, node.prepareCalls, node.identityCalls)
		}
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}

	requirement := outputAncestryRequirement{path: "a/b", authority: outputAncestryCreateAuthority}
	validated, err := validateOutputSelectionAncestry(platform, selection, prepared.snapshot, requirement)
	if err != nil {
		t.Fatal(err)
	}
	if platform.nodes["a/b"].createAuthorityCalls != 1 {
		t.Fatalf("create authority calls = %d", platform.nodes["a/b"].createAuthorityCalls)
	}
	for _, path := range boundPaths {
		node := platform.nodes[path]
		if node.prepareCalls != 2 || node.identityCalls != 0 {
			t.Fatalf("rebound node %q calls = prepare %d, identity %d", path, node.prepareCalls, node.identityCalls)
		}
	}
	platform.nodes["a/b"].claim = []byte("changed-on-retained-handle")
	if err := validated.revalidateRetainedDirectory("a/b", outputAncestryCreateAuthority); !errors.Is(err, errOutputAncestryUnsafe) {
		t.Fatalf("retained claim mismatch error = %v", err)
	}
	if err := validated.Close(); err != nil {
		t.Fatal(err)
	}
	if platform.guardAcquires != 2 || platform.guardCloses != 2 {
		t.Fatalf("guard lifecycle = acquires %d closes %d", platform.guardAcquires, platform.guardCloses)
	}
}

func TestOutputAncestryRestartPreparationRejectsReplacementBinding(t *testing.T) {
	selection := outputAncestryTestSelection(t)
	platform := newOutputAncestryTestPlatform(t)
	prepared, err := prepareOutputSelectionAncestry(platform, selection)
	if err != nil {
		t.Fatal(err)
	}
	expected := prepared.snapshot
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}

	replacement := &outputAncestryTestNode{claim: []byte("replacement-b"), prepared: true, children: map[string]*outputAncestryTestNode{}}
	platform.nodes["a"].children["b"] = replacement
	platform.nodes["a/b"] = replacement
	// Preparation captures the candidate binding; the durable header comparison
	// is the admission cut that rejects it.
	validation, err := prepareOutputSelectionAncestry(platform, selection)
	if err != nil {
		t.Fatalf("replacement preparation = %v", err)
	}
	if validation == nil {
		t.Fatal("replacement preparation returned no validation")
	}
	if validation.snapshot.matches(expected) {
		t.Fatal("replacement preparation matched the admitted ancestry")
	}
	if err := validation.Close(); err != nil {
		t.Fatal(err)
	}
	if replacement.prepareCalls != 1 || replacement.identityCalls != 0 {
		t.Fatalf("replacement calls = prepare %d, identity %d", replacement.prepareCalls, replacement.identityCalls)
	}
	if platform.guardAcquires != 2 || platform.guardCloses != 2 {
		t.Fatalf("replacement guard lifecycle = acquires %d closes %d", platform.guardAcquires, platform.guardCloses)
	}
}

func TestOutputAncestryGuardCloseFailureIsNeverIdentityAuthority(t *testing.T) {
	selection := outputAncestryTestSelection(t)
	platform := newOutputAncestryTestPlatform(t)
	validation, err := prepareOutputSelectionAncestry(platform, selection)
	if err != nil {
		t.Fatal(err)
	}
	platform.guardCloseErr = errors.New("guard close failed")
	closeErr := closeOutputAncestryValidation(validation)
	if !errors.Is(closeErr, platform.guardCloseErr) || errors.Is(closeErr, errOutputAncestryUnsafe) {
		t.Fatalf("guard close error = %v", closeErr)
	}
	classified := outputAncestryCleanupFault("close guarded ancestry", closeErr)
	var fault *transfer.OutputFault
	var sessionErr *transfer.OutputSessionError
	if !errors.As(classified, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultStateIO || !errors.As(classified, &sessionErr) ||
		!sessionErr.RequiresJobPause() || errors.Is(classified, errOutputAncestryUnsafe) {
		t.Fatalf("classified guard close error = %v", classified)
	}
}

func TestOutputAncestryOperationFaultSeparatesDenialFromContradiction(t *testing.T) {
	denial := errors.Join(
		errOutputAncestryAuthorityDenied,
		errOutputV3Unsafe,
		errors.New("temporary ancestry denial"),
	)
	denied := outputAncestryOperationFault("revalidate ancestry", denial)
	var deniedFault *transfer.OutputFault
	var deniedSession *transfer.OutputSessionError
	if !errors.As(denied, &deniedFault) || deniedFault.Scope() != transfer.OutputFaultSession ||
		deniedFault.Code() != transfer.OutputFaultStateIO ||
		!errors.As(denied, &deniedSession) || !deniedSession.RequiresJobPause() ||
		errors.Is(denied, errOutputIntentUnsafe) {
		t.Fatalf("operational ancestry denial = %v", denied)
	}

	contradiction := errors.Join(errOutputAncestryUnsafe, errOutputAncestryMismatch)
	unsafe := outputAncestryOperationFault("revalidate ancestry", contradiction)
	var unsafeFault *transfer.OutputFault
	var unsafeSession *transfer.OutputSessionError
	if !errors.As(unsafe, &unsafeFault) || unsafeFault.Scope() != transfer.OutputFaultSession ||
		unsafeFault.Code() != transfer.OutputFaultNamespaceUnsafe ||
		!errors.As(unsafe, &unsafeSession) || !unsafeSession.RequiresJobPause() ||
		!errors.Is(unsafe, errOutputIntentUnsafe) {
		t.Fatalf("structural ancestry contradiction = %v", unsafe)
	}
}

func TestOutputAncestryTraceExposesOnlyStableAggregateContext(t *testing.T) {
	selection := outputAncestryTestSelection(t)
	platform := newOutputAncestryTestPlatform(t)
	validation, err := prepareOutputSelectionAncestry(platform, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := validation.Close(); err != nil {
			t.Errorf("close ancestry validation: %v", err)
		}
	}()
	sessionID := v3RecoveryIdentity16[transfer.OutputSessionID](0x4a)
	locator := resumestate.DigestCanonicalLocator("a/b/file.bin")
	var events []FilesystemOutputTrace
	authority := &FilesystemOutputAuthority{tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		events = append(events, event)
	})}
	authority.traceOutputAncestry(
		selection, sessionID, locator, validation.snapshot, len(validation.snapshot.entries),
		FilesystemOutputAncestryPublicationPre, FilesystemOutputAncestryMatched,
	)
	if len(events) != 1 {
		t.Fatalf("ancestry trace count = %d", len(events))
	}
	event := events[0]
	if event.Operation != TraceAncestryValidation || event.ResumeIntent != selection.ResumeIntent() ||
		event.SessionID != sessionID || event.LocatorDigest != outputLocatorDigestFromState(locator) ||
		event.SelectionIdentity != selection.Identity() ||
		event.OutputAncestryDigest != filesystemOutputAncestryDigestFromState(validation.snapshot.binding) ||
		event.AncestryBoundary != FilesystemOutputAncestryPublicationPre ||
		event.AncestryDecision != FilesystemOutputAncestryMatched ||
		event.AncestryClaimCount != uint32(len(validation.snapshot.entries)) || event.Failed {
		t.Fatalf("ancestry trace = %+v", event)
	}
	formatted := fmt.Sprintf("%+v", event)
	for _, secret := range []string{"claim:", "a/b", "file.bin"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("ancestry trace disclosed raw claim or path %q: %s", secret, formatted)
		}
	}
}

func TestOutputAncestryTraceDecisionIsSemanticallyStable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FilesystemOutputAncestryDecision
	}{
		{name: "matched", want: FilesystemOutputAncestryMatched},
		{name: "mismatch", err: errors.Join(errOutputAncestryUnsafe, errOutputAncestryMismatch), want: FilesystemOutputAncestryMismatch},
		{name: "authority", err: errors.Join(errOutputAncestryUnsafe, errOutputAncestryAuthorityDenied), want: FilesystemOutputAncestryAuthorityDenied},
		{name: "structural", err: errOutputAncestryUnsafe, want: FilesystemOutputAncestryStructuralUnsafe},
		{name: "operational", err: errors.New("temporary denial"), want: FilesystemOutputAncestryAuthorityDenied},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := outputAncestryTraceDecision(test.err); got != test.want {
				t.Fatalf("trace decision = %v, want %v", got, test.want)
			}
		})
	}
}

func TestOutputAncestryAdmissionFaultRequiresExplicitPauseOnlyForExistingState(t *testing.T) {
	cause := errors.Join(errOutputAncestryUnsafe, errOutputAncestryMismatch)
	for _, test := range []struct {
		name           string
		preserveState  bool
		wantExplicitly bool
		wantPause      bool
	}{
		{name: "fresh selection", preserveState: false, wantExplicitly: false, wantPause: false},
		{name: "matching state", preserveState: true, wantExplicitly: true, wantPause: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := outputAncestrySessionFault("bind ancestry", cause, test.preserveState)
			var fault *transfer.OutputFault
			if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
				fault.Code() != transfer.OutputFaultNamespaceUnsafe {
				t.Fatalf("ancestry fault = %v", err)
			}
			var sessionErr *transfer.OutputSessionError
			if got := errors.As(err, &sessionErr); got != test.wantExplicitly {
				t.Fatalf("explicit pause wrapper = %v, want %v", got, test.wantExplicitly)
			}
			if sessionErr != nil && !sessionErr.RequiresJobPause() {
				t.Fatal("matching durable state did not require preservation pause")
			}
			var requirement interface{ RequiresJobPause() bool }
			if !errors.As(err, &requirement) || requirement.RequiresJobPause() != test.wantPause {
				t.Fatalf("effective pause requirement = %v, want %v", requirement, test.wantPause)
			}
		})
	}
}

func TestOutputAncestryRestartReplacementPreservesIntentAndResumesAfterRestore(t *testing.T) {
	root := v3RecoveryRoot(t)
	selection := outputAncestryTestSelection(t)
	authority := v3RecoveryAuthority(t, root, nil)
	opened := v3RecoveryOpen(t, authority, root, selection)
	session := opened.Session
	sessionID := session.SessionID()
	original := filepath.Join(root, "a", "b")
	displaced := filepath.Join(root, "a", "b.admitted")
	sentinel := filepath.Join(original, "existing-user-file")
	if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	v3RecoveryCloseSession(t, session)

	if err := os.Rename(original, displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	reopened, err := v3OpenSelection(context.Background(), authority, selection)
	if err == nil || reopened.Session != nil {
		t.Fatalf("replacement restart = %+v, %v", reopened, err)
	}
	var sessionErr *transfer.OutputSessionError
	if !errors.As(err, &sessionErr) || !sessionErr.RequiresJobPause() {
		t.Fatalf("replacement restart did not require intent-preserving pause: %v", err)
	}
	var fault *transfer.OutputFault
	if !errors.As(err, &fault) || fault.Scope() != transfer.OutputFaultSession ||
		fault.Code() != transfer.OutputFaultNamespaceUnsafe {
		t.Fatalf("replacement restart fault = %v", err)
	}
	if entries, readErr := os.ReadDir(original); readErr != nil || len(entries) != 0 {
		t.Fatalf("replacement received output content: entries=%v err=%v", entries, readErr)
	}
	if data, readErr := os.ReadFile(filepath.Join(displaced, "existing-user-file")); readErr != nil || string(data) != "preserve" {
		t.Fatalf("admitted directory content changed: data=%q err=%v", data, readErr)
	}
	sessionPath := v3RecoverySessionPath(root, selection, sessionID)
	if _, statErr := os.Stat(filepath.Join(sessionPath, resumestate.HeaderRecordName)); statErr != nil {
		t.Fatalf("matching session header was not preserved: %v", statErr)
	}
	intentPath := filepath.Dir(sessionPath)
	if entries, readErr := os.ReadDir(intentPath); readErr != nil || len(entries) != 1 ||
		entries[0].Name() != resumestate.SessionDirectoryName(sessionID) {
		t.Fatalf("matching intent gained a competing session: entries=%v err=%v", entries, readErr)
	}

	if err := os.Remove(original); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(displaced, original); err != nil {
		t.Fatal(err)
	}
	restored := v3RecoveryOpen(t, authority, root, selection)
	if restored.Session.SessionID() != sessionID {
		t.Fatalf("restored ancestry session = %x, want %x", restored.Session.SessionID(), sessionID)
	}
	v3RecoveryCloseSession(t, restored.Session)
}

func TestOutputAncestrySessionFinalizeRejectsMismatchBeforeLifecycleMutation(t *testing.T) {
	selection := outputAncestryTestSelection(t)
	platform := newOutputAncestryTestPlatform(t)
	prepared, err := prepareOutputSelectionAncestry(platform, selection)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := prepared.snapshot
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	control, err := resumestate.NewControl(resumestate.ControlSpec{
		Backend: filesystemOutputBackendID, OutputRoot: platform.rootBinding,
		Certification: platform.Certification(), Durability: transfer.DurabilityProcessRestart,
		Generation: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := v3RecoveryIdentity16[transfer.OutputSessionID](0x5a)
	header, err := resumestate.NewHeader(resumestate.HeaderSpec{
		Backend: filesystemOutputBackendID, SessionID: sessionID, Selection: selection,
		OutputRoot: platform.rootBinding, OutputAncestry: snapshot.binding,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := resumestate.BindSessionAuthority(
		control, header, selection, resumestate.ResumeNamespaceName(selection.ResumeIntent()),
		resumestate.SessionDirectoryName(sessionID),
	)
	if err != nil {
		t.Fatal(err)
	}
	var events []FilesystemOutputTrace
	authority := &FilesystemOutputAuthority{tracer: FilesystemOutputTraceFunc(func(event FilesystemOutputTrace) {
		events = append(events, event)
	})}
	session := &filesystemOutputSession{
		owner: authority, platform: platform, state: state,
		sessionID: sessionID, selection: selection, resumeIntent: selection.ResumeIntent(), ancestry: snapshot,
	}
	platform.nodes["a/b"].claim = []byte("replacement-before-complete")
	settlement, err := session.CompleteJob(context.Background(), transfer.JobSucceeded)
	if err == nil || settlement.Kind() != 0 {
		t.Fatalf("mismatched completion = settlement %v, error %v", settlement, err)
	}
	var sessionErr *transfer.OutputSessionError
	if !errors.As(err, &sessionErr) || !sessionErr.RequiresJobPause() {
		t.Fatalf("mismatched completion did not require preservation pause: %v", err)
	}
	if got := session.stateSnapshot().Header().Lifecycle(); got != resumestate.SessionActive {
		t.Fatalf("completion mutated lifecycle to %v before ancestry validation", got)
	}
	found := false
	for _, event := range events {
		if event.Operation == TraceAncestryValidation &&
			event.AncestryBoundary == FilesystemOutputAncestrySessionFinalize &&
			event.AncestryDecision == FilesystemOutputAncestryMismatch && event.SessionID == sessionID {
			found = true
		}
	}
	if !found {
		t.Fatalf("session-finalize mismatch trace absent: %+v", events)
	}
}

func outputAncestryEntryPaths(entries []outputAncestryEntry) []string {
	paths := make([]string, len(entries))
	for index := range entries {
		paths[index] = entries[index].path
	}
	return paths
}

func outputAncestryTestSelection(t *testing.T) transfer.OutputSelection {
	t.Helper()
	share := v3RecoveryIdentity16[catalog.ShareInstance](0x31)
	root := v3RecoveryIdentity16[catalog.DirectoryID](0x32)
	generation := v3RecoveryIdentity16[catalog.DirectoryGeneration](0x33)
	directories := []transfer.OutputSelectionDirectory{
		{Path: "a", DirectoryID: v3RecoveryIdentity16[catalog.DirectoryID](0x34), Generation: v3RecoveryIdentity16[catalog.DirectoryGeneration](0x35)},
		{Path: "a/b", DirectoryID: v3RecoveryIdentity16[catalog.DirectoryID](0x36), Generation: v3RecoveryIdentity16[catalog.DirectoryGeneration](0x37)},
		{Path: "empty", DirectoryID: v3RecoveryIdentity16[catalog.DirectoryID](0x38), Generation: v3RecoveryIdentity16[catalog.DirectoryGeneration](0x39)},
	}
	files := []transfer.OutputSelectionFile{
		{
			Path: "a/b/file.bin", FileID: v3RecoveryIdentity16[catalog.FileID](0x3a),
			ParentDirectoryID: directories[1].DirectoryID, ParentGeneration: directories[1].Generation,
			ExpectedSize: 8,
		},
	}
	plan, err := transfer.NewOutputSelection(share, root, generation, directories, files)
	if err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transfer.NewCanonicalSelectionRequest(share, root, rules)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := transfer.NewCanonicalSelectionV1(request, plan)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := canonical.BindPlan(plan)
	if err != nil {
		t.Fatal(err)
	}
	return selection
}

type outputAncestryTestNode struct {
	claim                  []byte
	prepared               bool
	children               map[string]*outputAncestryTestNode
	prepareCalls           int
	identityCalls          int
	createAuthorityCalls   int
	metadataAuthorityCalls int
}

type outputAncestryTestDirectory struct {
	outputV3Directory
	node *outputAncestryTestNode
}

func (directory *outputAncestryTestDirectory) Close() error { return nil }

func (directory *outputAncestryTestDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	current, ok := other.(*outputAncestryTestDirectory)
	return ok && current.node == directory.node, nil
}

func (directory *outputAncestryTestDirectory) OpenDirectory(name string, _ bool) (outputV3Directory, error) {
	child := directory.node.children[name]
	if child == nil {
		return nil, fs.ErrNotExist
	}
	return &outputAncestryTestDirectory{node: child}, nil
}

func (directory *outputAncestryTestDirectory) PrepareIdentityClaim() ([]byte, error) {
	directory.node.prepareCalls++
	directory.node.prepared = true
	return append([]byte(nil), directory.node.claim...), nil
}

func (directory *outputAncestryTestDirectory) IdentityClaim() ([]byte, error) {
	directory.node.identityCalls++
	if !directory.node.prepared {
		return nil, errors.New("identity claim is not prepared")
	}
	return append([]byte(nil), directory.node.claim...), nil
}

func (directory *outputAncestryTestDirectory) ValidateCreateAuthority() error {
	directory.node.createAuthorityCalls++
	return nil
}

func (directory *outputAncestryTestDirectory) ValidateMetadataAuthority() error {
	directory.node.metadataAuthorityCalls++
	return nil
}

type outputAncestryTestPlatform struct {
	outputV3Platform
	root          *outputAncestryTestDirectory
	nodes         map[string]*outputAncestryTestNode
	rootBinding   resumestate.OutputRootBinding
	guardAcquires int
	guardCloses   int
	guardCloseErr error
}

type outputAncestryPlatformDecorator struct {
	outputV3Platform
}

var _ outputV3Platform = (*outputAncestryPlatformDecorator)(nil)

func newOutputAncestryTestPlatform(t *testing.T) *outputAncestryTestPlatform {
	t.Helper()
	nodes := map[string]*outputAncestryTestNode{}
	for _, path := range []string{"", "a", "a/b", "empty"} {
		nodes[path] = &outputAncestryTestNode{
			claim: []byte("claim:" + path), children: map[string]*outputAncestryTestNode{},
		}
	}
	nodes[""].children["a"] = nodes["a"]
	nodes[""].children["empty"] = nodes["empty"]
	nodes["a"].children["b"] = nodes["a/b"]
	binding, err := resumestate.NewOutputRootBinding(
		resumestate.CertificationLinuxExt4ProcessRestart,
		[]byte("test-volume"), []byte("test-root"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &outputAncestryTestPlatform{
		root: &outputAncestryTestDirectory{node: nodes[""]}, nodes: nodes, rootBinding: binding,
	}
}

func (platform *outputAncestryTestPlatform) Root() outputV3Directory { return platform.root }
func (platform *outputAncestryTestPlatform) Close() error            { return nil }
func (platform *outputAncestryTestPlatform) RootBinding() (resumestate.OutputRootBinding, error) {
	return platform.rootBinding, nil
}
func (platform *outputAncestryTestPlatform) Certification() resumestate.CertificationID {
	return resumestate.CertificationLinuxExt4ProcessRestart
}
func (platform *outputAncestryTestPlatform) AcquirePublicOperationGuard() (outputV3PublicOperationGuard, error) {
	platform.guardAcquires++
	return &outputAncestryTestGuard{platform: platform}, nil
}

type outputAncestryTestGuard struct {
	platform *outputAncestryTestPlatform
}

func (guard *outputAncestryTestGuard) Root() outputV3Directory { return guard.platform.root }
func (guard *outputAncestryTestGuard) Close() error {
	guard.platform.guardCloses++
	return guard.platform.guardCloseErr
}

func TestOutputAncestryPlatformDecoratorRetainsPublicOperationGuard(t *testing.T) {
	platform := newOutputAncestryTestPlatform(t)
	decorated := &outputAncestryPlatformDecorator{outputV3Platform: platform}
	selection := outputAncestryTestSelection(t)

	validation, err := prepareOutputSelectionAncestry(decorated, selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := validation.Close(); err != nil {
		t.Fatal(err)
	}
	if platform.guardAcquires != 1 || platform.guardCloses != 1 {
		t.Fatalf("embedded platform guard lifecycle = (%d acquires, %d closes), want (1, 1)",
			platform.guardAcquires, platform.guardCloses)
	}
}
