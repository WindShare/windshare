package transfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestReceiveIntentJSONRoundTripsFreshProcessAuthority(t *testing.T) {
	fixtures := receiveIntentJSONFixtures(t)
	for name, intent := range fixtures {
		t.Run(name, func(t *testing.T) {
			stored, err := json.Marshal(intent)
			if err != nil {
				t.Fatal(err)
			}
			var decoded ReceiveIntent
			if err := json.Unmarshal(stored, &decoded); err != nil {
				t.Fatalf("unmarshal %s: %v", name, err)
			}
			if !decoded.Equal(intent) ||
				decoded.Digest() != intent.Digest() ||
				decoded.OperationID() != intent.OperationID() ||
				decoded.BindingDigest() != intent.BindingDigest() {
				t.Fatalf("decoded operation=%x digest=%x binding=%x", decoded.OperationID(), decoded.Digest(), decoded.BindingDigest())
			}
			expectedReservation, expectedDirect := intent.MaterializationPlan().DestinationReservation()
			decodedReservation, decodedDirect := decoded.MaterializationPlan().DestinationReservation()
			if decodedDirect != expectedDirect || (expectedDirect &&
				(decodedReservation.Digest() != expectedReservation.Digest() ||
					decodedReservation.ID() != expectedReservation.ID() ||
					decodedReservation.OperationID() != expectedReservation.OperationID() ||
					decodedReservation.PhysicalName() != expectedReservation.PhysicalName())) {
				t.Fatal("fresh-process json decode did not reconstruct the exact destination reservation")
			}
		})
	}
}

func TestReceiveIntentJSONRejectsMalformedAndCrossBoundImages(t *testing.T) {
	fixtures := receiveIntentJSONFixtures(t)
	intent := fixtures["direct-tree-node-selection"]
	encoded, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string][]byte{
		"empty":          nil,
		"empty-object":   []byte(`{}`),
		"trailing":       append(bytes.Clone(encoded), ' ', '{', '}'),
		"unknown-field":  []byte(`{"selection":{},"artifact":{},"plan":{},"extra":1}`),
		"truncated-half": encoded[:len(encoded)/2],
	} {
		t.Run(name, func(t *testing.T) {
			var decoded ReceiveIntent
			if err := json.Unmarshal(value, &decoded); err == nil {
				t.Fatalf("accepted malformed json %s", name)
			}
		})
	}

	// Cross-bound artifact and plan must be rejected.
	other := fixtures["direct-tree-single-file"]
	var rawIntent, rawOther map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &rawIntent); err != nil {
		t.Fatal(err)
	}
	otherEncoded, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(otherEncoded, &rawOther); err != nil {
		t.Fatal(err)
	}
	rawIntent["artifact"] = rawOther["artifact"]
	crossBound, err := json.Marshal(rawIntent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ReceiveIntent
	if err := json.Unmarshal(crossBound, &decoded); !errors.Is(err, ErrInvalidReceiveIntent) {
		t.Fatalf("cross-bound artifact error=%v, want ErrInvalidReceiveIntent", err)
	}
}

func receiveIntentJSONFixtures(t *testing.T) map[string]ReceiveIntent {
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
	syntheticTree, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	rootReservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, syntheticTree, authority,
		receivecontract.DefaultResultRootName, receivecontract.DefaultResultRootName, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	directTree, err := receivecontract.NewDirectTreePlan(syntheticTree, rootReservation)
	if err != nil {
		t.Fatal(err)
	}
	file := transferID[catalog.FileID](91)
	singleFile, err := receivecontract.NewSingleFileDirectoryTree(file, "report.txt", "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	singleReservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, singleFile, authority, "report.txt", "report.txt", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	singlePlan, err := receivecontract.NewDirectTreePlan(singleFile, singleReservation)
	if err != nil {
		t.Fatal(err)
	}
	resultLayout, err := receivecontract.NewDirectorySelectionResultRoot(transferID[catalog.DirectoryID](92), "photos")
	if err != nil {
		t.Fatal(err)
	}
	resultArtifact, err := receivecontract.NewResultRootDirectoryTree(resultLayout)
	if err != nil {
		t.Fatal(err)
	}
	resultReservation, err := receivecontract.NewNativeNamedEntryReservation(
		operation, reservationID, resultArtifact, authority, "photos-selection", "photos-selection", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	resultPlan, err := receivecontract.NewDirectTreePlan(resultArtifact, resultReservation)
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
		"direct-tree-node-selection": makeIntent(nodeSelection, syntheticTree, directTree),
		"direct-tree-single-file":    makeIntent(nodeSelection, singleFile, singlePlan),
		"direct-tree-result-root":    makeIntent(nodeSelection, resultArtifact, resultPlan),
		"direct-tree-path-selection": makeIntent(pathSelection, syntheticTree, directTree),
	}
}
