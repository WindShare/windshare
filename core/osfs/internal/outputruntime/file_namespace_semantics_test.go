package outputruntime

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestScanOutputV3FileNamespaceClassifiesAttentionWithoutGrantingAuthority(t *testing.T) {
	t.Parallel()
	paths := []string{"corrupt.bin", "unreadable.bin", "unsafe-type.bin", "unbound.bin"}
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelectionPaths(t, paths, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })

	corrupt := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(paths[0]))
	outputV3SemanticCreatePrivateFile(t, opened.Session.filesDir, corrupt, []byte{0xff}, 1)
	unreadable := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(paths[1]))
	outputV3SemanticCreatePrivateFile(
		t, opened.Session.filesDir, unreadable, nil, int64(resumestate.MaxFileStateBytes+1),
	)
	unsafeType := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(paths[2]))
	unsafeShard := outputV3SemanticOpenShard(t, opened.Session.filesDir, unsafeType.Shard(), true)
	unsafeEntry, err := unsafeShard.CreateDirectory(unsafeType.Name(), true)
	if err != nil {
		_ = unsafeShard.Close()
		t.Fatal(err)
	}
	if err := errors.Join(unsafeEntry.Sync(), unsafeEntry.Close(), unsafeShard.Sync(), unsafeShard.Close()); err != nil {
		t.Fatal(err)
	}

	otherRoot := v3RecoveryRoot(t)
	otherSessionIDs := &v3RecoverySessionIDs{next: 0x40}
	other := v3RecoveryOpen(t, v3RecoveryAuthority(t, otherRoot, otherSessionIDs), otherRoot, selection)
	otherIndex := outputV3NamespaceSelectionIndex(t, selection, paths[3])
	otherFile := v3RecoveryOutputFileAt(t, other.Session, selection, otherIndex)
	otherRecord, err := resumestate.NewFileRecord(resumestate.FileRecordSpec{
		Session: other.Session.stateSnapshot(), Descriptor: otherFile.Descriptor,
		CanonicalLocator: otherFile.Path, OutputObject: outputV3SemanticObjectID(t, 0x91),
	})
	if err != nil {
		v3RecoveryCloseSession(t, other.Session)
		t.Fatal(err)
	}
	encodedOther, err := resumestate.EncodeFileRecord(otherRecord.Bound())
	v3RecoveryCloseSession(t, other.Session)
	if err != nil {
		t.Fatal(err)
	}
	unbound := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(paths[3]))
	outputV3SemanticCreatePrivateFile(
		t, opened.Session.filesDir, unbound, encodedOther, int64(len(encodedOther)),
	)

	nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x72}, resumestate.UpdateNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	malformed := resumestate.UpdateTemporaryName(resumestate.DigestCanonicalLocator(paths[0]), nonce)
	outputV3SemanticCreatePrivateNamedFile(
		t, opened.Session.filesDir, malformed.Shard(), strings.ToUpper(malformed.Name()), nil, 0,
	)
	outputV3SemanticCreatePrivateNamedFile(t, opened.Session.filesDir, "ff", "opaque", nil, 0)

	snapshot, err := scanOutputV3FileNamespace(opened.Session)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.records) != 0 || len(snapshot.duplicateObjects) != 0 {
		t.Fatalf("unsafe state became authority: records=%d duplicates=%d", len(snapshot.records), len(snapshot.duplicateObjects))
	}
	codes := make(map[string]ResumeAttention)
	for _, attention := range snapshot.attention {
		codes[attention.Code] = attention
	}
	for _, code := range []string{
		"corrupt-file-record",
		"unreadable-file-record",
		"unsafe-file-state-entry",
		"unbound-file-record",
		"malformed-file-state-entry",
		"opaque-file-state-entry",
	} {
		attention, found := codes[code]
		if !found || attention.Scope != ResumeAttentionFile {
			t.Fatalf("attention %q = %+v, found=%t; codes=%v", code, attention, found, codes)
		}
	}
	for _, code := range []string{"corrupt-file-record", "unreadable-file-record", "unbound-file-record"} {
		if codes[code].Detail == "" {
			t.Fatalf("diagnostic attention %q omitted its cause", code)
		}
	}
	for _, code := range []string{"unsafe-file-state-entry", "malformed-file-state-entry", "opaque-file-state-entry"} {
		if codes[code].Detail != "" {
			t.Fatalf("classification-only attention %q invented a cause: %q", code, codes[code].Detail)
		}
	}
}

func TestScanOutputV3FileNamespaceRejectsInvalidOwnershipAndNamespaceRaces(t *testing.T) {
	t.Parallel()
	t.Run("nil-session", func(t *testing.T) {
		_, err := scanOutputV3FileNamespace(nil)
		outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultContract)
	})

	t.Run("closed-session", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, false, 0)
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
		v3RecoveryCloseSession(t, opened.Session)
		_, err := scanOutputV3FileNamespace(opened.Session)
		outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultOwnership)
	})

	t.Run("list-failure", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, false, 0)
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
		failure := errors.New("file namespace list failed")
		opened.Session.filesDir = &outputV3NamespaceNamesDirectory{
			Directory: opened.Session.filesDir,
			namesErr:  failure,
		}
		_, err := scanOutputV3FileNamespace(opened.Session)
		if !errors.Is(err, failure) {
			t.Fatalf("namespace list error = %v", err)
		}
		outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultStateIO)
	})

	t.Run("shard-set-changed", func(t *testing.T) {
		root := v3RecoveryRoot(t)
		selection := v3RecoverySelection(t, false, 0)
		opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
		t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
		opened.Session.filesDir = &outputV3NamespaceNamesDirectory{
			Directory:    opened.Session.filesDir,
			changeOnCall: 2,
		}
		_, err := scanOutputV3FileNamespace(opened.Session)
		if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
			t.Fatalf("namespace race error = %v", err)
		}
		outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe)
	})
}

func TestScanOutputV3FileNamespaceRejectsShardMutationDuringSecondPass(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	record := outputV3SemanticInstallReservedRecord(t, opened.Session, file).Bound().Record()
	recordName := resumestate.FileRecordName(record.LocatorDigest())
	opened.Session.filesDir = &outputV3NamespaceMutationRoot{
		Directory:   opened.Session.filesDir,
		targetShard: recordName.Shard(),
	}

	_, err := scanOutputV3FileNamespace(opened.Session)
	if !errors.Is(err, outputcap.ErrUnsafeNamespace) {
		t.Fatalf("shard mutation error = %v", err)
	}
	outputV3SemanticRequireFault(t, err, transfer.OutputFaultSession, transfer.OutputFaultNamespaceUnsafe)
}

func TestOutputV3ReconcilesUpdateTemporaryCutsAtTheLocatorScope(t *testing.T) {
	t.Parallel()
	root := v3RecoveryRoot(t)
	selection := v3RecoverySelection(t, true, 1)
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, nil), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, opened.Session) })
	file := v3RecoveryOutputFile(t, opened.Session, selection, 1)
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*FileTransaction)
	record := transaction.resumable.Bound().Record()
	outputV3SemanticDetachTransaction(t, opened.Session, transaction)
	recordName := resumestate.FileRecordName(record.LocatorDigest())
	shard := outputV3SemanticOpenShard(t, opened.Session.filesDir, recordName.Shard(), false)
	t.Cleanup(func() {
		if err := shard.Close(); err != nil {
			t.Errorf("close record shard: %v", err)
		}
	})
	nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x73}, resumestate.UpdateNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	temporary := resumestate.UpdateTemporaryName(record.LocatorDigest(), nonce)

	attention, err := opened.Session.reconcileFileShardUpdates(
		recordName.Shard(), shard, []string{temporary.Name()},
	)
	if err != nil || attention {
		t.Fatalf("post-rename missing temporary = (attention=%t, err=%v)", attention, err)
	}

	outputV3SemanticCreatePrivateFile(t, opened.Session.filesDir, temporary, nil, 0)
	attention, err = opened.Session.reconcileFileShardUpdates(
		recordName.Shard(), shard, []string{temporary.Name()},
	)
	if err != nil || attention {
		t.Fatalf("stale canonical temporary = (attention=%t, err=%v)", attention, err)
	}
	kind, err := shard.ObserveEntry(temporary.Name())
	if err != nil || kind != outputcap.EntryAbsent {
		t.Fatalf("stale temporary remained = (kind=%v, err=%v)", kind, err)
	}

	malformedName := strings.ToUpper(temporary.Name())
	outputV3SemanticCreatePrivateNamedFile(
		t, opened.Session.filesDir, recordName.Shard(), malformedName, nil, 0,
	)
	attention, err = opened.Session.reconcileFileShardUpdates(
		recordName.Shard(), shard, []string{malformedName},
	)
	if err != nil || !attention {
		t.Fatalf("malformed temporary = (attention=%t, err=%v)", attention, err)
	}
	bound, closeErr, err := opened.Session.openBoundFileRecord(shard, recordName)
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || bound.Record().Phase() != resumestate.FileQuarantined ||
		bound.Record().QuarantineReason() != resumestate.QuarantineUpdateTemporary {
		t.Fatalf("malformed temporary record = %+v, %v", bound.Record(), err)
	}

	outputV3SemanticCreatePrivateNamedFile(t, opened.Session.filesDir, recordName.Shard(), "opaque", nil, 0)
	attention, err = opened.Session.reconcileFileShardUpdates(recordName.Shard(), shard, []string{"opaque"})
	if err != nil || !attention {
		t.Fatalf("opaque temporary = (attention=%t, err=%v)", attention, err)
	}
}

func TestScanOutputV3FileNamespaceFailsClosedAtEveryPinnedShardCut(t *testing.T) {
	t.Parallel()
	failure := errors.New("file namespace scan fault")
	tooManyShards := make([]string, resumestate.MaxFileStateShardDirectories+1)
	for index := range tooManyShards {
		tooManyShards[index] = "aa"
	}
	for _, test := range []struct {
		name   string
		faults outputV3NamespaceFaults
		cause  error
	}{
		{name: "global shard budget", faults: outputV3NamespaceFaults{namesOverride: tooManyShards}, cause: resumestate.ErrFileStateNamespaceLimit},
		{name: "initial shard classification", faults: outputV3NamespaceFaults{classifyErrAt: 1, injected: failure}, cause: failure},
		{name: "initial shard open", faults: outputV3NamespaceFaults{openErrAt: 1, injected: failure}, cause: failure},
		{name: "initial shard enumeration", faults: outputV3NamespaceFaults{childNamesErrAt: 1, injected: failure}, cause: failure},
		{name: "initial shard close", faults: outputV3NamespaceFaults{childCloseErrAt: 1, injected: failure}, cause: failure},
		{name: "reclassified shard", faults: outputV3NamespaceFaults{classifyErrAt: 2, injected: failure}, cause: failure},
		{name: "reopened shard", faults: outputV3NamespaceFaults{openErrAt: 2, injected: failure}, cause: failure},
		{name: "first identity revalidation", faults: outputV3NamespaceFaults{openErrAt: 3, injected: failure}, cause: failure},
		{name: "stable shard enumeration", faults: outputV3NamespaceFaults{childNamesErrAt: 2, injected: failure}, cause: failure},
		{name: "final identity revalidation", faults: outputV3NamespaceFaults{openErrAt: 4, injected: failure}, cause: failure},
		{name: "stable root enumeration", faults: outputV3NamespaceFaults{namesErrAt: 2, injected: failure}, cause: failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _, _, _, _ := outputV3FileShardFixture(t)
			faults := test.faults
			original := session.filesDir
			session.filesDir = &outputV3NamespaceFaultDirectory{Directory: original, faults: &faults}
			t.Cleanup(func() { session.filesDir = original })

			snapshot, err := scanOutputV3FileNamespace(session)
			if !errors.Is(err, test.cause) || len(snapshot.records) != 0 || len(snapshot.shards) != 0 {
				t.Fatalf("faulted namespace scan = (%+v, %v)", snapshot, err)
			}
		})
	}
}

func TestInstallScannedFileRecordRevalidatesShardBeforeMutation(t *testing.T) {
	t.Parallel()
	failure := errors.New("scanned record install fault")
	for _, test := range []struct {
		name   string
		faults outputV3NamespaceFaults
	}{
		{name: "reopen shard", faults: outputV3NamespaceFaults{openErrAt: 1, injected: failure}},
		{name: "pin shard", faults: outputV3NamespaceFaults{openErrAt: 2, injected: failure}},
		{name: "install state", faults: outputV3NamespaceFaults{childCreateFileErr: failure, injected: failure}},
		{name: "close shard", faults: outputV3NamespaceFaults{childCloseErrAt: 2, injected: failure}},
		{name: "join install and close faults", faults: outputV3NamespaceFaults{childCreateFileErr: failure, childCloseErrAt: 2, injected: failure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			session, _, recordName, shard, _ := outputV3FileShardFixture(t)
			bound, closeErr, err := session.openBoundFileRecord(shard, recordName)
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			next, err := resumestate.PrepareIsolatedRetirement(bound)
			if err != nil {
				t.Fatal(err)
			}
			scanned := outputV3FileNamespaceRecord{
				shardName: recordName.Shard(), recordName: recordName.Name(), bound: bound,
			}
			faults := test.faults
			original := session.filesDir
			session.filesDir = &outputV3NamespaceFaultDirectory{Directory: original, faults: &faults}
			t.Cleanup(func() { session.filesDir = original })

			if err := session.installScannedFileRecord(scanned, next); !errors.Is(err, failure) {
				t.Fatalf("scanned record install error = %v", err)
			}
		})
	}

	t.Run("unchanged generation", func(t *testing.T) {
		session, _, recordName, shard, _ := outputV3FileShardFixture(t)
		bound, closeErr, err := session.openBoundFileRecord(shard, recordName)
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		scanned := outputV3FileNamespaceRecord{
			shardName: recordName.Shard(), recordName: recordName.Name(), bound: bound,
		}
		if err := session.installScannedFileRecord(scanned, bound); err != nil {
			t.Fatalf("unchanged scanned record install = %v", err)
		}
	})
}

type outputV3NamespaceFaults struct {
	injected           error
	namesOverride      []string
	namesErrAt         int
	namesCalls         int
	classifyErrAt      int
	classifyCalls      int
	openErrAt          int
	openCalls          int
	childNamesErrAt    int
	childNamesCalls    int
	childCloseErrAt    int
	childCloseCalls    int
	childCreateFileErr error
}

type outputV3NamespaceFaultDirectory struct {
	outputcap.Directory
	faults *outputV3NamespaceFaults
	child  bool
}

func (directory *outputV3NamespaceFaultDirectory) Names(limit int) ([]string, error) {
	if directory.child {
		directory.faults.childNamesCalls++
		if directory.faults.childNamesCalls == directory.faults.childNamesErrAt {
			return nil, directory.faults.injected
		}
	} else {
		directory.faults.namesCalls++
		if directory.faults.namesCalls == directory.faults.namesErrAt {
			return nil, directory.faults.injected
		}
		if directory.faults.namesOverride != nil {
			return append([]string(nil), directory.faults.namesOverride...), nil
		}
	}
	return directory.Directory.Names(limit)
}

func (directory *outputV3NamespaceFaultDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	directory.faults.classifyCalls++
	if directory.faults.classifyCalls == directory.faults.classifyErrAt {
		return outputcap.EntryAbsent, false, directory.faults.injected
	}
	return directory.Directory.ClassifyExactEntry(name)
}

func (directory *outputV3NamespaceFaultDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	directory.faults.openCalls++
	if directory.faults.openCalls == directory.faults.openErrAt {
		return nil, directory.faults.injected
	}
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &outputV3NamespaceFaultDirectory{Directory: opened, faults: directory.faults, child: true}, nil
}

func (directory *outputV3NamespaceFaultDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	if wrapped, ok := other.(*outputV3NamespaceFaultDirectory); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func (directory *outputV3NamespaceFaultDirectory) CreateFile(name string, private bool, size int64) (outputcap.File, error) {
	if directory.faults.childCreateFileErr != nil {
		return nil, directory.faults.childCreateFileErr
	}
	return directory.Directory.CreateFile(name, private, size)
}

func (directory *outputV3NamespaceFaultDirectory) Close() error {
	closeErr := directory.Directory.Close()
	if !directory.child {
		return closeErr
	}
	directory.faults.childCloseCalls++
	if directory.faults.childCloseCalls == directory.faults.childCloseErrAt {
		return errors.Join(closeErr, directory.faults.injected)
	}
	return closeErr
}

type outputV3NamespaceNamesDirectory struct {
	outputcap.Directory
	namesErr     error
	changeOnCall int
	namesCalls   int
}

func (directory *outputV3NamespaceNamesDirectory) Names(limit int) ([]string, error) {
	directory.namesCalls++
	if directory.namesErr != nil {
		return nil, directory.namesErr
	}
	names, err := directory.Directory.Names(limit)
	if err == nil && directory.namesCalls == directory.changeOnCall {
		names = append(names, "aa")
	}
	return names, err
}

type outputV3NamespaceMutationRoot struct {
	outputcap.Directory
	targetShard string
	opens       int
}

func (directory *outputV3NamespaceMutationRoot) OpenDirectory(
	name string,
	private bool,
) (outputcap.Directory, error) {
	opened, err := directory.Directory.OpenDirectory(name, private)
	if err != nil || name != directory.targetShard {
		return opened, err
	}
	directory.opens++
	return &outputV3NamespaceMutationShard{
		Directory: opened,
		hideNames: directory.opens == 2,
	}, nil
}

type outputV3NamespaceMutationShard struct {
	outputcap.Directory
	hideNames bool
}

func (directory *outputV3NamespaceMutationShard) Names(limit int) ([]string, error) {
	if directory.hideNames {
		return nil, nil
	}
	return directory.Directory.Names(limit)
}

func (directory *outputV3NamespaceMutationShard) SameDirectory(other outputcap.Directory) (bool, error) {
	if wrapped, ok := other.(*outputV3NamespaceMutationShard); ok {
		other = wrapped.Directory
	}
	return directory.Directory.SameDirectory(other)
}

func outputV3NamespaceSelectionIndex(t *testing.T, selection transfer.OutputSelection, path string) int {
	t.Helper()
	for index, selected := range selection.Files() {
		if selected.Path == path {
			return index
		}
	}
	t.Fatalf("selection does not contain %q", path)
	return -1
}
