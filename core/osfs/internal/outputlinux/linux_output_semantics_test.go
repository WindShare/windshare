//go:build linux

package outputlinux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

const (
	linuxTestDefaultACLXattr = "system.posix_acl_default"
	linuxTestAccessACLXattr  = "system.posix_acl_access"
	linuxTestACLVersion      = uint32(2)
	linuxTestACLUndefinedID  = ^uint32(0)
	linuxTestACLUserObject   = uint16(0x01)
	linuxTestACLUser         = uint16(0x02)
	linuxTestACLGroupObject  = uint16(0x04)
	linuxTestACLMask         = uint16(0x10)
	linuxTestACLOther        = uint16(0x20)
	linuxTestACLRead         = uint16(0x04)
	linuxTestACLWrite        = uint16(0x02)
	linuxTestACLExecute      = uint16(0x01)
)

func TestLinuxDestinationCapabilityEvidenceRemainsOrthogonal(t *testing.T) {
	const failureText = "fact unavailable"
	tests := []struct {
		name   string
		set    func(*linuxCapabilityProbeResults, error)
		fact   func(outputcap.DestinationCapabilities) outputcap.CapabilityEvidence
		reason outputcap.CapabilityReason
	}{
		{
			name: "safe publish",
			set:  func(results *linuxCapabilityProbeResults, err error) { results.safePublish = err },
			fact: func(capabilities outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return capabilities.SafePublish()
			},
			reason: outputcap.CapabilityReasonUnsafePublication,
		},
		{
			name: "operation recovery",
			set:  func(results *linuxCapabilityProbeResults, err error) { results.operationRecovery = err },
			fact: func(capabilities outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return capabilities.OperationRecovery()
			},
			reason: outputcap.CapabilityReasonUnverifiableOperationRecovery,
		},
		{
			name: "range recovery",
			set:  func(results *linuxCapabilityProbeResults, err error) { results.rangeRecovery = err },
			fact: func(capabilities outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return capabilities.RangeRecovery()
			},
			reason: outputcap.CapabilityReasonUnverifiableRangeRecovery,
		},
		{
			name: "crash cleanup",
			set:  func(results *linuxCapabilityProbeResults, err error) { results.crashCleanup = err },
			fact: func(capabilities outputcap.DestinationCapabilities) outputcap.CapabilityEvidence {
				return capabilities.CrashCleanup()
			},
			reason: outputcap.CapabilityReasonUnverifiableCrashCleanup,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var results linuxCapabilityProbeResults
			test.set(&results, errors.New(failureText))
			capabilities, err := linuxDestinationCapabilitiesFromResults(results)
			if err != nil {
				t.Fatal(err)
			}
			evidence := []outputcap.CapabilityEvidence{
				capabilities.SafePublish(), capabilities.OperationRecovery(),
				capabilities.RangeRecovery(), capabilities.CrashCleanup(),
			}
			unsupported := 0
			for _, current := range evidence {
				if !current.Supported() {
					unsupported++
				}
			}
			got := test.fact(capabilities)
			if unsupported != 1 || got.Supported() || got.Reason() != test.reason {
				t.Fatalf("unsupported=%d fact=%+v, want one unsupported reason %v",
					unsupported, got, test.reason)
			}
		})
	}

	results := linuxCapabilityProbeResults{
		safePublish: linuxUnsafe("probe safe publish", "contradictory namespace", nil),
	}
	if _, err := linuxDestinationCapabilitiesFromResults(results); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("unsafe proof contradiction was reduced to a capability fact: %v", err)
	}
}

func TestLinuxCapabilityProbeKeepsCrashCleanupIndependent(t *testing.T) {
	platform, rootPath := newLinuxAdapterTestPlatform(t)
	root := platform.root
	probeName := linuxOutputProbePrefix + "6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b6b"
	probeDirectory, err := root.native.createPrivateDirectoryExact(probeName, linuxOutputDirectoryMode)
	if err != nil {
		t.Fatal(err)
	}
	probe := linuxOutputProbe{root: root.native, rootName: probeName, directory: probeDirectory}
	originalOpenat2 := root.native.system.openat2
	root.native.system.openat2 = func(fd int, name string, how *unix.OpenHow) (int, error) {
		if how.Flags&uint64(unix.O_TMPFILE) == uint64(unix.O_TMPFILE) {
			return -1, unix.EOPNOTSUPP
		}
		return originalOpenat2(fd, name, how)
	}
	results, probeErr := probe.runCapabilityFacts()
	root.native.system.openat2 = originalOpenat2
	if probeErr != nil {
		t.Fatal(probeErr)
	}
	if results.safePublish != nil || results.operationRecovery != nil || results.rangeRecovery != nil {
		t.Fatalf("O_TMPFILE failure erased unrelated facts: %+v", results)
	}
	if !errors.Is(results.crashCleanup, errLinuxOutputUnsupported) {
		t.Fatalf("O_TMPFILE failure crash-cleanup evidence=%v", results.crashCleanup)
	}
	if err := probe.cleanup(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("independent probe residue=%v error=%v", entries, err)
	}
}

func TestLinuxPublicOperationGuardRevalidatesCurrentAccess(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	originalAccess := platform.root.native.system.faccessat2
	platform.root.native.system.faccessat2 = func(int, string, uint32, int) error {
		return unix.EACCES
	}
	defer func() { platform.root.native.system.faccessat2 = originalAccess }()

	guard, err := platform.AcquirePublicOperationGuard()
	if guard != nil || !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("revoked root access produced guard=%T error=%v", guard, err)
	}
}

func TestLinuxPersistentDirectoryIdentityIgnoresDisplayPlacement(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	root := platform.root
	first, err := root.PersistentDirectoryIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	root.native.absolutePath = "/renamed/display-only/root"
	second, err := root.PersistentDirectoryIdentityClaim()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("display placement changed restart identity: first=%x second=%x", first, second)
	}
}

func TestLinuxSemanticFilePublicationIsNoCopyAndNoReplace(t *testing.T) {
	platform, rootPath := newLinuxAdapterTestPlatform(t)
	root := platform.root
	controlValue, err := root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	control := controlValue.(*linuxV3Directory)
	t.Cleanup(func() { _ = control.Close() })

	source, err := control.CreateFile("source", true, 9)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	outcome, err := root.PublishFileNoReplace(source, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("first publish outcome=%v error=%v", outcome, err)
	}
	final, err := root.OpenFile("final", false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = final.Close() })
	if same, sameErr := source.SameFile(final); sameErr != nil || !same {
		t.Fatalf("published file was copied: same=%t error=%v", same, sameErr)
	}
	outcome, err = root.PublishFileNoReplace(source, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCollision {
		t.Fatalf("collision publish outcome=%v error=%v", outcome, err)
	}

	if err := root.RemoveFile("final", final); err != nil {
		t.Fatal(err)
	}
	if err := control.RemoveFile("source", source); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("control", control); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(rootPath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("publication test residue=%v error=%v", entries, err)
	}
}

func TestLinuxSemanticPublicationReportsPreMutationFailureWithZeroOutcome(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	root := platform.root
	controlValue, err := root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	control := controlValue.(*linuxV3Directory)
	source, err := control.CreateFile("source", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := control.RemoveFile("source", source); err != nil {
		t.Fatal(err)
	}
	outcome, publishErr := root.PublishFileNoReplace(source, "final")
	if outcome != 0 || !errors.Is(publishErr, outputcap.ErrFixedLinkSourceChanged) {
		t.Fatalf("pre-link failure outcome=%v error=%v", outcome, publishErr)
	}
	if kind, _, err := root.ClassifyExactEntry("final"); err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("pre-link failure mutated final: kind=%v error=%v", kind, err)
	}
	if err := root.RemoveDirectory("control", control); err != nil {
		t.Fatal(err)
	}
	_ = source.Close()
	_ = control.Close()
}

func TestLinuxSemanticFilePublicationReportsPostLinkIndeterminate(t *testing.T) {
	platform, rootPath := newLinuxAdapterTestPlatform(t)
	root := platform.root
	controlValue, err := root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	control := controlValue.(*linuxV3Directory)
	source, err := control.CreateFile("source", true, 1)
	if err != nil {
		t.Fatal(err)
	}
	originalSync := root.native.system.fsync
	root.native.system.fsync = func(fd int) error {
		if fd == root.native.fd {
			return unix.EIO
		}
		return originalSync(fd)
	}
	outcome, publishErr := root.PublishFileNoReplace(source, "final")
	root.native.system.fsync = originalSync
	if outcome != outputcap.PublishNoReplaceIndeterminate || publishErr == nil {
		t.Fatalf("post-link sync outcome=%v error=%v", outcome, publishErr)
	}
	final, err := root.OpenFile("final", false, false)
	if err != nil {
		t.Fatalf("indeterminate publish did not retain reconcilable final: %v", err)
	}
	if same, sameErr := source.SameFile(final); sameErr != nil || !same {
		t.Fatalf("indeterminate final differs from source: same=%t error=%v", same, sameErr)
	}
	if _, err := os.Stat(filepath.Join(rootPath, "control", "source")); err != nil {
		t.Fatalf("indeterminate publish removed its source: %v", err)
	}

	if err := root.RemoveFile("final", final); err != nil {
		t.Fatal(err)
	}
	if err := control.RemoveFile("source", source); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("control", control); err != nil {
		t.Fatal(err)
	}
	_ = final.Close()
	_ = source.Close()
	_ = control.Close()
}

func TestLinuxPublicDirectoryReservationIsNoReplaceAndNotPrivate(t *testing.T) {
	platform, rootPath := newLinuxAdapterTestPlatform(t)
	root := platform.root
	result, outcome, err := root.ReservePublicDirectoryNoReplace("result")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted || result == nil {
		t.Fatalf("directory reservation outcome=%v result=%T error=%v", outcome, result, err)
	}
	collision, outcome, err := root.ReservePublicDirectoryNoReplace("result")
	if err != nil || outcome != outputcap.PublishNoReplaceCollision || collision != nil {
		t.Fatalf("directory collision outcome=%v result=%T error=%v", outcome, collision, err)
	}
	info, err := os.Stat(filepath.Join(rootPath, "result"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() == os.FileMode(linuxOutputDirectoryMode) {
		t.Fatalf("public result root inherited private permissions: %o", info.Mode().Perm())
	}
	if err := root.RemoveDirectory("result", result); err != nil {
		t.Fatal(err)
	}
	_ = result.Close()
}

func TestLinuxPublicDirectoryReservationReturnsHandleAfterCreateCut(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	root := platform.root
	originalMkdirat := root.native.system.mkdirat
	originalSync := root.native.system.fsync
	var requestedMode uint32
	root.native.system.mkdirat = func(fd int, name string, mode uint32) error {
		requestedMode = mode
		return originalMkdirat(fd, name, mode)
	}
	root.native.system.fsync = func(fd int) error {
		if fd == root.native.fd {
			return unix.EIO
		}
		return originalSync(fd)
	}
	result, outcome, reserveErr := root.ReservePublicDirectoryNoReplace("result")
	root.native.system.mkdirat = originalMkdirat
	root.native.system.fsync = originalSync
	if outcome != outputcap.PublishNoReplaceIndeterminate || reserveErr == nil || result == nil {
		t.Fatalf("post-create reservation outcome=%v result=%T error=%v", outcome, result, reserveErr)
	}
	if requestedMode != linuxPublicDirectoryCreateMode {
		t.Fatalf("public directory requested mode=%o, want %o", requestedMode, linuxPublicDirectoryCreateMode)
	}
	reopened, err := root.OpenDirectory("result", false)
	if err != nil {
		t.Fatal(err)
	}
	if same, sameErr := result.SameDirectory(reopened); sameErr != nil || !same {
		t.Fatalf("indeterminate reservation handle differs from name: same=%t error=%v", same, sameErr)
	}
	if err := root.RemoveDirectory("result", result); err != nil {
		t.Fatal(err)
	}
	_ = reopened.Close()
	_ = result.Close()
}

func TestLinuxLiveCleanupTicketCreatesAndRemovesOnlyExactStage(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	if got := platform.LiveCleanupNativeProfile(); got != checkpointmodel.LiveCleanupLinuxExt4V1 {
		t.Fatalf("cleanup profile=%v", got)
	}
	root := platform.root
	controlValue, err := root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	control := controlValue.(*linuxV3Directory)
	ticket := newLinuxLiveCleanupTicket(t, 7, checkpointmodel.LiveCleanupTicketCommitted)
	originalOpenat2 := root.native.system.openat2
	var requestedMode uint64
	root.native.system.openat2 = func(fd int, name string, how *unix.OpenHow) (int, error) {
		if how.Flags&uint64(unix.O_TMPFILE) == uint64(unix.O_TMPFILE) {
			requestedMode = how.Mode
		}
		return originalOpenat2(fd, name, how)
	}
	createErr := root.CreateLiveCleanupStage(control, ticket)
	root.native.system.openat2 = originalOpenat2
	if createErr != nil {
		t.Fatal(createErr)
	}
	if requestedMode != linuxPublicFileCreateMode {
		t.Fatalf("anonymous stage requested mode=%o, want %o", requestedMode, linuxPublicFileCreateMode)
	}
	stage, err := control.OpenFile(ticket.StageName(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	if size, sizeErr := stage.Size(); sizeErr != nil || size != ticket.ExactSize() {
		t.Fatalf("stage size=%d error=%v", size, sizeErr)
	}
	facts, err := stage.(*linuxV3File).native.currentIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if facts.mode&linuxOutputPermissionMask == linuxOutputStateFileMode ||
		stage.(*linuxV3File).native.requireExactPermissions {
		t.Fatalf("stage inherited private mode/profile: mode=%o exact=%t",
			facts.mode&linuxOutputPermissionMask, stage.(*linuxV3File).native.requireExactPermissions)
	}

	if err := root.CreateLiveCleanupStage(control, ticket); !errors.Is(err, outputcap.ErrNamespaceCollision) {
		t.Fatalf("second stage create did not collide: %v", err)
	}
	wrongSize := newLinuxLiveCleanupTicket(t, ticket.ExactSize()+1, checkpointmodel.LiveCleanupTicketCommitted)
	if err := control.RemoveLiveCleanupStage(wrongSize, stage); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("mismatched ticket removed stage: %v", err)
	}
	if kind, _, err := control.ClassifyExactEntry(ticket.StageName()); err != nil || kind != outputcap.EntryRegularFile {
		t.Fatalf("mismatched cleanup mutated stage: kind=%v error=%v", kind, err)
	}
	outcome, err := root.PublishFileNoReplace(stage, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("publish cleanup stage outcome=%v error=%v", outcome, err)
	}
	final, err := root.OpenFile("final", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if same, sameErr := stage.SameFile(final); sameErr != nil || !same {
		t.Fatalf("published cleanup stage was copied: same=%t error=%v", same, sameErr)
	}
	if err := control.RemoveLiveCleanupStage(ticket, stage); err != nil {
		t.Fatal(err)
	}
	if kind, _, err := control.ClassifyExactEntry(ticket.StageName()); err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("exact cleanup left stage: kind=%v error=%v", kind, err)
	}
	if kind, _, err := root.ClassifyExactEntry("final"); err != nil || kind != outputcap.EntryRegularFile {
		t.Fatalf("proof cleanup removed published final: kind=%v error=%v", kind, err)
	}
	if err := root.RemoveFile("final", final); err != nil {
		t.Fatal(err)
	}
	_ = final.Close()
	if err := root.RemoveDirectory("control", control); err != nil {
		t.Fatal(err)
	}
	_ = stage.Close()
	_ = control.Close()
}

func TestLinuxNestedLiveStageInheritsExactParentDefaultACL(t *testing.T) {
	platform, rootPath := newLinuxAdapterTestPlatform(t)
	root := platform.root
	controlValue, err := root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	control := controlValue.(*linuxV3Directory)
	defer control.Close()

	rootACL := linuxTestDefaultACL(uint32(unix.Geteuid()+1), linuxTestACLRead)
	linuxTestSetDefaultACL(t, rootPath, rootACL)
	nestedValue, err := root.CreateDirectory("nested-parent", false)
	if err != nil {
		t.Fatal(err)
	}
	nested := nestedValue.(*linuxV3Directory)
	defer nested.Close()
	nestedPath := filepath.Join(rootPath, "nested-parent")
	nestedACL := linuxTestDefaultACL(uint32(unix.Geteuid()+2), linuxTestACLWrite)
	linuxTestSetDefaultACL(t, nestedPath, nestedACL)

	ticket := newLinuxLiveCleanupTicket(t, 5, checkpointmodel.LiveCleanupTicketCommitted)
	if err := nested.CreateLiveCleanupStage(control, ticket); err != nil {
		t.Fatal(err)
	}
	stage, err := control.OpenFile(ticket.StageName(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer stage.Close()
	nestedReference, err := nested.CreateFile("nested-reference", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nestedReference.Close()
	rootReference, err := root.CreateFile("root-reference", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rootReference.Close()

	stageACL := linuxTestReadACL(
		t, filepath.Join(rootPath, "control", ticket.StageName()), linuxTestAccessACLXattr,
	)
	nestedReferenceACL := linuxTestReadACL(
		t, filepath.Join(nestedPath, "nested-reference"), linuxTestAccessACLXattr,
	)
	rootReferenceACL := linuxTestReadACL(
		t, filepath.Join(rootPath, "root-reference"), linuxTestAccessACLXattr,
	)
	if !bytes.Equal(stageACL, nestedReferenceACL) {
		t.Fatalf("stage ACL differs from exact-parent reference: stage=%x nested=%x",
			stageACL, nestedReferenceACL)
	}
	if bytes.Equal(stageACL, rootReferenceACL) {
		t.Fatalf("nested stage inherited the container default ACL: stage=%x root=%x",
			stageACL, rootReferenceACL)
	}

	outcome, err := nested.PublishFileNoReplace(stage, "final")
	if err != nil || outcome != outputcap.PublishNoReplaceCommitted {
		t.Fatalf("nested publish = (%d, %v)", outcome, err)
	}
	final, err := nested.OpenFile("final", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Close()
	if same, err := stage.SameFile(final); err != nil || !same {
		t.Fatalf("nested publication copied the staged object: same=%t error=%v", same, err)
	}
	finalACL := linuxTestReadACL(t, filepath.Join(nestedPath, "final"), linuxTestAccessACLXattr)
	if !bytes.Equal(finalACL, nestedReferenceACL) {
		t.Fatalf("publication changed inherited ACL: final=%x nested=%x", finalACL, nestedReferenceACL)
	}
	if err := control.RemoveLiveCleanupStage(ticket, stage); err != nil {
		t.Fatal(err)
	}
}

type linuxTestACLEntry struct {
	tag        uint16
	permission uint16
	id         uint32
}

func linuxTestDefaultACL(namedUser uint32, namedPermission uint16) []byte {
	entries := []linuxTestACLEntry{
		{tag: linuxTestACLUserObject, permission: linuxTestACLRead | linuxTestACLWrite | linuxTestACLExecute, id: linuxTestACLUndefinedID},
		{tag: linuxTestACLUser, permission: namedPermission, id: namedUser},
		{tag: linuxTestACLGroupObject, permission: 0, id: linuxTestACLUndefinedID},
		{tag: linuxTestACLMask, permission: namedPermission, id: linuxTestACLUndefinedID},
		{tag: linuxTestACLOther, permission: 0, id: linuxTestACLUndefinedID},
	}
	value := make([]byte, 4+len(entries)*8)
	binary.LittleEndian.PutUint32(value, linuxTestACLVersion)
	for index, entry := range entries {
		offset := 4 + index*8
		binary.LittleEndian.PutUint16(value[offset:], entry.tag)
		binary.LittleEndian.PutUint16(value[offset+2:], entry.permission)
		binary.LittleEndian.PutUint32(value[offset+4:], entry.id)
	}
	return value
}

func linuxTestSetDefaultACL(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := unix.Setxattr(path, linuxTestDefaultACLXattr, value, 0); err != nil {
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EPERM) {
			t.Skipf("filesystem does not support owner-installed default ACLs: %v", err)
		}
		t.Fatal(err)
	}
}

func linuxTestReadACL(t *testing.T, path, name string) []byte {
	t.Helper()
	size, err := unix.Getxattr(path, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	value := make([]byte, size)
	written, err := unix.Getxattr(path, name, value)
	if err != nil {
		t.Fatal(err)
	}
	return value[:written]
}

func TestLinuxLiveCleanupPreservesUnknownReplacement(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	root := platform.root
	controlValue, err := root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	control := controlValue.(*linuxV3Directory)
	ticket := newLinuxLiveCleanupTicket(t, 3, checkpointmodel.LiveCleanupTicketCommitted)
	if err := root.CreateLiveCleanupStage(control, ticket); err != nil {
		t.Fatal(err)
	}
	owned, err := control.OpenFile(ticket.StageName(), false, true)
	if err != nil {
		t.Fatal(err)
	}
	displacedName := "displaced-stage"
	if err := owned.(*linuxV3File).origin.parent.renameRegularFile(
		ticket.StageName(), owned.(*linuxV3File).native,
		control.native, displacedName, linuxRenameReplace,
	); err != nil {
		t.Fatal(err)
	}
	replacement, err := control.CreateFile(ticket.StageName(), true, int64(ticket.ExactSize()))
	if err != nil {
		t.Fatal(err)
	}
	if err := control.RemoveLiveCleanupStage(ticket, owned); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("cleanup accepted an unknown replacement: %v", err)
	}
	if same, sameErr := replacement.SameFile(owned); sameErr != nil || same {
		t.Fatalf("replacement identity same=%t error=%v", same, sameErr)
	}
	if kind, _, err := control.ClassifyExactEntry(ticket.StageName()); err != nil || kind != outputcap.EntryRegularFile {
		t.Fatalf("cleanup mutated unknown replacement: kind=%v error=%v", kind, err)
	}

	if err := control.RemoveFile(ticket.StageName(), replacement); err != nil {
		t.Fatal(err)
	}
	if err := control.RemoveFile(displacedName, owned); err != nil {
		t.Fatal(err)
	}
	if err := root.RemoveDirectory("control", control); err != nil {
		t.Fatal(err)
	}
	_ = replacement.Close()
	_ = owned.Close()
	_ = control.Close()
}

func TestLinuxLiveCleanupRejectsUncommittedTicketAndPublicProofNamespace(t *testing.T) {
	platform, _ := newLinuxAdapterTestPlatform(t)
	root := platform.root
	committed := newLinuxLiveCleanupTicket(t, 1, checkpointmodel.LiveCleanupTicketCommitted)
	if err := root.CreateLiveCleanupStage(root, committed); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("public namespace accepted itself as cleanup proof: %v", err)
	}
	controlValue, err := root.CreateDirectory("control", true)
	if err != nil {
		t.Fatal(err)
	}
	control := controlValue.(*linuxV3Directory)
	createdState := newLinuxLiveCleanupTicket(t, 1, checkpointmodel.LiveCleanupStageCreated)
	if err := root.CreateLiveCleanupStage(control, createdState); !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("uncommitted ticket created cleanup stage: %v", err)
	}
	if kind, _, err := control.ClassifyExactEntry(createdState.StageName()); err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("rejected cleanup created residue: kind=%v error=%v", kind, err)
	}
	if err := root.RemoveDirectory("control", control); err != nil {
		t.Fatal(err)
	}
	_ = control.Close()
}

func newLinuxLiveCleanupTicket(
	t *testing.T,
	size uint64,
	state checkpointmodel.LiveCleanupTicketState,
) checkpointmodel.LiveCleanupTicket {
	t.Helper()
	nonce := bytes.Repeat([]byte{0x4a}, checkpointmodel.LiveCleanupNonceBytesV1)
	ticket, err := checkpointmodel.NewLiveCleanupTicket(checkpointmodel.LiveCleanupTicketSpec{
		Nonce: nonce, ExactSize: size, Profile: checkpointmodel.LiveCleanupLinuxExt4V1,
		Generation: 1, State: state,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestLinuxOptionalConsumerSeamsStayNarrow(t *testing.T) {
	platformType := reflect.TypeOf((*linuxDestinationCapabilityReporter)(nil)).Elem()
	if platformType.NumMethod() != 2 {
		t.Fatalf("platform seam methods=%d", platformType.NumMethod())
	}
	publisherType := reflect.TypeOf((*linuxSemanticPublisher)(nil)).Elem()
	if publisherType.NumMethod() != 2 {
		t.Fatalf("publisher seam methods=%d", publisherType.NumMethod())
	}
	creatorType := reflect.TypeOf((*linuxLiveCleanupStageCreator)(nil)).Elem()
	if creatorType.NumMethod() != 1 {
		t.Fatalf("cleanup creator seam methods=%d", creatorType.NumMethod())
	}
	removerType := reflect.TypeOf((*linuxLiveCleanupStageRemover)(nil)).Elem()
	if removerType.NumMethod() != 1 {
		t.Fatalf("cleanup remover seam methods=%d", removerType.NumMethod())
	}
}
