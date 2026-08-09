package checkpointcleaner

import (
	"bytes"
	"context"
	"errors"
	"path"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const c5ClosureLegacyRecordSuffix = ".state"

func TestC5ClosureClassifiesRootControlAndCurrentStateWithoutDeletionAuthority(t *testing.T) {
	bootstrap := legacyresume.BootstrapCandidatePrefix + strings.Repeat("1", 64)
	root := &c5ClosureDirectory{entries: map[string]c5ClosureEntry{
		legacyresume.ControlDirectory: {kind: outputcap.EntryDirectory, exact: true},
		"published.txt":               {kind: outputcap.EntryRegularFile, exact: true},
		bootstrap:                     {kind: outputcap.EntryDirectory, exact: true},
		strings.ToUpper(legacyresume.BootstrapCandidatePrefix) + strings.Repeat("2", 64): {kind: outputcap.EntryDirectory, exact: true},
		legacyLookingRootPrefix + "foreign":                                              {kind: outputcap.EntryDirectory, exact: true},
		strings.ToUpper(legacyresume.ControlDirectory):                                   {kind: outputcap.EntryDirectory, exact: false},
	}}
	run := &cleanupRun{root: root, approved: make(map[string]outputcap.EntryKind)}
	report := CheckpointCleanupReport{}
	present, err := run.inspectRootEntries(context.Background(), &report)
	if err != nil || !present {
		t.Fatalf("inspect root: present=%t err=%v", present, err)
	}
	if !c5ClosureHasEntry(report, "published.txt", cleanupDetailPublished) ||
		!c5ClosureHasEntry(report, bootstrap, cleanupDetailUnknown) ||
		!c5ClosureHasEntry(report, strings.ToUpper(legacyresume.ControlDirectory), cleanupDetailConflict) {
		t.Fatalf("root report = %+v", report.Entries)
	}
	if len(run.approved) != 0 {
		t.Fatalf("root classification granted deletion authority: %+v", run.approved)
	}

	controlTemporary := legacyresume.ControlRecord + ".tmp-" + strings.Repeat("3", 64)
	current := &c5ClosureDirectory{entries: map[string]c5ClosureEntry{
		legacyresume.CheckpointOwnership: {kind: outputcap.EntryRegularFile, exact: true},
		legacyresume.CheckpointLeases:    {kind: outputcap.EntryDirectory, exact: true},
		legacyresume.CheckpointIntents:   {kind: outputcap.EntryRegularFile, exact: true},
		FileCheckpointCleanupState:       {kind: outputcap.EntryRegularFile, exact: false},
		FileCheckpointCleanupLock:        {kind: outputcap.EntryRegularFile, exact: true},
		"foreign.current":                {kind: outputcap.EntryRegularFile, exact: true},
	}}
	control := &c5ClosureDirectory{
		entries: map[string]c5ClosureEntry{
			legacyresume.CheckpointDirectory: {kind: outputcap.EntryDirectory, exact: true},
			legacyresume.ControlRecord:       {kind: outputcap.EntryDirectory, exact: true},
			controlTemporary:                 {kind: outputcap.EntryRegularFile, exact: true},
			legacyresume.CoordinatorLock:     {kind: outputcap.EntryOther, exact: true},
			legacyresume.SessionsDirectory:   {kind: outputcap.EntryRegularFile, exact: true},
			"aliased.state":                  {kind: outputcap.EntryRegularFile, exact: false},
			"foreign.state":                  {kind: outputcap.EntryRegularFile, exact: true},
		},
		children: map[string]*c5ClosureDirectory{legacyresume.CheckpointDirectory: current},
	}
	run.control = control
	report = CheckpointCleanupReport{}
	if err := run.inspectControlEntries(context.Background(), &report); err != nil {
		t.Fatal(err)
	}
	if !run.legacy.controlRecord || !run.legacy.coordinatorLock || !run.legacy.sessionsDirectory {
		t.Fatalf("legacy observation = %+v", run.legacy)
	}
	if !run.approvedEntry(path.Join(legacyresume.ControlDirectory, controlTemporary), outputcap.EntryRegularFile) {
		t.Fatalf("canonical control temporary was not approved: %+v", run.approved)
	}
	for _, relative := range []string{
		path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord),
		path.Join(legacyresume.ControlDirectory, legacyresume.CoordinatorLock),
		path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory),
		path.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, "foreign.current"),
	} {
		if _, ok := run.approved[relative]; ok {
			t.Fatalf("conflicting/current path %q gained deletion authority", relative)
		}
	}
	if !c5ClosureHasEntry(report, path.Join(legacyresume.ControlDirectory, "foreign.state"), cleanupDetailUnknown) ||
		!c5ClosureHasEntry(report, path.Join(legacyresume.ControlDirectory, legacyresume.ControlRecord), cleanupDetailConflict) ||
		!c5ClosureHasEntry(report, path.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, "foreign.current"), cleanupDetailConflict) {
		t.Fatalf("control report = %+v", report.Entries)
	}
}

func TestC5ClosurePreservesUnknownConflictingAndCurrentSessionChildren(t *testing.T) {
	intentName := strings.Repeat("a", 64)
	candidateName := legacyresume.SessionCandidatePrefix + strings.Repeat("b", 32)
	conflictingSession := strings.Repeat("c", 32)
	intent := &c5ClosureDirectory{
		entries: map[string]c5ClosureEntry{
			legacyresume.CheckpointDirectory: {kind: outputcap.EntryDirectory, exact: true},
			candidateName:                    {kind: outputcap.EntryDirectory, exact: true},
			conflictingSession:               {kind: outputcap.EntryDirectory, exact: true},
			"foreign-child":                  {kind: outputcap.EntryRegularFile, exact: true},
		},
		children: map[string]*c5ClosureDirectory{
			candidateName: {entries: map[string]c5ClosureEntry{}},
		},
	}
	sessions := &c5ClosureDirectory{
		entries: map[string]c5ClosureEntry{
			intentName:       {kind: outputcap.EntryDirectory, exact: true},
			"foreign-intent": {kind: outputcap.EntryDirectory, exact: true},
		},
		children: map[string]*c5ClosureDirectory{intentName: intent},
	}
	control := &c5ClosureDirectory{
		children: map[string]*c5ClosureDirectory{legacyresume.SessionsDirectory: sessions},
	}
	run := &cleanupRun{control: control, approved: make(map[string]outputcap.EntryKind)}
	report := CheckpointCleanupReport{}
	if err := run.inspectAndLockLegacySessions(context.Background(), &report); err != nil {
		t.Fatal(err)
	}
	base := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory, intentName)
	if !c5ClosureHasEntry(report, path.Join(base, legacyresume.CheckpointDirectory), cleanupDetailSeparateOwnership) ||
		!c5ClosureHasEntry(report, path.Join(base, "foreign-child"), cleanupDetailUnknown) ||
		!c5ClosureHasEntry(report, path.Join(base, conflictingSession), cleanupDetailConflict) ||
		!c5ClosureHasEntry(report, path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory, "foreign-intent"), cleanupDetailUnknown) {
		t.Fatalf("session report = %+v", report.Entries)
	}
	for _, relative := range []string{
		path.Join(base, legacyresume.CheckpointDirectory),
		path.Join(base, "foreign-child"),
		path.Join(base, conflictingSession),
	} {
		if _, ok := run.approved[relative]; ok {
			t.Fatalf("retained child %q gained deletion authority", relative)
		}
	}
	if len(run.sessionLocks) != 0 {
		t.Fatalf("empty candidate unexpectedly acquired locks: %+v", run.sessionLocks)
	}
}

func TestC5ClosureRejectsMalformedShardMarkers(t *testing.T) {
	validBase := "aa" + strings.Repeat("1", 62)
	wrongKindBase := "aa" + strings.Repeat("2", 62)
	aliasedBase := "aa" + strings.Repeat("3", 62)
	validName := validBase + c5ClosureLegacyRecordSuffix
	wrongKindName := wrongKindBase + c5ClosureLegacyRecordSuffix
	aliasedName := aliasedBase + c5ClosureLegacyRecordSuffix
	wrongShardName := "bb" + strings.Repeat("4", 62) + c5ClosureLegacyRecordSuffix
	shard := &c5ClosureDirectory{entries: map[string]c5ClosureEntry{
		validName:      {kind: outputcap.EntryRegularFile, exact: true},
		wrongKindName:  {kind: outputcap.EntryDirectory, exact: true},
		aliasedName:    {kind: outputcap.EntryRegularFile, exact: false},
		wrongShardName: {kind: outputcap.EntryRegularFile, exact: true},
		"short.state":  {kind: outputcap.EntryRegularFile, exact: true},
	}}
	run := &cleanupRun{approved: make(map[string]outputcap.EntryKind)}
	report := CheckpointCleanupReport{}
	base := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory, "intent", "session", legacyresume.FilesDirectory, "aa")
	if err := run.inspectLegacyShard(context.Background(), shard, "aa", base, legacyFiles, &report); err != nil {
		t.Fatal(err)
	}
	if !run.approvedEntry(path.Join(base, validName), outputcap.EntryRegularFile) {
		t.Fatalf("canonical marker was not approved: %+v", run.approved)
	}
	if len(run.approved) != 1 {
		t.Fatalf("malformed markers gained deletion authority: %+v", run.approved)
	}
	if !c5ClosureHasEntry(report, path.Join(base, wrongKindName), cleanupDetailConflict) ||
		!c5ClosureHasEntry(report, path.Join(base, wrongShardName), cleanupDetailUnknown) {
		t.Fatalf("marker report = %+v", report.Entries)
	}
}

func TestC5ClosureBoundsTraversalWithoutBuildingAFilesystemWave(t *testing.T) {
	requested := 0
	overflow := &c5ClosureDirectory{names: func(limit int) ([]string, error) {
		requested = limit
		// A fake oversized answer tests the hard bound without creating any
		// filesystem entries or making traversal runtime depend on the host.
		return make([]string, maxCleanerEntries+1), nil
	}}
	if _, err := boundedNames(overflow); !errors.Is(err, ErrCheckpointCleanerLimit) {
		t.Fatalf("oversized directory = %v", err)
	}
	if requested != maxCleanerEntries+1 {
		t.Fatalf("enumeration limit = %d", requested)
	}
	if _, err := boundedNames(nil); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nil directory = %v", err)
	}
	namesFailure := errors.New("enumeration failed")
	if _, err := boundedNames(&c5ClosureDirectory{names: func(int) ([]string, error) {
		return nil, namesFailure
	}}); !errors.Is(err, namesFailure) {
		t.Fatalf("enumeration failure = %v", err)
	}

	run := &cleanupRun{observed: maxCleanerEntries}
	report := CheckpointCleanupReport{}
	if err := run.observeEntry(context.Background(), &report); !errors.Is(err, ErrCheckpointCleanerLimit) {
		t.Fatalf("cumulative traversal limit = %v", err)
	}
	if report.Scanned != 0 {
		t.Fatalf("overflow entry was reported as scanned: %+v", report)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	run = &cleanupRun{}
	if err := run.observeEntry(canceled, &report); !errors.Is(err, context.Canceled) || run.observed != 0 {
		t.Fatalf("canceled observation: observed=%d err=%v", run.observed, err)
	}
}

func TestC5ClosureRejectsActionTimeReplacement(t *testing.T) {
	base := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory)
	tests := []struct {
		name     string
		entry    c5ClosureEntry
		approved outputcap.EntryKind
		wantErr  bool
	}{
		{name: "removed concurrently", entry: c5ClosureEntry{kind: outputcap.EntryAbsent, exact: true}},
		{name: "name alias", entry: c5ClosureEntry{kind: outputcap.EntryRegularFile, exact: false}, approved: outputcap.EntryRegularFile, wantErr: true},
		{name: "kind replacement", entry: c5ClosureEntry{kind: outputcap.EntryDirectory, exact: true}, approved: outputcap.EntryRegularFile, wantErr: true},
		{name: "unobserved child", entry: c5ClosureEntry{kind: outputcap.EntryRegularFile, exact: true}, wantErr: true},
		{name: "unsupported object", entry: c5ClosureEntry{kind: outputcap.EntryOther, exact: true}, approved: outputcap.EntryOther, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			name := "target"
			relative := path.Join(base, name)
			run := &cleanupRun{approved: make(map[string]outputcap.EntryKind)}
			if test.approved != outputcap.EntryAbsent {
				run.approved[relative] = test.approved
			}
			directory := &c5ClosureDirectory{entries: map[string]c5ClosureEntry{name: test.entry}}
			err := run.removeTreeEntry(
				context.Background(), directory, base, name,
				&cleanerState{}, new([]byte), &CheckpointCleanupReport{},
			)
			if test.wantErr && !errors.Is(err, ErrCheckpointCleanerOwnership) {
				t.Fatalf("replacement error = %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatal(err)
			}
		})
	}

	lockPath := path.Join(base, legacyresume.SessionLock)
	run := &cleanupRun{approved: map[string]outputcap.EntryKind{lockPath: outputcap.EntryRegularFile}}
	directory := &c5ClosureDirectory{entries: map[string]c5ClosureEntry{
		legacyresume.SessionLock: {kind: outputcap.EntryRegularFile, exact: true},
	}}
	if err := run.removeTreeEntry(
		context.Background(), directory, base, legacyresume.SessionLock,
		&cleanerState{}, new([]byte), &CheckpointCleanupReport{},
	); err != nil {
		t.Fatalf("held session lock was removed by tree walk: %v", err)
	}

	called := false
	if err := run.applyRemoval(
		context.Background(), path.Join(base, "late"), &cleanerState{}, new([]byte),
		&CheckpointCleanupReport{}, func() error { called = true; return nil },
	); !errors.Is(err, ErrCheckpointCleanerOwnership) || called {
		t.Fatalf("unplanned mutation: called=%t err=%v", called, err)
	}
}

func TestC5ClosureRejectsCanonicalStateBoundToDifferentAuthority(t *testing.T) {
	rootIdentity := bytes.Repeat([]byte{0x5a}, legacyresume.RootIdentityBytes)
	run := &cleanupRun{
		cleaner: &OneShotCheckpointCleaner{config: OneShotCheckpointCleanerConfig{
			BackendID: legacyresume.NativeFilesystemBackend,
		}},
		rootBinding:   rootIdentity,
		certification: legacyresume.CertificationWindowsNTFSProcessRestart,
		durability:    transfer.DurabilityProcessRestart,
	}
	base := cleanerState{
		Schema: cleanerStateSchema, BackendID: string(legacyresume.NativeFilesystemBackend),
		Certification: run.certification, RootIdentity: append([]byte(nil), rootIdentity...),
		Durability: uint8(transfer.DurabilityProcessRestart), RunGeneration: 1, Complete: true,
	}
	base.Checksum = stateChecksum(base)
	if !run.validState(base) {
		t.Fatal("canonical state was rejected")
	}
	tests := []struct {
		name   string
		mutate func(*cleanerState)
	}{
		{name: "schema", mutate: func(state *cleanerState) { state.Schema++ }},
		{name: "backend", mutate: func(state *cleanerState) { state.BackendID = "foreign" }},
		{name: "certification", mutate: func(state *cleanerState) { state.Certification = legacyresume.CertificationLinuxExt4ProcessRestart }},
		{name: "root", mutate: func(state *cleanerState) {
			state.RootIdentity = bytes.Repeat([]byte{0x6b}, legacyresume.RootIdentityBytes)
		}},
		{name: "durability", mutate: func(state *cleanerState) { state.Durability = uint8(transfer.DurabilityPowerLoss) }},
		{name: "generation", mutate: func(state *cleanerState) { state.RunGeneration = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.RootIdentity = append([]byte(nil), base.RootIdentity...)
			test.mutate(&candidate)
			candidate.Checksum = stateChecksum(candidate)
			if run.validState(candidate) {
				t.Fatalf("foreign canonical state was accepted: %+v", candidate)
			}
		})
	}
	corruptChecksum := base
	corruptChecksum.Checksum = bytes.Repeat([]byte{0xff}, 32)
	if run.validState(corruptChecksum) {
		t.Fatal("state with corrupt checksum was accepted")
	}
}

func TestC5ClosureFailsClosedBeforeInspection(t *testing.T) {
	if _, err := NewOneShotCheckpointCleaner(OneShotCheckpointCleanerConfig{}); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nil platform = %v", err)
	}
	rootless := &c5ClosurePlatform{}
	if _, err := NewOneShotCheckpointCleaner(OneShotCheckpointCleanerConfig{Platform: rootless}); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("rootless platform = %v", err)
	}
	platform := &c5ClosurePlatform{root: &c5ClosureDirectory{}}
	if _, err := NewOneShotCheckpointCleaner(OneShotCheckpointCleanerConfig{
		Platform: platform, BackendID: " invalid ",
	}); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("invalid backend = %v", err)
	}
	cleaner, err := NewOneShotCheckpointCleaner(OneShotCheckpointCleanerConfig{Platform: platform})
	if err != nil || cleaner.config.BackendID != legacyresume.NativeFilesystemBackend {
		t.Fatalf("default backend: cleaner=%+v err=%v", cleaner, err)
	}
	if _, err := (*OneShotCheckpointCleaner)(nil).Run(context.Background()); !errors.Is(err, ErrCheckpointCleanerOwnership) {
		t.Fatalf("nil cleaner = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cleaner.Run(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleaner = %v", err)
	}
	acquireFailure := errors.New("guard acquisition failed")
	platform.acquireErr = acquireFailure
	if _, err := cleaner.Run(context.Background()); !errors.Is(err, acquireFailure) {
		t.Fatalf("guard failure = %v", err)
	}

	binding := c5ClosureRootBinding(t, "empty-root")
	emptyRoot := &c5ClosureFaultDirectory{
		classify: func(string) (outputcap.EntryKind, bool, error) {
			return outputcap.EntryAbsent, true, nil
		},
		sameDirectory: func(outputcap.Directory) (bool, error) { return true, nil },
	}
	emptyPlatform := &c5ClosurePlatform{
		root: emptyRoot, guard: &c5ClosureStaticGuard{root: emptyRoot}, binding: binding,
		certification: outputcap.CertificationWindowsNTFSProcessRestart,
		durability:    transfer.DurabilityProcessRestart,
	}
	emptyCleaner, err := NewOneShotCheckpointCleaner(OneShotCheckpointCleanerConfig{Platform: emptyPlatform})
	if err != nil {
		t.Fatal(err)
	}
	report, err := emptyCleaner.Run(context.Background())
	if err != nil || !report.Complete || report.Status != CheckpointCleanupStatusComplete || report.Removed != 0 {
		t.Fatalf("empty root cleanup = %+v, %v", report, err)
	}
}
