package resumestate

import (
	"bytes"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputObjectIDGenerationAndParsing(t *testing.T) {
	raw := bytes.Repeat([]byte{9}, OutputObjectIDBytes)
	id, err := GenerateOutputObjectID(bytes.NewReader(raw))
	if err != nil || !bytes.Equal(id.Bytes(), raw) || id.String() == "" || id.IsZero() {
		t.Fatalf("generated ID = %v, %v", id, err)
	}
	copyBytes := id.Bytes()
	copyBytes[0]++
	if id[0] != 9 {
		t.Fatal("Bytes exposed mutable identity storage")
	}
	parsed, err := OutputObjectIDFromBytes(raw)
	if err != nil || parsed != id {
		t.Fatalf("parsed ID = %v, %v", parsed, err)
	}
	for _, test := range []struct {
		name   string
		reader io.Reader
	}{
		{name: "nil", reader: nil},
		{name: "short", reader: bytes.NewReader(raw[:3])},
		{name: "zero", reader: bytes.NewReader(make([]byte, OutputObjectIDBytes))},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := GenerateOutputObjectID(test.reader); err == nil {
				t.Fatal("invalid entropy source accepted")
			}
		})
	}
	if _, err := OutputObjectIDFromBytes(raw[:4]); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("short parse error = %v", err)
	}
}

func TestBootstrapNonceHasIndependentRandomSemantics(t *testing.T) {
	raw := bytes.Repeat([]byte{0xab}, BootstrapNonceBytes)
	nonce, err := GenerateBootstrapNonce(bytes.NewReader(raw))
	if err != nil || nonce.IsZero() || !bytes.Equal(nonce.Bytes(), raw) {
		t.Fatalf("nonce = %v, %v", nonce, err)
	}
	parsed, err := BootstrapNonceFromBytes(raw)
	if err != nil || parsed != nonce {
		t.Fatalf("parsed nonce = %v, %v", parsed, err)
	}
	for _, reader := range []io.Reader{nil, bytes.NewReader(raw[:1]), bytes.NewReader(make([]byte, BootstrapNonceBytes))} {
		if _, err := GenerateBootstrapNonce(reader); err == nil {
			t.Fatal("invalid bootstrap entropy accepted")
		}
	}
}

func TestOutputRootBindingRequiresVolumeObjectAndCertificationClaims(t *testing.T) {
	volume := []byte("volume-identity")
	object := []byte("root-object-identity")
	binding, err := NewOutputRootBinding(CertificationLinuxExt4ProcessRestart, volume, object)
	if err != nil || binding.IsZero() || binding.Certification() != CertificationLinuxExt4ProcessRestart ||
		len(binding.Bytes()) != OutputRootBindingBytes || binding.String() == "" {
		t.Fatalf("root binding = %+v, %v", binding, err)
	}
	repeated, err := NewOutputRootBinding(CertificationLinuxExt4ProcessRestart, volume, object)
	if err != nil || repeated != binding {
		t.Fatalf("repeated root binding = %+v, %v", repeated, err)
	}
	changedVolume, _ := NewOutputRootBinding(CertificationLinuxExt4ProcessRestart, []byte("other-volume"), object)
	changedObject, _ := NewOutputRootBinding(CertificationLinuxExt4ProcessRestart, volume, []byte("other-object"))
	changedCertification, _ := NewOutputRootBinding(CertificationWindowsNTFSProcessRestart, volume, object)
	if changedVolume == binding || changedObject == binding || changedCertification == binding {
		t.Fatal("root binding omitted a filesystem authority claim")
	}
	owned := binding.Bytes()
	owned[0]++
	if reflect.DeepEqual(owned, binding.Bytes()) {
		t.Fatal("root binding exposed mutable digest storage")
	}
	restored, err := outputRootBindingFromBytes(binding.Certification(), binding.Bytes())
	if err != nil || restored != binding {
		t.Fatalf("restored root binding = %+v, %v", restored, err)
	}

	tooLarge := bytes.Repeat([]byte{1}, MaxRootIdentityClaimBytes+1)
	for _, invalid := range []struct {
		certification CertificationID
		volume        []byte
		object        []byte
	}{
		{certification: CertificationLinuxExt4ProcessRestart, object: object},
		{certification: CertificationLinuxExt4ProcessRestart, volume: volume},
		{certification: CertificationLinuxExt4ProcessRestart, volume: tooLarge, object: object},
		{certification: "uncertified", volume: volume, object: object},
	} {
		if _, err := NewOutputRootBinding(invalid.certification, invalid.volume, invalid.object); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid root binding error = %v", err)
		}
	}
	if _, err := outputRootBindingFromBytes(binding.Certification(), binding.Bytes()[:3]); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("short restored binding error = %v", err)
	}
	if _, err := NewControl(ControlSpec{
		Backend: testBackend(t), OutputRoot: binding,
		Certification: CertificationWindowsNTFSProcessRestart,
		Durability:    transfer.DurabilityProcessRestart, Generation: 1,
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("mismatched binding certification error = %v", err)
	}
}

func TestOutputAncestryBindingCommitsToExactCanonicalClosure(t *testing.T) {
	selection := testSelection(t, 10)
	root := testRootBinding(t)
	claims := []OutputAncestryIdentityClaim{
		{CanonicalPath: "", IdentityClaim: []byte("root-object")},
		{CanonicalPath: "folder", IdentityClaim: []byte("folder-object")},
	}
	binding, err := NewOutputAncestryBinding(root, selection.Identity(), claims)
	if err != nil || binding.IsZero() || len(binding.Bytes()) != OutputAncestryBindingBytes || binding.String() == "" {
		t.Fatalf("ancestry binding = %v, %v", binding, err)
	}
	owned := binding.Bytes()
	owned[0]++
	if bytes.Equal(owned, binding.Bytes()) {
		t.Fatal("ancestry Bytes exposed mutable digest storage")
	}
	restored, err := outputAncestryBindingFromBytes(binding.Bytes())
	if err != nil || restored != binding {
		t.Fatalf("restored ancestry binding = %v, %v", restored, err)
	}

	changedClaim := []OutputAncestryIdentityClaim{
		{CanonicalPath: "", IdentityClaim: []byte("root-object")},
		{CanonicalPath: "folder", IdentityClaim: []byte("replacement-object")},
	}
	changed, err := NewOutputAncestryBinding(root, selection.Identity(), changedClaim)
	if err != nil || changed == binding {
		t.Fatalf("changed identity binding = %v, %v", changed, err)
	}
	otherRoot := testRootBindingFor(t, CertificationLinuxExt4ProcessRestart, 9)
	rootChanged, err := NewOutputAncestryBinding(otherRoot, selection.Identity(), claims)
	if err != nil || rootChanged == binding {
		t.Fatalf("changed root binding = %v, %v", rootChanged, err)
	}
	otherSelection := testSelection(t, 11)
	selectionChanged, err := NewOutputAncestryBinding(root, otherSelection.Identity(), claims)
	if err != nil || selectionChanged == binding {
		t.Fatalf("changed selection binding = %v, %v", selectionChanged, err)
	}

	tooLarge := bytes.Repeat([]byte{1}, MaxAncestryIdentityClaimBytes+1)
	invalid := [][]OutputAncestryIdentityClaim{
		nil,
		{{CanonicalPath: "folder", IdentityClaim: []byte("missing-root")}},
		{{CanonicalPath: "", IdentityClaim: nil}},
		{{CanonicalPath: "", IdentityClaim: tooLarge}},
		{{CanonicalPath: "", IdentityClaim: []byte("root")}, {CanonicalPath: "folder/../other", IdentityClaim: []byte("bad")}},
		{{CanonicalPath: "", IdentityClaim: []byte("root")}, {CanonicalPath: "folder", IdentityClaim: []byte("one")}, {CanonicalPath: "folder", IdentityClaim: []byte("two")}},
		{{CanonicalPath: "", IdentityClaim: []byte("root")}, {CanonicalPath: "z", IdentityClaim: []byte("one")}, {CanonicalPath: "a", IdentityClaim: []byte("two")}},
	}
	for _, current := range invalid {
		if _, err := NewOutputAncestryBinding(root, selection.Identity(), current); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid ancestry claims %+v error = %v", current, err)
		}
	}
	if _, err := NewOutputAncestryBinding(OutputRootBinding{}, selection.Identity(), claims); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero root error = %v", err)
	}
	if _, err := NewOutputAncestryBinding(root, transfer.SelectionIdentity{}, claims); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero selection error = %v", err)
	}
	for _, raw := range [][]byte{nil, make([]byte, OutputAncestryBindingBytes), binding.Bytes()[:3]} {
		if _, err := outputAncestryBindingFromBytes(raw); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid restored ancestry binding error = %v", err)
		}
	}
}

func TestHeaderCarriesCanonicalSelectionScopedResumeIdentity(t *testing.T) {
	selection := testSelection(t, 10)
	header := testHeader(t)
	if header.ResumeNamespace() != header.ResumeIntent() || header.ResumeIntent().IsZero() ||
		header.SelectionIdentity() != selection.Identity() || header.ShareInstance() != selection.ShareInstance() ||
		header.SyntheticRoot() != selection.SyntheticRoot() || header.SelectedDirectoryCount() != 1 ||
		header.SelectedFileCount() != 1 || header.OutputAncestry().IsZero() {
		t.Fatalf("header scope = %+v", header)
	}
	if _, err := NewHeader(HeaderSpec{
		Backend: header.Backend(), SessionID: header.SessionID(), OutputRoot: header.OutputRoot(),
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound selection error = %v", err)
	}
	claims := headerClaims{
		backend: header.backend, sessionID: header.sessionID, shareInstance: header.shareInstance,
		syntheticRoot: header.syntheticRoot, resumeIntent: header.resumeIntent,
		selectionIdentity:      header.selectionIdentity,
		selectedDirectoryCount: header.selectedDirectoryCount, selectedFileCount: header.selectedFileCount,
		outputRoot: header.outputRoot, outputAncestry: header.outputAncestry,
		lifecycle: header.lifecycle, stateGeneration: header.stateGeneration,
	}
	claims.syntheticRoot = catalog.DirectoryID{}
	if _, err := newHeaderFromClaims(claims); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero synthetic root error = %v", err)
	}
	claims.syntheticRoot = header.syntheticRoot
	claims.selectedFileCount = MaxFilesPerSession
	claims.selectedDirectoryCount = 0
	if _, err := newHeaderFromClaims(claims); err != nil {
		t.Fatalf("exact file-count limit rejected: %v", err)
	}
	claims.selectedFileCount = MaxFilesPerSession + 1
	if _, err := newHeaderFromClaims(claims); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("file-count limit + 1 error = %v", err)
	}
	claims.selectedFileCount = MaxFilesPerSession
	claims.selectedDirectoryCount = 1
	if _, err := newHeaderFromClaims(claims); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("oversized selected plan error = %v", err)
	}
}

func TestPersistedEnumWireValuesAreStable(t *testing.T) {
	values := map[string]uint8{
		"session active": uint8(SessionActive), "session pausing": uint8(SessionPausing),
		"session paused": uint8(SessionPaused), "session paused attention": uint8(SessionPausedNeedsAttention),
		"session completing": uint8(SessionCompleting), "session discarding": uint8(SessionDiscarding),
		"file reserved": uint8(FileReserved), "file witnessed": uint8(FileWitnessed),
		"file publishing": uint8(FilePublishing), "file publish blocked": uint8(FilePublishBlocked),
		"file published": uint8(FilePublished), "file retiring": uint8(FileRetiring),
		"file quarantined":                uint8(FileQuarantined),
		"quarantine anchor missing":       uint8(QuarantineAnchorMissing),
		"quarantine anchor unsafe":        uint8(QuarantineAnchorUnsafe),
		"quarantine stage missing":        uint8(QuarantineStageMissing),
		"quarantine stage mismatch":       uint8(QuarantineStageMismatch),
		"quarantine stage unsafe":         uint8(QuarantineStageUnsafe),
		"quarantine final mismatch":       uint8(QuarantineFinalMismatch),
		"quarantine final unsafe":         uint8(QuarantineFinalUnsafe),
		"quarantine partial object":       uint8(QuarantinePartialObjectCreation),
		"quarantine publication history":  uint8(QuarantinePublicationHistory),
		"quarantine metadata mismatch":    uint8(QuarantineMetadataMismatch),
		"quarantine update temporary":     uint8(QuarantineUpdateTemporary),
		"quarantine duplicate object":     uint8(QuarantineOutputObjectDuplicate),
		"retirement published":            uint8(RetirementPublished),
		"retirement isolated":             uint8(RetirementIsolatedFailure),
		"retirement collision":            uint8(RetirementPreObjectCollision),
		"retirement invalidated revision": uint8(RetirementInvalidatedRevision),
	}
	expected := uint8(1)
	for _, group := range [][]string{
		{"session active", "session pausing", "session paused", "session paused attention", "session completing", "session discarding"},
		{"file reserved", "file witnessed", "file publishing", "file publish blocked", "file published", "file retiring", "file quarantined"},
		{"quarantine anchor missing", "quarantine anchor unsafe", "quarantine stage missing", "quarantine stage mismatch", "quarantine stage unsafe", "quarantine final mismatch", "quarantine final unsafe", "quarantine partial object", "quarantine publication history", "quarantine metadata mismatch", "quarantine update temporary", "quarantine duplicate object"},
		{"retirement published", "retirement isolated", "retirement collision", "retirement invalidated revision"},
	} {
		for _, name := range group {
			if got := values[name]; got != expected {
				t.Fatalf("%s wire value = %d, want %d", name, got, expected)
			}
			expected++
		}
		expected = 1
	}
}

func TestQuarantineReasonHistoryMatrixIsExhaustive(t *testing.T) {
	allowed := map[QuarantineReason]map[FilePhase]bool{
		QuarantineAnchorMissing:         phases(FileWitnessed, FilePublishing, FilePublishBlocked, FilePublished),
		QuarantineAnchorUnsafe:          phases(FileReserved, FileWitnessed, FilePublishing, FilePublishBlocked, FilePublished, FileRetiring),
		QuarantineStageMissing:          phases(FileWitnessed, FilePublishing, FilePublishBlocked),
		QuarantineStageMismatch:         phases(FileReserved, FileWitnessed, FilePublishing, FilePublishBlocked, FilePublished, FileRetiring),
		QuarantineStageUnsafe:           phases(FileReserved, FileWitnessed, FilePublishing, FilePublishBlocked, FilePublished, FileRetiring),
		QuarantineFinalMismatch:         phases(FilePublished),
		QuarantineFinalUnsafe:           phases(FileReserved, FileWitnessed, FilePublishing, FilePublishBlocked, FilePublished),
		QuarantinePartialObjectCreation: phases(FileReserved, FileRetiring),
		QuarantinePublicationHistory:    phases(FileReserved, FileWitnessed, FilePublishing, FilePublishBlocked),
		QuarantineMetadataMismatch:      phases(FilePublishing, FilePublished),
		QuarantineUpdateTemporary:       phases(FileReserved, FileWitnessed, FilePublishing, FilePublishBlocked, FilePublished, FileRetiring),
		QuarantineOutputObjectDuplicate: phases(FileReserved, FileWitnessed, FilePublishing, FilePublishBlocked, FilePublished, FileRetiring),
	}
	for reason := QuarantineReason(0); reason <= QuarantineOutputObjectDuplicate+1; reason++ {
		for phase := FilePhase(0); phase <= FileQuarantined+1; phase++ {
			if got, want := validQuarantineHistory(phase, reason), allowed[reason][phase]; got != want {
				t.Fatalf("quarantine history phase=%d reason=%d = %v, want %v", phase, reason, got, want)
			}
		}
	}
	corrupt := testFileRecord(t, FileQuarantined)
	corrupt.phaseBeforeQuarantine = FilePublished
	corrupt.quarantineReason = QuarantineStageMissing
	if corrupt.valid() {
		t.Fatal("persisted quarantine accepted a reason impossible for its durable history")
	}
}

func TestPrepareUnsafeNamespaceQuarantineRequiresExactBoundAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		phase  FilePhase
		reason QuarantineReason
	}{
		{name: "partial creation", phase: FileReserved, reason: QuarantinePartialObjectCreation},
		{name: "publication cut", phase: FilePublishing, reason: QuarantinePublicationHistory},
		{name: "published final", phase: FilePublished, reason: QuarantineFinalUnsafe},
		{name: "retiring stage", phase: FileRetiring, reason: QuarantineStageUnsafe},
	} {
		t.Run(test.name, func(t *testing.T) {
			bound := testBoundFileRecord(t, test.phase)
			quarantined, err := PrepareUnsafeNamespaceQuarantine(bound, test.reason)
			if err != nil || quarantined.Record().Phase() != FileQuarantined ||
				quarantined.Record().PhaseBeforeQuarantine() != test.phase ||
				quarantined.Record().QuarantineReason() != test.reason ||
				quarantined.Record().StateGeneration() != bound.Record().StateGeneration()+1 {
				t.Fatalf("unsafe quarantine = %+v, %v", quarantined.Record(), err)
			}
		})
	}

	if _, err := PrepareUnsafeNamespaceQuarantine(
		BoundFileRecord{}, QuarantineStageUnsafe,
	); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("unbound quarantine error = %v", err)
	}
	if _, err := PrepareUnsafeNamespaceQuarantine(
		testBoundFileRecord(t, FilePublished), QuarantineMetadataMismatch,
	); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("non-unsafe quarantine reason error = %v", err)
	}
}

func phases(values ...FilePhase) map[FilePhase]bool {
	result := make(map[FilePhase]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func TestSessionNamespaceAuthoritySupportsSelectionIndependentLifecycleRecovery(t *testing.T) {
	header := testHeader(t)
	control := testControl(t)
	segments := SessionDirectorySegments(header.ResumeIntent(), header.SessionID())
	authority, err := BindSessionNamespaceAuthority(control, header, segments[1], segments[2])
	if err != nil || authority.Control() != control || authority.Header() != header ||
		authority.IntentDirectory() != segments[1] || authority.SessionDirectory() != segments[2] {
		t.Fatalf("namespace authority = %+v, %v", authority, err)
	}
	discarding, err := authority.WithLifecycle(SessionDiscarding)
	if err != nil || discarding.Header().Lifecycle() != SessionDiscarding ||
		discarding.Header().StateGeneration() != header.StateGeneration()+1 {
		t.Fatalf("discarding authority = %+v, %v", discarding, err)
	}
	if _, err := authority.WithLifecycle(SessionPaused); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("illegal namespace lifecycle error = %v", err)
	}

	selectionAuthority := testSessionAuthority(t, SessionActive)
	if !selectionAuthority.NamespaceAuthority().valid() || selectionAuthority.Control() != control ||
		selectionAuthority.Selection().Identity().IsZero() ||
		selectionAuthority.IntentDirectory() != segments[1] || selectionAuthority.SessionDirectory() != segments[2] {
		t.Fatalf("selection authority did not extend namespace authority: %+v", selectionAuthority)
	}

	wrongControl, err := NewControl(ControlSpec{
		Backend: control.Backend(), OutputRoot: testRootBindingFor(t, control.Certification(), 0xee),
		Certification: control.Certification(), Durability: control.Durability(), Generation: control.Generation(),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []struct {
		control Control
		intent  string
		session string
	}{
		{control: wrongControl, intent: segments[1], session: segments[2]},
		{control: control, intent: ResumeNamespaceName(identity32[transfer.ResumeIntent](0xee)), session: segments[2]},
		{control: control, intent: segments[1], session: "not-a-session"},
	} {
		if _, err := BindSessionNamespaceAuthority(invalid.control, header, invalid.intent, invalid.session); !errors.Is(err, ErrInvalidState) {
			t.Fatalf("invalid namespace binding error = %v", err)
		}
	}
}

func TestSessionLifecycleTransitionsAreExhaustive(t *testing.T) {
	legal := map[[2]SessionLifecycle]bool{
		{SessionActive, SessionPausing}: true, {SessionActive, SessionCompleting}: true,
		{SessionActive, SessionDiscarding}: true, {SessionPausing, SessionPaused}: true,
		{SessionPausing, SessionPausedNeedsAttention}: true,
		{SessionPaused, SessionActive}:                true, {SessionPaused, SessionDiscarding}: true,
		{SessionPausedNeedsAttention, SessionActive}: true, {SessionPausedNeedsAttention, SessionDiscarding}: true,
		{SessionCompleting, SessionPausedNeedsAttention}: true,
	}
	for from := SessionLifecycle(0); from <= SessionDiscarding+1; from++ {
		for to := SessionLifecycle(0); to <= SessionDiscarding+1; to++ {
			if got := CanTransitionSession(from, to); got != legal[[2]SessionLifecycle{from, to}] {
				t.Fatalf("transition %v -> %v = %v", from, to, got)
			}
		}
	}
	authority := testSessionAuthority(t, SessionActive)
	header := authority.Header()
	paused, err := authority.WithLifecycle(SessionPausing)
	if err != nil || paused.Header().Lifecycle() != SessionPausing ||
		paused.Header().StateGeneration() != header.StateGeneration()+1 {
		t.Fatalf("pausing header = %+v, %v", paused, err)
	}
	if _, err := authority.WithLifecycle(SessionPaused); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("illegal header transition error = %v", err)
	}
	authority.namespace.header.stateGeneration = math.MaxUint64
	if _, err := authority.WithLifecycle(SessionPausing); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("overflow transition error = %v", err)
	}
	for lifecycle := SessionLifecycle(0); lifecycle <= SessionDiscarding+1; lifecycle++ {
		if lifecycle.Valid() && lifecycle.String() == "invalid" || !lifecycle.Valid() && lifecycle.String() != "invalid" {
			t.Fatalf("lifecycle string/valid mismatch for %d", lifecycle)
		}
	}
}

func TestFilePhaseTransitionsAreExhaustive(t *testing.T) {
	legal := map[[2]FilePhase]bool{
		{FileReserved, FileWitnessed}: true, {FileReserved, FileRetiring}: true, {FileReserved, FileQuarantined}: true,
		{FileWitnessed, FilePublishing}: true, {FileWitnessed, FileRetiring}: true, {FileWitnessed, FileQuarantined}: true,
		{FilePublishing, FilePublished}: true, {FilePublishing, FilePublishBlocked}: true,
		{FilePublishing, FileRetiring}: true, {FilePublishing, FileQuarantined}: true,
		{FilePublishBlocked, FilePublishing}: true, {FilePublishBlocked, FileRetiring}: true, {FilePublishBlocked, FileQuarantined}: true,
		{FilePublished, FileRetiring}: true, {FilePublished, FileQuarantined}: true,
		{FileRetiring, FileQuarantined}: true,
	}
	for from := FilePhase(0); from <= FileQuarantined+1; from++ {
		for to := FilePhase(0); to <= FileQuarantined+1; to++ {
			if got := CanTransitionFile(from, to); got != legal[[2]FilePhase{from, to}] {
				t.Fatalf("transition %v -> %v = %v", from, to, got)
			}
		}
	}
	for phase := FilePhase(0); phase <= FileQuarantined+1; phase++ {
		if phase.Valid() && phase.String() == "invalid" || !phase.Valid() && phase.String() != "invalid" {
			t.Fatalf("phase string/valid mismatch for %d", phase)
		}
	}
}

func TestCheckpointGenerationsAreIndependentAndMonotonic(t *testing.T) {
	authority := testResumableFile(t, FileWitnessed)
	record := authority.Bound().Record()
	nextRanges := testRanges(t, content.Range{Offset: 0, End: 10})
	next, err := authority.WithCheckpoint(record.CheckpointGeneration()+1, nextRanges)
	nextRecord := next.Bound().Record()
	if err != nil || nextRecord.CheckpointGeneration() != record.CheckpointGeneration()+1 ||
		nextRecord.StateGeneration() != record.StateGeneration()+1 || !nextRecord.Complete() {
		t.Fatalf("checkpoint = %+v, %v", next, err)
	}
	invalid := []struct {
		name       string
		generation uint64
		ranges     content.RangeSet
	}{
		{name: "skip generation", generation: record.CheckpointGeneration() + 2, ranges: nextRanges},
		{name: "same ranges", generation: record.CheckpointGeneration() + 1, ranges: record.DurableRanges()},
		{name: "empty", generation: record.CheckpointGeneration() + 1, ranges: testRanges(t)},
		{name: "shrunk", generation: record.CheckpointGeneration() + 1, ranges: testRanges(t, content.Range{Offset: 0, End: 4})},
		{name: "past end", generation: record.CheckpointGeneration() + 1, ranges: testRanges(t, content.Range{Offset: 0, End: 11})},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			if _, err := authority.WithCheckpoint(test.generation, test.ranges); !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("checkpoint error = %v", err)
			}
		})
	}
	publishing, err := next.Bound().transition(FileTransition{Next: FilePublishing})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BindResumableFile(publishing, next.Descriptor()); err != nil {
		t.Fatal(err)
	}
	invalidPublishing := ResumableFileAuthority{bound: publishing, descriptor: next.Descriptor()}
	if _, err := invalidPublishing.WithCheckpoint(3, nextRanges); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("publishing checkpoint error = %v", err)
	}
}

func TestFileAuthorityConstructionPublicationAndRetirement(t *testing.T) {
	selection := testSelection(t, 10)
	session := testSessionAuthorityForSelection(t, selection, SessionActive)
	descriptor := testDescriptor(t, session, 10)
	created, err := NewFileRecord(FileRecordSpec{
		Session: session, Descriptor: descriptor, CanonicalLocator: "folder/file.bin",
		OutputObject: identity32[OutputObjectID](9),
	})
	if err != nil {
		t.Fatal(err)
	}
	record := created.Bound().Record()
	if record.Phase() != FileReserved || record.SessionID() != session.Header().SessionID() ||
		record.ShareInstance() != descriptor.ShareInstance() || record.FileID() != descriptor.FileID() ||
		record.Revision() != descriptor.FileRevision() || record.CanonicalLocator() != "folder/file.bin" ||
		record.OutputObject() != identity32[OutputObjectID](9) || record.ExactSize() != 10 ||
		record.ChunkSize() != descriptor.Geometry().ChunkSize() ||
		record.ExpectedMetadata().ModifiedTime != descriptor.ModifiedTime() || record.QuarantineReason() != 0 {
		t.Fatalf("created record = %+v", record)
	}
	if record.LocatorDigest() != DigestCanonicalLocator(record.CanonicalLocator()) {
		t.Fatal("locator digest is not bound to canonical locator")
	}
	transferLocator, err := transfer.NewPathOutputLocator(record.CanonicalLocator())
	if err != nil || record.LocatorDigest().OutputLocatorDigest() != transferLocator.Digest() {
		t.Fatal("state and transfer locator digest domains diverged")
	}
	if !DigestCanonicalLocator("folder/../file.bin").IsZero() {
		t.Fatal("noncanonical locator produced an index digest")
	}
	changedGeometry, err := content.NewFileGeometry(descriptor.ExactSize(), catalog.MinChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	changedDescriptor, err := content.NewFileRevisionDescriptor(
		descriptor.ShareInstance(), descriptor.FileID(), descriptor.FileRevision(), changedGeometry,
		descriptor.ModifiedTime(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BindResumableFile(created.Bound(), changedDescriptor); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("changed geometry binding error = %v", err)
	}

	witnessed, err := created.Bound().transition(FileTransition{Next: FileWitnessed})
	if err != nil {
		t.Fatal(err)
	}
	resumable, err := BindResumableFile(witnessed, descriptor)
	if err != nil {
		t.Fatal(err)
	}
	complete, err := resumable.WithCheckpoint(1, testRanges(t, content.Range{Offset: 0, End: 10}))
	if err != nil {
		t.Fatal(err)
	}
	publishing, err := PreparePublication(complete)
	if err != nil || publishing.Record().Phase() != FilePublishing {
		t.Fatalf("publishing record = %+v, %v", publishing, err)
	}
	published, err := publishing.transition(FileTransition{Next: FilePublished})
	if err != nil {
		t.Fatal(err)
	}
	retiring, err := PreparePublishedRetirement(published)
	if err != nil || retiring.Record().Phase() != FileRetiring ||
		retiring.Record().RetirementReason() != RetirementPublished {
		t.Fatalf("published retirement = %+v, %v", retiring, err)
	}
	isolated, err := PrepareIsolatedRetirement(witnessed)
	if err != nil || isolated.Record().RetirementReason() != RetirementIsolatedFailure {
		t.Fatalf("isolated retirement = %+v, %v", isolated, err)
	}
	invalidated, err := PrepareInvalidatedRevisionRetirement(publishing)
	if err != nil || invalidated.Record().RetirementReason() != RetirementInvalidatedRevision {
		t.Fatalf("invalidated revision retirement = %+v, %v", invalidated, err)
	}

	if _, err := PreparePublication(resumable); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("incomplete publication error = %v", err)
	}
	if _, err := PreparePublishedRetirement(witnessed); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("unpublished retirement error = %v", err)
	}
	if _, err := NewFileRecord(FileRecordSpec{
		Session: session, Descriptor: descriptor, CanonicalLocator: "folder/file.bin",
	}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("zero output object error = %v", err)
	}
}

func TestFileTransitionsAdvanceOnlyStateGeneration(t *testing.T) {
	witnessed := testResumableFile(t, FileWitnessed)
	complete, err := witnessed.WithCheckpoint(2, testRanges(t, content.Range{Offset: 0, End: 10}))
	if err != nil {
		t.Fatal(err)
	}
	completeRecord := complete.Bound().Record()
	publishing, err := complete.Bound().transition(FileTransition{Next: FilePublishing})
	publishingRecord := publishing.Record()
	if err != nil || publishingRecord.StateGeneration() != completeRecord.StateGeneration()+1 ||
		publishingRecord.CheckpointGeneration() != completeRecord.CheckpointGeneration() {
		t.Fatalf("publishing transition = %+v, %v", publishing, err)
	}
	published, err := publishing.transition(FileTransition{Next: FilePublished})
	if err != nil {
		t.Fatal(err)
	}
	retiring, err := published.transition(FileTransition{Next: FileRetiring, RetirementReason: RetirementPublished})
	if err != nil || retiring.Record().RetirementReason() != RetirementPublished {
		t.Fatalf("retiring = %+v, %v", retiring, err)
	}
	quarantined, err := retiring.transition(FileTransition{Next: FileQuarantined, QuarantineReason: QuarantineStageMismatch})
	if err != nil || quarantined.Record().PhaseBeforeQuarantine() != FileRetiring ||
		quarantined.Record().RetirementReason() != RetirementPublished {
		t.Fatalf("quarantined retirement = %+v, %v", quarantined, err)
	}
	if _, err := published.transition(FileTransition{Next: FileRetiring}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reasonless retirement error = %v", err)
	}
	if _, err := published.transition(FileTransition{Next: FileQuarantined}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("reasonless quarantine error = %v", err)
	}
	for _, invalid := range []struct {
		record BoundFileRecord
		reason RetirementReason
	}{
		{record: testBoundFileRecord(t, FilePublished), reason: RetirementIsolatedFailure},
		{record: testBoundFileRecord(t, FilePublished), reason: RetirementPreObjectCollision},
		{record: testBoundFileRecord(t, FileWitnessed), reason: RetirementPublished},
		{record: testBoundFileRecord(t, FileWitnessed), reason: RetirementPreObjectCollision},
		{record: testBoundFileRecord(t, FilePublishBlocked), reason: RetirementPublished},
	} {
		if _, err := invalid.record.transition(FileTransition{Next: FileRetiring, RetirementReason: invalid.reason}); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("unauthorized retirement %v/%v error = %v", invalid.record.Record().Phase(), invalid.reason, err)
		}
	}
}

func TestFileRecordValidationRejectsUnboundedOrContradictoryState(t *testing.T) {
	base := testFileRecord(t, FileWitnessed)
	claims := fileRecordClaims{
		sessionID: base.sessionID, shareInstance: base.shareInstance, fileID: base.fileID,
		revision: base.revision, canonicalLocator: base.canonicalLocator, outputObject: base.outputObject,
		exactSize: base.exactSize, chunkSize: base.chunkSize, stateGeneration: base.stateGeneration,
		checkpointGeneration: base.checkpointGeneration, durableRanges: base.durableRanges, phase: base.phase,
		expectedMetadata: base.expectedMetadata,
	}
	cases := []struct {
		name   string
		mutate func(*fileRecordClaims)
	}{
		{name: "zero state generation", mutate: func(s *fileRecordClaims) { s.stateGeneration = 0 }},
		{name: "checkpoint ahead of state", mutate: func(s *fileRecordClaims) { s.stateGeneration = s.checkpointGeneration }},
		{name: "noncanonical locator", mutate: func(s *fileRecordClaims) { s.canonicalLocator = "folder/../file" }},
		{name: "zero object", mutate: func(s *fileRecordClaims) { s.outputObject = OutputObjectID{} }},
		{name: "reserved ranges", mutate: func(s *fileRecordClaims) { s.phase = FileReserved }},
		{name: "publishing incomplete", mutate: func(s *fileRecordClaims) { s.phase = FilePublishing }},
		{name: "quarantine without reasons", mutate: func(s *fileRecordClaims) { s.phase = FileQuarantined }},
		{name: "reason outside quarantine", mutate: func(s *fileRecordClaims) { s.quarantineReason = QuarantineStageMismatch }},
		{name: "retiring without reason", mutate: func(s *fileRecordClaims) { s.phase = FileRetiring }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			candidate := claims
			test.mutate(&candidate)
			if _, err := newFileRecordFromClaims(candidate); !errors.Is(err, ErrInvalidState) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	atLimit := make([]content.Range, MaxDurableRangesPerFile)
	for index := range atLimit {
		atLimit[index] = content.Range{Offset: uint64(index * 2), End: uint64(index*2 + 1)}
	}
	limitSet, err := content.NewRangeSet(atLimit)
	if err != nil {
		t.Fatal(err)
	}
	boundary := claims
	boundary.exactSize = uint64(len(atLimit) * 2)
	boundary.durableRanges = limitSet
	if _, err := newFileRecordFromClaims(boundary); err != nil {
		t.Fatalf("exact range limit rejected: %v", err)
	}
	tooMany := append(atLimit, content.Range{Offset: uint64(len(atLimit) * 2), End: uint64(len(atLimit)*2 + 1)})
	large, err := content.NewRangeSet(tooMany)
	if err != nil {
		t.Fatal(err)
	}
	boundary.exactSize = uint64(len(tooMany) * 2)
	boundary.durableRanges = large
	if _, err := newFileRecordFromClaims(boundary); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("range limit error = %v", err)
	}
	if reflect.DeepEqual(base.DurableRanges().Ranges(), []content.Range{}) {
		t.Fatal("test fixture unexpectedly empty")
	}
}
