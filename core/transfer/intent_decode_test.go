package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestDecodeReceiveIntentRoundTripsFreshProcessAuthority(t *testing.T) {
	fixtures := decodeReceiveIntentFixtures(t)
	for name, intent := range fixtures {
		t.Run(name, func(t *testing.T) {
			stored := intent.CanonicalBytes()
			decoded, err := DecodeReceiveIntent(bytes.Clone(stored))
			derived := ReceiveIntentDigest(sha256.Sum256(stored))
			if err != nil || !decoded.EqualCanonical(intent) ||
				!bytes.Equal(decoded.CanonicalBytes(), stored) || decoded.Digest() != derived ||
				decoded.OperationID() != intent.OperationID() ||
				decoded.BindingDigest() != intent.BindingDigest() {
				t.Fatalf("decoded operation=%x digest=%x binding=%x error=%v", decoded.OperationID(), decoded.Digest(), decoded.BindingDigest(), err)
			}
			expectedReservation, expectedDirect := intent.MaterializationPlan().DestinationReservation()
			decodedReservation, decodedDirect := decoded.MaterializationPlan().DestinationReservation()
			if decodedDirect != expectedDirect || expectedDirect &&
				(!bytes.Equal(decodedReservation.CanonicalBytes(), expectedReservation.CanonicalBytes()) ||
					decodedReservation.ID() != expectedReservation.ID() ||
					decodedReservation.OperationID() != expectedReservation.OperationID()) {
				t.Fatal("fresh-process decode did not reconstruct the exact destination reservation")
			}
			stored[0] ^= 0xff
			if decoded.IsZero() || bytes.Equal(decoded.CanonicalBytes(), stored) {
				t.Fatal("decoded authority retained caller-owned storage")
			}
		})
	}
}

func TestDecodeReceiveIntentRejectsMalformedNonCanonicalAndCrossBoundImages(t *testing.T) {
	fixtures := decodeReceiveIntentFixtures(t)
	intent := fixtures["direct-tree-node-selection"]
	encoded := intent.CanonicalBytes()
	legacyVersion := bytes.Clone(encoded)
	legacyVersion[len(receiveIntentDomain)-1] = '1'
	legacyVersion[len(receiveIntentDomain)+1] = 1
	zeroShare := bytes.Clone(encoded)
	shareOffset := bytes.Index(zeroShare, intent.ShareInstance().Bytes())
	if shareOffset < 0 {
		t.Fatal("share identity is absent from receive intent")
	}
	clear(zeroShare[shareOffset : shareOffset+catalog.IdentityBytes])
	for name, value := range map[string][]byte{
		"empty":          nil,
		"v1-version":     legacyVersion,
		"zero-share":     zeroShare,
		"trailing":       append(bytes.Clone(encoded), 0),
		"foreign-prefix": append([]byte("foreign"), encoded...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeReceiveIntent(value); !errors.Is(err, ErrInvalidReceiveIntent) {
				t.Fatalf("decode error=%v", err)
			}
		})
	}
	for offset := range len(encoded) {
		if _, err := DecodeReceiveIntent(encoded[:offset]); !errors.Is(err, ErrInvalidReceiveIntent) {
			t.Fatalf("truncation %d error=%v", offset, err)
		}
	}

	pathIntent := fixtures["portable-path-selection"]
	selection := pathIntent.SelectionSpec().CanonicalBytes()
	firstPath, secondPath := selectionPathValueOffsets(t, selection)
	selection[firstPath], selection[secondPath] = selection[secondPath], selection[firstPath]
	nonCanonicalSelection := canonicalIntentFromFields(
		selection, pathIntent.ArtifactSpec().CanonicalBytes(), pathIntent.MaterializationPlan().CanonicalBytes(),
	)
	if _, err := DecodeReceiveIntent(nonCanonicalSelection); !errors.Is(err, ErrInvalidReceiveIntent) {
		t.Fatalf("unsorted selection error=%v", err)
	}

	foreignArtifact := fixtures["direct-atomic-original"].ArtifactSpec()
	crossBound := canonicalIntentFromFields(
		intent.SelectionSpec().CanonicalBytes(), foreignArtifact.CanonicalBytes(),
		intent.MaterializationPlan().CanonicalBytes(),
	)
	if _, err := DecodeReceiveIntent(crossBound); !errors.Is(err, ErrInvalidReceiveIntent) {
		t.Fatalf("cross-bound artifact error=%v", err)
	}
}

func canonicalIntentFromFields(selection, artifact, plan []byte) []byte {
	encoded := append([]byte(receiveIntentDomain), 0, ReceiveIntentV2)
	encoded = appendCanonicalField(encoded, selection)
	encoded = appendCanonicalField(encoded, artifact)
	return appendCanonicalField(encoded, plan)
}

func selectionPathValueOffsets(t *testing.T, encoded []byte) (int, int) {
	t.Helper()
	offset := len(selectionSpecDomain) + 2
	for _, size := range []int{catalog.IdentityBytes, catalog.IdentityBytes, 1, 1} {
		if len(encoded)-offset < 8 || binary.BigEndian.Uint64(encoded[offset:offset+8]) != uint64(size) {
			t.Fatal("unexpected selection field framing")
		}
		offset += 8 + size
	}
	if len(encoded)-offset < 8 || binary.BigEndian.Uint64(encoded[offset:offset+8]) != 2 {
		t.Fatal("unexpected path rule count")
	}
	offset += 8
	if len(encoded)-offset < 9 || binary.BigEndian.Uint64(encoded[offset:offset+8]) != 1 {
		t.Fatal("unexpected first path frame")
	}
	first := offset + 8
	offset += 9
	if len(encoded)-offset < 9 || binary.BigEndian.Uint64(encoded[offset:offset+8]) != 1 {
		t.Fatal("unexpected second path frame")
	}
	return first, offset + 8
}

func decodeReceiveIntentFixtures(t *testing.T) map[string]ReceiveIntent {
	t.Helper()
	share := transferID[catalog.ShareInstance](81)
	root := transferID[catalog.DirectoryID](82)
	nodeRules, err := NewSelectionRules(true, []SelectionOverride{
		{DirectoryID: transferID[catalog.DirectoryID](83), Selected: true},
		{FileID: transferID[catalog.FileID](84), Selected: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	pathRules, err := NewPathSelectionRules([]string{"b", "a"})
	if err != nil {
		t.Fatal(err)
	}
	nodeSelection, err := NewSelectionSpec(share, root, nodeRules)
	if err != nil {
		t.Fatal(err)
	}
	pathSelection, err := NewSelectionSpec(share, root, pathRules)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := receivecontract.OperationIDFromBytes(bytes.Repeat([]byte{85}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	reservationID, err := receivecontract.DestinationReservationIDFromBytes(bytes.Repeat([]byte{86}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	authority, err := receivecontract.AuthorityRefFromBytes(bytes.Repeat([]byte{87}, receivecontract.AuthorityRefBytes))
	if err != nil {
		t.Fatal(err)
	}
	workspaceID, err := receivecontract.WorkspaceIDFromBytes(bytes.Repeat([]byte{88}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := receivecontract.RepositoryRefFromBytes(bytes.Repeat([]byte{89}, receivecontract.AuthorityRefBytes))
	if err != nil {
		t.Fatal(err)
	}
	portableID, err := receivecontract.PortablePlanIDFromBytes(bytes.Repeat([]byte{90}, receivecontract.StableIdentityBytes))
	if err != nil {
		t.Fatal(err)
	}

	catalogTree := receivecontract.NewCatalogRootDirectoryTree()
	rootReservation, err := receivecontract.NewNativeContainerRootReservation(
		operation, reservationID, catalogTree, authority,
	)
	if err != nil {
		t.Fatal(err)
	}
	directTree, err := receivecontract.NewDirectTreePlan(catalogTree, rootReservation)
	if err != nil {
		t.Fatal(err)
	}
	file := transferID[catalog.FileID](91)
	original, err := receivecontract.NewOriginalFileArtifact(file, "report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	atomicReservation, err := receivecontract.NewManagedAtomicReservation(
		operation, reservationID, original, authority, receivecontract.NameApplicationChosen,
		"report.txt", "report.txt", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	directAtomic, err := receivecontract.NewDirectAtomicPlan(original, atomicReservation)
	if err != nil {
		t.Fatal(err)
	}
	resultRoot := receivecontract.NewSyntheticSelectionResultRoot()
	archive, err := receivecontract.NewZipArchiveArtifact(resultRoot)
	if err != nil {
		t.Fatal(err)
	}
	workspaceBinding, err := receivecontract.NewWorkspaceBinding(operation, workspaceID, archive, repository)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := receivecontract.NewWorkspaceThenPublishPlan(archive, workspaceBinding)
	if err != nil {
		t.Fatal(err)
	}
	portableBinding, err := receivecontract.NewPortableBinding(operation, portableID, original)
	if err != nil {
		t.Fatal(err)
	}
	portable, err := receivecontract.NewPortableHandoffPlan(original, portableBinding)
	if err != nil {
		t.Fatal(err)
	}

	makeIntent := func(selection SelectionSpec, artifact receivecontract.ArtifactSpec, plan receivecontract.MaterializationPlan) ReceiveIntent {
		intent, intentErr := NewReceiveIntent(selection, artifact, plan)
		if intentErr != nil {
			t.Fatal(intentErr)
		}
		return intent
	}
	return map[string]ReceiveIntent{
		"direct-tree-node-selection": makeIntent(nodeSelection, catalogTree, directTree),
		"direct-atomic-original":     makeIntent(nodeSelection, original, directAtomic),
		"workspace-zip":              makeIntent(nodeSelection, archive, workspace),
		"portable-path-selection":    makeIntent(pathSelection, original, portable),
	}
}
