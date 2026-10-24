//go:build windows

package outputwindows

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/windows"
)

func TestWindowsObjectIDFailurePreservesLiveOnlyCapabilityFacts(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := guard.Root()
	trap := &windowsObjectIDMutationTrap{}
	root.objectIDs = trap

	capabilities, err := root.destinationCapabilities()
	if err != nil {
		t.Fatal(err)
	}
	if calls := trap.calls.Load(); calls != 1 {
		t.Fatalf("persistent-recovery proof attempted Object ID enrollment %d times", calls)
	}
	if !capabilities.SafePublish().Supported() || !capabilities.CrashCleanup().Supported() ||
		capabilities.OperationRecovery().Supported() || capabilities.RangeRecovery().Supported() {
		t.Fatalf("Object ID split facts = safe:%v op:%v range:%v cleanup:%v",
			capabilities.SafePublish().Fact(), capabilities.OperationRecovery().Fact(),
			capabilities.RangeRecovery().Fact(), capabilities.CrashCleanup().Fact())
	}
	if mode, err := outputcap.SelectExecutionMode(capabilities); err != nil || mode != outputcap.ExecutionLiveOnly {
		t.Fatalf("Object ID negative mode = %v, %v", mode, err)
	}
}

func TestWindowsDestinationCapabilityFactsFailIndependently(t *testing.T) {
	failures := []struct {
		name   string
		reason outputcap.CapabilityReason
		read   func(outputcap.DestinationCapabilities) outputcap.CapabilityEvidence
		set    func(*windowsCapabilityProbeResults, error)
	}{
		{"safe-publish", outputcap.CapabilityReasonUnsafePublication,
			func(value outputcap.DestinationCapabilities) outputcap.CapabilityEvidence { return value.SafePublish() },
			func(value *windowsCapabilityProbeResults, err error) { value.safePublish = err }},
		{"operation-recovery", outputcap.CapabilityReasonUnverifiableOperationRecovery,
			func(value outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return value.OperationRecovery()
			},
			func(value *windowsCapabilityProbeResults, err error) { value.operationRecovery = err }},
		{"range-recovery", outputcap.CapabilityReasonUnverifiableRangeRecovery,
			func(value outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return value.RangeRecovery()
			},
			func(value *windowsCapabilityProbeResults, err error) { value.rangeRecovery = err }},
		{"crash-cleanup", outputcap.CapabilityReasonUnverifiableCrashCleanup,
			func(value outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return value.CrashCleanup()
			},
			func(value *windowsCapabilityProbeResults, err error) { value.crashCleanup = err }},
	}
	for index, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			var results windowsCapabilityProbeResults
			failure.set(&results, errors.Join(errWindowsOutputUnsupported, errors.New("injected proof failure")))
			capabilities, err := windowsDestinationCapabilitiesFromResults(results)
			if err != nil {
				t.Fatal(err)
			}
			for otherIndex, other := range failures {
				evidence := other.read(capabilities)
				if otherIndex == index {
					if evidence.Supported() || evidence.Reason() != failure.reason {
						t.Fatalf("failed evidence = (%v, %v)", evidence.Fact(), evidence.Reason())
					}
					continue
				}
				if !evidence.Supported() {
					t.Fatalf("%s was coupled to %s failure: %v", other.name, failure.name, evidence.Reason())
				}
			}
		})
	}
	_, err := windowsDestinationCapabilitiesFromResults(windowsCapabilityProbeResults{
		safePublish: errors.Join(errWindowsOutputUnsafe, errors.New("injected unsafe namespace")),
	})
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe probe result = %v", err)
	}
}

func TestWindowsSemanticFilePublishIsNoCopyNoReplace(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := &windowsOutputDirectory{native: guard.Root()}
	stageDirectory, err := root.CreateDirectory("publish-stages", true)
	if err != nil {
		t.Fatal(err)
	}
	defer stageDirectory.Close()
	stages := stageDirectory.(*windowsOutputDirectory)
	ticket := windowsTestLiveCleanupTicket(t, 4, checkpointmodel.LiveCleanupTicketCommitted)
	if err := root.CreateLiveCleanupStage(stages, ticket); err != nil {
		t.Fatal(err)
	}
	stage, err := stages.OpenMutableFile(ticket.StageName(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if _, err := stage.WriteAt([]byte("data"), 0); err != nil {
		t.Fatal(err)
	}
	outcome, err := root.PublishFileNoReplace(stage, "published.bin")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish outcome = %v, %v", outcome, err)
	}
	published, err := root.OpenObservedFile("published.bin", false)
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()
	same, err := stage.SameFile(published)
	if err != nil || !same {
		t.Fatalf("publication copied/replaced the source: same=%t error=%v", same, err)
	}
	collision, err := root.PublishFileNoReplace(stage, "published.bin")
	if err != nil || collision != outputcap.PublishNoReplaceCollision {
		t.Fatalf("collision outcome = %v, %v", collision, err)
	}
	stillSame, err := stage.SameFile(published)
	if err != nil || !stillSame {
		t.Fatalf("collision mutated existing publication: same=%t error=%v", stillSame, err)
	}
	if err := root.RemoveFile("published.bin", published); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsSemanticFilePublishRaceHasOneWinner(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := &windowsOutputDirectory{native: guard.Root()}
	stagesValue, err := root.CreateDirectory("race-stages", true)
	if err != nil {
		t.Fatal(err)
	}
	defer stagesValue.Close()
	stages := stagesValue.(*windowsOutputDirectory)
	const contenders = 8
	files := make([]outputcap.MutableFile, contenders)
	for index := range files {
		ticket := windowsTestLiveCleanupTicketWithNonce(
			t, byte(index+1), 1, checkpointmodel.LiveCleanupTicketCommitted,
		)
		if err := root.CreateLiveCleanupStage(stages, ticket); err != nil {
			t.Fatal(err)
		}
		files[index], err = stages.OpenMutableFile(ticket.StageName(), false)
		if err != nil {
			t.Fatal(err)
		}
		defer files[index].Close()
	}
	type result struct {
		outcome outputcap.PublishNoReplaceOutcome
		err     error
	}
	results := make(chan result, contenders)
	for _, file := range files {
		go func(source outputcap.FileIdentity) {
			outcome, publishErr := root.PublishFileNoReplace(source, "race.bin")
			results <- result{outcome: outcome, err: publishErr}
		}(file)
	}
	committed := 0
	collisions := 0
	for range contenders {
		current := <-results
		if current.err != nil {
			t.Fatalf("race publish error = %v (%v)", current.err, current.outcome)
		}
		switch current.outcome {
		case outputcap.PublishNoReplaceCommitted:
			committed++
		case outputcap.PublishNoReplaceCollision:
			collisions++
		default:
			t.Fatalf("race publish outcome = %v", current.outcome)
		}
	}
	if committed != 1 || collisions != contenders-1 {
		t.Fatalf("race results committed=%d collisions=%d", committed, collisions)
	}
	published, err := root.OpenObservedFile("race.bin", false)
	if err != nil {
		t.Fatal(err)
	}
	defer published.Close()
	if err := root.RemoveFile("race.bin", published); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsPublicDirectoryMetadataUsesOptionalIdentityCheckedHandle(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := &windowsOutputDirectory{native: guard.Root()}
	childValue, err := root.CreateDirectory("metadata-child", false)
	if err != nil {
		t.Fatal(err)
	}
	defer childValue.Close()
	child := childValue.(*windowsOutputDirectory)
	if err := child.ValidateMetadataAuthority(); err != nil {
		t.Fatal(err)
	}
	modified, err := catalog.NewModifiedTime(1_700_000_000, 0, catalog.TimePrecisionSeconds)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.SetModifiedTime(modified); err != nil {
		t.Fatal(err)
	}
	if matches, err := child.MetadataMatches(modified); err != nil || !matches {
		t.Fatalf("directory metadata match = %t, %v", matches, err)
	}
	closed := &windowsOutputDirectory{}
	if err := closed.ValidateMetadataAuthority(); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("closed metadata authority = %v", err)
	}
}

func TestWindowsPublicDirectoryReservationIsDirectNoReplace(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := &windowsOutputDirectory{native: guard.Root()}
	reserved, outcome, err := root.ReservePublicDirectoryNoReplace("published-directory")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("directory reservation outcome = %v, %v", outcome, err)
	}
	defer reserved.Close()
	opened, err := root.OpenDirectory("published-directory", false)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	same, err := reserved.SameDirectory(opened)
	if err != nil || !same {
		t.Fatalf("directory reservation changed object: same=%t error=%v", same, err)
	}
	collisionDirectory, collision, err := root.ReservePublicDirectoryNoReplace("published-directory")
	if err != nil || collision != outputcap.PublishNoReplaceCollision || collisionDirectory != nil {
		t.Fatalf("directory collision = %T, %v, %v", collisionDirectory, collision, err)
	}
}

func TestWindowsSemanticPublishClassifiesMutationCut(t *testing.T) {
	closed := &windowsOutputDirectory{}
	if outcome, err := closed.PublishFileNoReplace(nil, "x"); outcome != 0 || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("pre-mutation file outcome = %v, %v", outcome, err)
	}
	if directory, outcome, err := closed.ReservePublicDirectoryNoReplace("x"); directory != nil || outcome != 0 ||
		!errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("pre-mutation directory outcome = %T, %v, %v", directory, outcome, err)
	}
	marked := &windowsPublishMutationError{cause: errors.New("post-mutation verification")}
	if !windowsPublicationMayBeVisible(marked) || windowsPublicationMayBeVisible(errors.New("preflight")) {
		t.Fatal("publication mutation marker classification is not exact")
	}
}

func TestWindowsLiveCleanupStageUsesPublicParentACLAndPrivateProofName(t *testing.T) {
	platform, guard := windowsOpenGuardedTestRoot(t)
	rootPath := platform.root.path
	root := &windowsOutputDirectory{native: guard.Root()}
	windowsInstallDirectFileChildMarker(t, rootPath)
	controlDirectory, err := root.CreateDirectory(checkpointmodel.LiveCleanupNamespaceV1, true)
	if err != nil {
		t.Fatal(err)
	}
	defer controlDirectory.Close()
	control := controlDirectory.(*windowsOutputDirectory)
	nestedDirectory, outcome, err := root.ReservePublicDirectoryNoReplace("nested-parent")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted || nestedDirectory == nil {
		t.Fatalf("nested parent = (%T, %d, %v)", nestedDirectory, outcome, err)
	}
	defer nestedDirectory.Close()
	nested := nestedDirectory.(*windowsOutputDirectory)
	ticket := windowsTestLiveCleanupTicket(t, 9, checkpointmodel.LiveCleanupTicketCommitted)
	if err := nested.CreateLiveCleanupStage(control, ticket); err != nil {
		t.Fatal(err)
	}
	reopened, err := control.OpenMutableFile(ticket.StageName(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if size, err := reopened.Size(); err != nil || size != ticket.ExactSize() {
		t.Fatalf("stage size = %d, %v", size, err)
	}
	reference, err := nested.CreateFile("ordinary-acl-reference.bin", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	referenceDACL, referenceSDDL := windowsTestDACL(
		t, filepath.Join(rootPath, "nested-parent", "ordinary-acl-reference.bin"),
	)
	stageDACL, stageSDDL := windowsTestDACL(
		t, filepath.Join(rootPath, checkpointmodel.LiveCleanupNamespaceV1, ticket.StageName()),
	)
	rootReference, err := root.CreateFile("container-acl-reference.bin", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rootReference.Close()
	_, rootReferenceSDDL := windowsTestDACL(t, filepath.Join(rootPath, "container-acl-reference.bin"))
	if referenceDACL&windows.SE_DACL_PROTECTED != 0 || stageDACL&windows.SE_DACL_PROTECTED != 0 {
		t.Fatalf("ordinary public child profile became protected: reference=(%#x,%q) stage=(%#x,%q)",
			referenceDACL, referenceSDDL, stageDACL, stageSDDL)
	}
	if stageSDDL != referenceSDDL {
		t.Fatalf("stage did not inherit exact nested parent: stage=%q nested-reference=%q",
			stageSDDL, referenceSDDL)
	}
	if stageSDDL == rootReferenceSDDL {
		t.Fatalf("nested stage retained the container direct-child ACL: stage=%q root-reference=%q",
			stageSDDL, rootReferenceSDDL)
	}
	for _, entry := range windowsDirectoryNames(t, rootPath) {
		if strings.HasPrefix(entry, windowsLiveStageTemporaryPrefix) {
			t.Fatalf("successful stage creation leaked public temporary name %q", entry)
		}
	}
	if err := control.RemoveLiveCleanupStage(ticket, reopened); err != nil {
		t.Fatal(err)
	}
	if kind, err := control.ObserveEntry(ticket.StageName()); err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("removed stage kind = %v, %v", kind, err)
	}
	if err := control.CreateLiveCleanupStage(control, ticket); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("private final parent accepted stage: %v", err)
	}
	wrongProfile, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce: bytes.Repeat([]byte{2}, checkpointmodel.LiveCleanupNonceBytesV1), ExactSize: 1,
		Profile: checkpointmodel.LiveCleanupLinuxExt4V1, Generation: 1,
		State: checkpointmodel.LiveCleanupTicketCommitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := root.CreateLiveCleanupStage(control, wrongProfile); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("wrong profile accepted: %v", err)
	}
}

func windowsInstallDirectFileChildMarker(t *testing.T, path string) {
	t.Helper()
	policy, err := newWindowsPrivatePolicy()
	if err != nil {
		t.Fatal(err)
	}
	owner, err := windowsTestDirectoryOwner(path)
	if err != nil {
		t.Fatal(err)
	}
	principals, err := windowsTestDirectoryPrincipals(policy)
	if err != nil {
		t.Fatal(err)
	}
	everyone, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	// This read ACE applies only to files created directly under the container.
	// A stage created through a nested final parent must therefore not receive it.
	descriptor, err := windows.SecurityDescriptorFromString(
		"O:" + owner.String() + "D:P" + windowsInheritableFullAccessEntries(principals) +
			"(A;OINPIO;GR;;;" + everyone.String() + ")",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := windowsSetTestDirectoryDACL(path, descriptor, policy); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsOrdinaryStageUsesOneSameFilesystemPrivateObject(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	semanticPlatform := &windowsOutputPlatform{}
	if semanticPlatform.LiveCleanupNativeProfile() != checkpointmodel.LiveCleanupWindowsNTFSV1 {
		t.Fatalf("native cleanup profile = %d", semanticPlatform.LiveCleanupNativeProfile())
	}
	root := &windowsOutputDirectory{native: guard.Root()}
	stageDirectory, err := root.CreateDirectory("ordinary-stages", true)
	if err != nil {
		t.Fatal(err)
	}
	defer stageDirectory.Close()
	stages := stageDirectory.(*windowsOutputDirectory)
	const stageName = "owned.stage"
	if err := root.CreateOrdinaryOutputStage(stages, stageName, 7); err != nil {
		t.Fatal(err)
	}
	stage, err := stages.OpenMutableFile(stageName, false)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	if size, err := stage.Size(); err != nil || size != 7 {
		t.Fatalf("ordinary stage size = (%d, %v)", size, err)
	}
	if _, err := stage.WriteAt([]byte("wind"), 1); err != nil {
		t.Fatal(err)
	}
	if err := stage.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := root.CreateOrdinaryOutputStage(stages, stageName, 7); !errors.Is(err, outputcap.ErrNamespaceCollision) {
		t.Fatalf("ordinary stage collision = %v", err)
	}
	if err := root.CreateOrdinaryOutputStage(stages, "", 1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("empty ordinary stage name = %v", err)
	}
	if err := stages.CreateOrdinaryOutputStage(stages, "nested.stage", 1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("private final parent accepted stage = %v", err)
	}
	if err := root.CreateOrdinaryOutputStage(root, "public.stage", 1); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("public proof directory accepted stage = %v", err)
	}
}

func windowsTestDACL(t *testing.T, path string) (windows.SECURITY_DESCRIPTOR_CONTROL, string) {
	t.Helper()
	descriptor, err := windows.GetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	daclOnly, err := windows.NewSecurityDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if err := daclOnly.SetDACL(dacl, true, false); err != nil {
		t.Fatal(err)
	}
	return control, daclOnly.String()
}

func windowsDirectoryNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

const (
	windowsLiveStageCrashChildEnvironment = "WINDSHARE_WINDOWS_LIVE_STAGE_CRASH_CHILD"
	windowsLiveStageCrashCutEnvironment   = "WINDSHARE_WINDOWS_LIVE_STAGE_CRASH_CUT"
	windowsLiveStageCrashParentName       = "nested-parent"
)

func TestWindowsLiveStageCreateSurvivesRealProcessKillAtEveryCut(t *testing.T) {
	if os.Getenv(windowsLiveStageCrashChildEnvironment) == "1" {
		runWindowsLiveStageCrashChild(t)
		return
	}
	requireUnprivilegedWindowsNTFSCertification(t)
	for _, cut := range []windowsLiveStageCreateCut{
		windowsLiveStageCutTemporaryCreated,
		windowsLiveStageCutInstalled,
		windowsLiveStageCutSynced,
		windowsLiveStageCutCommitted,
	} {
		t.Run(strconv.Itoa(int(cut)), func(t *testing.T) {
			base := t.TempDir()
			rootPath := filepath.Join(base, "output")
			if err := os.Mkdir(rootPath, 0o700); err != nil {
				t.Fatal(err)
			}
			nestedPath := filepath.Join(rootPath, windowsLiveStageCrashParentName)
			if err := os.Mkdir(nestedPath, 0o700); err != nil {
				t.Fatal(err)
			}
			readyPath := filepath.Join(base, "child.ready")
			killNativeOutputChildAfterReady(t, readyPath, []string{
				windowsLiveStageCrashChildEnvironment + "=1",
				windowsLiveStageCrashCutEnvironment + "=" + strconv.Itoa(int(cut)),
				nativeOutputCrashRootEnvironment + "=" + rootPath,
				nativeOutputCrashReadyEnvironment + "=" + readyPath,
			})
			platform, err := openWindowsOutputPlatform(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer platform.Close()
			proof, err := platform.root.OpenPrivateDirectory(checkpointmodel.LiveCleanupNamespaceV1)
			if err != nil {
				t.Fatal(err)
			}
			defer proof.Close()
			ticket := windowsTestLiveCleanupTicket(t, 9, checkpointmodel.LiveCleanupTicketCommitted)
			stage, _, err := proof.openFile(
				ticket.StageName(), windows.FILE_OPEN, windowsReadFileAccess(), nil, false,
			)
			if cut != windowsLiveStageCutCommitted {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("precommit cut retained stage: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("committed cut lost stage: %v", err)
				}
				if size, sizeErr := stage.Size(); sizeErr != nil || size != ticket.ExactSize() {
					t.Fatalf("committed stage size = %d, %v", size, sizeErr)
				}
				_ = stage.Close()
			}
			for _, name := range windowsDirectoryNames(t, nestedPath) {
				if strings.HasPrefix(name, windowsLiveStageTemporaryPrefix) {
					t.Fatalf("crash cut leaked nested-parent temporary name %q", name)
				}
			}
		})
	}
}

func runWindowsLiveStageCrashChild(t *testing.T) {
	rootPath := os.Getenv(nativeOutputCrashRootEnvironment)
	readyPath := os.Getenv(nativeOutputCrashReadyEnvironment)
	rawCut := os.Getenv(windowsLiveStageCrashCutEnvironment)
	value, err := strconv.Atoi(rawCut)
	if err != nil || value < int(windowsLiveStageCutTemporaryCreated) ||
		value > int(windowsLiveStageCutCommitted) || rootPath == "" || readyPath == "" {
		t.Fatalf("invalid live-stage crash child parameters: %v", err)
	}
	cut := windowsLiveStageCreateCut(value)
	platform, err := openWindowsOutputPlatform(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer platform.Close()
	proof, err := platform.root.CreatePrivateDirectory(checkpointmodel.LiveCleanupNamespaceV1)
	if err != nil {
		t.Fatal(err)
	}
	defer proof.Close()
	parentValue, err := platform.root.OpenDirectory(windowsLiveStageCrashParentName)
	if err != nil {
		t.Fatal(err)
	}
	defer parentValue.Close()
	parentValue.createObserver = windowsLiveStageCreateObserverFunc(func(observed windowsLiveStageCreateCut) error {
		if observed != cut {
			return nil
		}
		signalNativeOutputCrashCut(t, readyPath)
		time.Sleep(nativeOutputCrashChildMaximumWait)
		t.Fatal("live-stage create child was not terminated")
		return nil
	})
	parent := &windowsOutputDirectory{native: parentValue}
	proofValue := &windowsOutputDirectory{native: proof}
	ticket := windowsTestLiveCleanupTicket(t, 9, checkpointmodel.LiveCleanupTicketCommitted)
	if err := parent.CreateLiveCleanupStage(proofValue, ticket); err != nil {
		t.Fatal(err)
	}
	t.Fatal("live-stage create child passed its requested kill cut")
}

func TestWindowsRecordBeforeStageCleanupReplaysEveryCut(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := &windowsOutputDirectory{native: guard.Root()}
	for _, cut := range []string{"ticket", "stage", "stage-removed", "ticket-removed"} {
		t.Run(cut, func(t *testing.T) {
			controlName := "cleanup-" + cut
			controlDirectory, err := root.CreateDirectory(controlName, true)
			if err != nil {
				t.Fatal(err)
			}
			defer controlDirectory.Close()
			control := controlDirectory.(*windowsOutputDirectory)
			ticket := windowsTestLiveCleanupTicket(t, 7, checkpointmodel.LiveCleanupTicketCommitted)
			ticketName := "ticket-" + hex.EncodeToString(ticket.Nonce()) + ".record"
			encoded, err := checkpointmodel.EncodeLiveCleanupTicket(ticket)
			if err != nil {
				t.Fatal(err)
			}
			record, err := control.CreateFile(ticketName, true, int64(len(encoded)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := record.WriteAt(encoded, 0); err != nil {
				t.Fatal(err)
			}
			if err := errors.Join(record.Sync(), control.Sync()); err != nil {
				t.Fatal(err)
			}
			if cut != "ticket" {
				if err := root.CreateLiveCleanupStage(control, ticket); err != nil {
					t.Fatal(err)
				}
			}
			if cut == "stage-removed" || cut == "ticket-removed" {
				stage, err := control.OpenMutableFile(ticket.StageName(), false)
				if err != nil {
					t.Fatal(err)
				}
				if err := control.RemoveLiveCleanupStage(ticket, stage); err != nil {
					_ = stage.Close()
					t.Fatal(err)
				}
				if err := stage.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if cut == "ticket-removed" {
				if err := control.RemoveFile(ticketName, record); err != nil {
					t.Fatal(err)
				}
				if err := control.Sync(); err != nil {
					t.Fatal(err)
				}
			}
			if err := record.Close(); err != nil {
				t.Fatal(err)
			}

			// Replay begins only from the durable ticket. A committed ticket with
			// no stage is retired; a present exact stage is removed before it.
			reopenedRecord, err := control.OpenObservedFile(ticketName, true)
			if cut == "ticket-removed" {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("retired ticket reopened: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer reopenedRecord.Close()
			reopenedStage, stageErr := control.OpenMutableFile(ticket.StageName(), false)
			if stageErr == nil {
				if err := control.RemoveLiveCleanupStage(ticket, reopenedStage); err != nil {
					_ = reopenedStage.Close()
					t.Fatal(err)
				}
				if err := reopenedStage.Close(); err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(stageErr, fs.ErrNotExist) {
				t.Fatal(stageErr)
			}
			if err := control.RemoveFile(ticketName, reopenedRecord); err != nil {
				t.Fatal(err)
			}
			if err := control.Sync(); err != nil {
				t.Fatal(err)
			}
			if kind, err := control.ObserveEntry(ticket.StageName()); err != nil || kind != outputcap.EntryAbsent {
				t.Fatalf("replay left stage kind=%v error=%v", kind, err)
			}
			if kind, err := control.ObserveEntry(ticketName); err != nil || kind != outputcap.EntryAbsent {
				t.Fatalf("replay left ticket kind=%v error=%v", kind, err)
			}
		})
	}
}

func TestWindowsPrivateIdentityWrapperUsesPrivateAuthority(t *testing.T) {
	_, guard := windowsOpenGuardedTestRoot(t)
	root := &windowsOutputDirectory{native: guard.Root()}
	privateDirectory, err := root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	defer privateDirectory.Close()
	preparer := privateDirectory.(outputcap.PersistentDirectoryIdentityPreparer)
	identity := privateDirectory.(outputcap.PersistentDirectoryIdentity)
	prepared, err := preparer.PreparePersistentDirectoryIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := identity.PersistentDirectoryIdentityClaim()
	if err != nil || !bytes.Equal(prepared, claimed) {
		t.Fatalf("private identity dispatch differs: equal=%t error=%v", bytes.Equal(prepared, claimed), err)
	}
}

func TestWindowsRootIdentityClaimSurvivesSameFilesystemRename(t *testing.T) {
	parent := windowsNativeTestTempDir(t)
	original := filepath.Join(parent, "ordinary-root")
	renamed := filepath.Join(parent, "ordinary-root-renamed")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	platform, err := Open(original, false)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := platform.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	preparer := guard.Root().(outputcap.PersistentDirectoryIdentityPreparer)
	before, err := preparer.PreparePersistentDirectoryIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(guard.Close(), platform.Close()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, renamed); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(renamed, false)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedGuard, err := reopened.AcquirePublicOperationGuard()
	if err != nil {
		t.Fatal(err)
	}
	defer reopenedGuard.Close()
	after, err := reopenedGuard.Root().(outputcap.PersistentDirectoryIdentityPreparer).
		PreparePersistentDirectoryIdentityClaim()
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("root rename changed native identity: equal=%t error=%v", bytes.Equal(before, after), err)
	}
}

func TestWindowsPublicRootRejectsReparseAndFileAncestors(t *testing.T) {
	requireUnprivilegedWindowsNTFSCertification(t)
	parent := windowsNativeTestTempDir(t)
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	junction := filepath.Join(parent, "junction")
	if err := os.Symlink(target, junction); err == nil {
		if platform, openErr := Open(junction, false); openErr == nil {
			_ = platform.Close()
			t.Fatal("reparse root was accepted")
		}
	}
	fileAncestor := filepath.Join(parent, "file-ancestor")
	if err := os.WriteFile(fileAncestor, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if platform, err := Open(filepath.Join(fileAncestor, "child"), false); platform != nil ||
		(!errors.Is(err, fs.ErrNotExist) && !errors.Is(err, outputcap.ErrRecoverableOutputUnsupported)) {
		if platform != nil {
			_ = platform.Close()
		}
		t.Fatalf("file ancestor admission = %v", err)
	}
}

func windowsTestLiveCleanupTicket(
	t *testing.T,
	size uint64,
	state checkpointmodel.LiveCleanupTicketState,
) checkpointmodel.LiveCleanupTicket {
	t.Helper()
	return windowsTestLiveCleanupTicketWithNonce(t, 1, size, state)
}

func windowsTestLiveCleanupTicketWithNonce(
	t *testing.T,
	nonce byte,
	size uint64,
	state checkpointmodel.LiveCleanupTicketState,
) checkpointmodel.LiveCleanupTicket {
	t.Helper()
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce:     bytes.Repeat([]byte{nonce}, checkpointmodel.LiveCleanupNonceBytesV1),
		ExactSize: size, Profile: checkpointmodel.LiveCleanupWindowsNTFSV1,
		Generation: 1, State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}
