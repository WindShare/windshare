package osfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3DirectPublicationNeverLinksReplacedAnchor(t *testing.T) {
	root := v3RecoveryRoot(t)
	payload := []byte("owned-by-transaction")
	foreign := bytes.Repeat([]byte{0xf1}, len(payload))
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	gate := &outputV3PublicationReplacementGate{target: v3RecoveryFilePath}
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryPublicationRaceAuthority(t, root, sessionIDs, gate)
	opened := v3RecoveryOpen(t, authority, root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	record := transaction.resumable.Bound().Record()
	gate.replace = outputV3ReplaceAnchorAtLink(root, selection, opened.Session.SessionID(), record, foreign)

	settlement, commitErr := transaction.Commit(context.Background())
	if commitErr != nil || settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("anchor replacement settlement = (%v, %v), want durable quarantine", settlement.Kind(), commitErr)
	}
	persisted := readOutputV3PublicationAuthorityRecord(t, root, selection, opened.Session.SessionID(), record)
	if persisted.Phase() != resumestate.FileQuarantined ||
		persisted.QuarantineReason() != resumestate.QuarantineAnchorUnsafe {
		t.Fatalf("anchor replacement record = (phase=%v, reason=%v)",
			persisted.Phase(), persisted.QuarantineReason())
	}
	assertOutputV3ReplacementGateFired(t, gate)
	assertOutputV3FinalNeverForeign(t, root, payload, foreign)
	v3RecoveryCloseSession(t, opened.Session)
	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
	retry, retryErr := reopened.Session.BeginFile(
		context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, uint64(len(payload))),
	)
	retrySettlement, immediate := retry.ImmediateSettlement()
	if retryErr != nil || !immediate || retrySettlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("anchor replacement restart = (kind=%v/%t, err=%v)",
			retrySettlement.Kind(), immediate, retryErr)
	}
}

func TestOutputV3RecoveryPublicationNeverLinksReplacedAnchor(t *testing.T) {
	root := v3RecoveryRoot(t)
	payload := []byte("owned-by-recovery")
	foreign := bytes.Repeat([]byte{0xf2}, len(payload))
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	record := v3RecoveryPreparePublishingCut(t, opened.Session, file, payload, "missing")
	sessionID := opened.Session.SessionID()
	v3RecoveryCloseSession(t, opened.Session)

	gate := &outputV3PublicationReplacementGate{
		target:  v3RecoveryFilePath,
		replace: outputV3ReplaceAnchorAtLink(root, selection, sessionID, record, foreign),
	}
	reopened := v3RecoveryOpen(
		t, v3RecoveryPublicationRaceAuthority(t, root, sessionIDs, gate), root, selection,
	)
	start, beginErr := reopened.Session.BeginFile(
		context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, uint64(len(payload))),
	)
	if beginErr == nil {
		settlement, immediate := start.ImmediateSettlement()
		if !immediate || settlement.Kind() != transfer.FileQuarantined {
			t.Fatalf("recovery replacement start = (%v, %t), want quarantine or safe link rejection", settlement.Kind(), immediate)
		}
	}
	assertOutputV3ReplacementGateFired(t, gate)
	assertOutputV3FinalNeverForeign(t, root, payload, foreign)
	v3RecoveryCloseSession(t, reopened.Session)
}

func TestOutputV3DirectPublicationPersistsPostPublishingUnsafeLinkCut(t *testing.T) {
	root := v3RecoveryRoot(t)
	payload := []byte("unsafe-link-cut")
	selection := v3RecoverySelection(t, true, uint64(len(payload)))
	sessionIDs := &v3RecoverySessionIDs{}
	opened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	file := v3RecoveryOutputFile(t, opened.Session, selection, uint64(len(payload)))
	transaction := v3RecoveryBeginTransaction(t, opened.Session, file).(*filesystemFileTransaction)
	if err := transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	record := transaction.resumable.Bound().Record()
	originalPlatform := opened.Session.platform
	failure := errors.Join(errOutputV3Unsafe, errors.New("unsafe publication primitive"))
	faults := &outputV3PublicationDirectoryFaults{linkErr: failure}
	opened.Session.platform = &outputV3PublicationPlatform{
		outputV3Platform: originalPlatform,
		root:             &outputV3PublicationDirectory{outputV3Directory: originalPlatform.Root(), faults: faults},
	}

	settlement, commitErr := transaction.Commit(context.Background())
	opened.Session.platform = originalPlatform
	if commitErr != nil || settlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("unsafe link settlement = (kind=%v, err=%v)", settlement.Kind(), commitErr)
	}
	persisted := readOutputV3PublicationAuthorityRecord(t, root, selection, opened.Session.SessionID(), record)
	if persisted.Phase() != resumestate.FileQuarantined ||
		persisted.QuarantineReason() != resumestate.QuarantinePublicationHistory {
		t.Fatalf("unsafe link record = (phase=%v, reason=%v)", persisted.Phase(), persisted.QuarantineReason())
	}
	v3RecoveryCloseSession(t, opened.Session)

	reopened := v3RecoveryOpen(t, v3RecoveryAuthority(t, root, sessionIDs), root, selection)
	t.Cleanup(func() { v3RecoveryCloseSession(t, reopened.Session) })
	retry, retryErr := reopened.Session.BeginFile(
		context.Background(), v3RecoveryOutputFile(t, reopened.Session, selection, uint64(len(payload))),
	)
	retrySettlement, immediate := retry.ImmediateSettlement()
	if retryErr != nil || !immediate || retrySettlement.Kind() != transfer.FileQuarantined {
		t.Fatalf("unsafe link restart = (kind=%v/%t, err=%v)",
			retrySettlement.Kind(), immediate, retryErr)
	}
}

func outputV3ReplaceAnchorAtLink(
	root string,
	selection transfer.OutputSelection,
	sessionID transfer.OutputSessionID,
	record resumestate.FileRecord,
	foreign []byte,
) func() error {
	return func() error {
		anchor := resumestate.AnchorName(record.OutputObject())
		path := filepath.Join(
			v3RecoverySessionPath(root, selection, sessionID),
			resumestate.AnchorsDirectoryName,
			anchor.Shard(),
			anchor.Name(),
		)
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.WriteFile(path, foreign, 0o600)
	}
}

func assertOutputV3FinalNeverForeign(t *testing.T, root string, payload, foreign []byte) {
	t.Helper()
	actual, err := os.ReadFile(filepath.Join(root, v3RecoveryFilePath))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(actual, foreign) {
		t.Fatalf("publication exposed replacement bytes %q", actual)
	}
	if !bytes.Equal(actual, payload) {
		t.Fatalf("publication exposed unexpected bytes %q", actual)
	}
}

type outputV3PublicationReplacementGate struct {
	mu      sync.Mutex
	target  string
	replace func() error
	fired   bool
	failure error
}

func (gate *outputV3PublicationReplacementGate) trigger(name string) error {
	gate.mu.Lock()
	if gate.fired || name != gate.target {
		gate.mu.Unlock()
		return nil
	}
	gate.fired = true
	replace := gate.replace
	gate.mu.Unlock()
	if replace == nil {
		return errors.New("publication replacement hook is absent")
	}
	err := replace()
	gate.mu.Lock()
	gate.failure = err
	gate.mu.Unlock()
	return err
}

func assertOutputV3ReplacementGateFired(t *testing.T, gate *outputV3PublicationReplacementGate) {
	t.Helper()
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.fired || gate.failure != nil {
		t.Fatalf("publication replacement gate = (fired=%t, failure=%v)", gate.fired, gate.failure)
	}
}

type outputV3PublicationRacePlatform struct {
	outputV3Platform
	gate *outputV3PublicationReplacementGate
}

func (platform *outputV3PublicationRacePlatform) Root() outputV3Directory {
	return &outputV3PublicationRaceDirectory{
		outputV3Directory: platform.outputV3Platform.Root(),
		gate:              platform.gate,
	}
}

func (platform *outputV3PublicationRacePlatform) AcquirePublicOperationGuard() (
	outputV3PublicOperationGuard,
	error,
) {
	return acquireOutputV3DecoratedPublicOperationGuard(
		platform.outputV3Platform,
		func(root outputV3Directory) outputV3Directory {
			return &outputV3PublicationRaceDirectory{outputV3Directory: root, gate: platform.gate}
		},
	)
}

type outputV3PublicationRaceDirectory struct {
	outputV3Directory
	gate *outputV3PublicationReplacementGate
}

func (directory *outputV3PublicationRaceDirectory) Duplicate() (outputV3Directory, error) {
	duplicate, err := directory.outputV3Directory.Duplicate()
	if err != nil {
		return nil, err
	}
	return &outputV3PublicationRaceDirectory{outputV3Directory: duplicate, gate: directory.gate}, nil
}

func (directory *outputV3PublicationRaceDirectory) SameDirectory(other outputV3Directory) (bool, error) {
	if wrapped, ok := other.(*outputV3PublicationRaceDirectory); ok {
		other = wrapped.outputV3Directory
	}
	return directory.outputV3Directory.SameDirectory(other)
}

func (directory *outputV3PublicationRaceDirectory) LinkFileNoReplace(
	source outputV3File,
	name string,
) (outputV3File, error) {
	if err := directory.gate.trigger(name); err != nil {
		return nil, err
	}
	return directory.outputV3Directory.LinkFileNoReplace(source, name)
}

func v3RecoveryPublicationRaceAuthority(
	t *testing.T,
	root string,
	sessions *v3RecoverySessionIDs,
	gate *outputV3PublicationReplacementGate,
) *FilesystemOutputAuthority {
	t.Helper()
	authority := v3RecoveryAuthority(t, root, sessions)
	authority.platformFactory = func(path string, create bool) (outputV3Platform, error) {
		platform, err := openOutputV3Platform(path, create)
		if err != nil {
			return nil, err
		}
		return &outputV3PublicationRacePlatform{outputV3Platform: platform, gate: gate}, nil
	}
	return authority
}
