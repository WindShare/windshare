package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sort"

	"github.com/windshare/windshare/core/catalog"
)

const ResumeIntentBytes = sha256.Size

type ResumeIntent [ResumeIntentBytes]byte

func ResumeIntentFromBytes(raw []byte) (ResumeIntent, error) {
	if len(raw) != ResumeIntentBytes {
		return ResumeIntent{}, ErrInvalidOutputSelection
	}
	var intent ResumeIntent
	copy(intent[:], raw)
	if intent.IsZero() {
		return ResumeIntent{}, ErrInvalidOutputSelection
	}
	return intent, nil
}

func (intent ResumeIntent) Bytes() []byte { return append([]byte(nil), intent[:]...) }
func (intent ResumeIntent) IsZero() bool  { return intent == ResumeIntent{} }

// CanonicalSelectionRequest freezes only caller intent. It deliberately cannot
// name a resume namespace until terminal discovery contributes the selected plan.
type CanonicalSelectionRequest struct {
	share   catalog.ShareInstance
	root    catalog.DirectoryID
	encoded []byte
}

func NewCanonicalSelectionRequest(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
) (CanonicalSelectionRequest, error) {
	if share.IsZero() || root.IsZero() || !rules.validSnapshot() {
		return CanonicalSelectionRequest{}, ErrInvalidSelectionRules
	}
	encoded := make([]byte, 0, 256)
	encoded = appendCanonicalField(encoded, share.Bytes())
	encoded = appendCanonicalField(encoded, root.Bytes())
	encoded = appendCanonicalField(encoded, []byte{byte(rules.mode)})
	defaultSelected := byte(0)
	if rules.defaultSelected {
		defaultSelected = 1
	}
	encoded = appendCanonicalField(encoded, []byte{defaultSelected})
	switch rules.mode {
	case SelectionByNodeID:
		type nodeRule struct {
			kind     byte
			identity []byte
			selected bool
		}
		nodes := make([]nodeRule, 0, len(rules.directories)+len(rules.files))
		for identity, selected := range rules.directories {
			nodes = append(nodes, nodeRule{kind: 1, identity: identity.Bytes(), selected: selected})
		}
		for identity, selected := range rules.files {
			nodes = append(nodes, nodeRule{kind: 2, identity: identity.Bytes(), selected: selected})
		}
		sort.Slice(nodes, func(left, right int) bool {
			if comparison := bytes.Compare(nodes[left].identity, nodes[right].identity); comparison != 0 {
				return comparison < 0
			}
			return nodes[left].kind < nodes[right].kind
		})
		encoded = appendCanonicalCount(encoded, len(nodes))
		for _, node := range nodes {
			selected := byte(0)
			if node.selected {
				selected = 1
			}
			encoded = appendCanonicalField(encoded, []byte{node.kind})
			encoded = appendCanonicalField(encoded, node.identity)
			encoded = appendCanonicalField(encoded, []byte{selected})
		}
	case SelectionByCatalogPath:
		paths := slices.Clone(rules.pathTargets)
		sort.Strings(paths)
		encoded = appendCanonicalCount(encoded, len(paths))
		for _, path := range paths {
			encoded = appendCanonicalField(encoded, []byte(path))
		}
	default:
		return CanonicalSelectionRequest{}, ErrInvalidSelectionRules
	}
	return CanonicalSelectionRequest{share: share, root: root, encoded: encoded}, nil
}

func (request CanonicalSelectionRequest) Bytes() []byte { return slices.Clone(request.encoded) }

// CanonicalSelectionV1 combines normalized request semantics and the terminal
// authenticated plan. Neither component alone is a resume namespace.
type CanonicalSelectionV1 struct {
	request      []byte
	root         catalog.DirectoryGeneration
	plan         outputSelectionPlan
	intent       ResumeIntent
	planIdentity SelectionIdentity
}

func NewCanonicalSelectionV1(
	request CanonicalSelectionRequest,
	plan OutputSelection,
) (CanonicalSelectionV1, error) {
	if request.share.IsZero() || request.root.IsZero() || len(request.encoded) == 0 ||
		plan.ShareInstance() != request.share || plan.SyntheticRoot() != request.root || plan.Identity().IsZero() {
		return CanonicalSelectionV1{}, ErrInvalidOutputSelection
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("windshare/output-resume-intent/v3"))
	if err := writeCanonicalSelectionV1(hash, request.encoded, plan.RootGeneration(), plan.plan); err != nil {
		return CanonicalSelectionV1{}, err
	}
	var intent ResumeIntent
	copy(intent[:], hash.Sum(nil))
	return CanonicalSelectionV1{
		request: slices.Clone(request.encoded), root: plan.RootGeneration(), plan: plan.plan,
		intent: intent, planIdentity: plan.Identity(),
	}, nil
}

// Bytes materializes the canonical artifact only for callers that explicitly
// need to export it. Resume admission hashes the frozen plan directly so a wide
// selection never acquires a second in-memory representation during transfer.
func (selection CanonicalSelectionV1) Bytes() []byte {
	if len(selection.request) == 0 || selection.root.IsZero() || selection.plan == nil {
		return nil
	}
	var encoded bytes.Buffer
	if err := writeCanonicalSelectionV1(&encoded, selection.request, selection.root, selection.plan); err != nil {
		panic(err)
	}
	return encoded.Bytes()
}
func (selection CanonicalSelectionV1) ResumeIntent() ResumeIntent { return selection.intent }

func (selection CanonicalSelectionV1) BindPlan(plan OutputSelection) (OutputSelection, error) {
	if selection.intent.IsZero() || selection.planIdentity.IsZero() || plan.Identity() != selection.planIdentity {
		return OutputSelection{}, ErrInvalidOutputSelection
	}
	plan.resumeIntent = selection.intent
	plan.canonical = selection
	return plan, nil
}

func writeCanonicalSelectionV1(
	destination selectionHash,
	request []byte,
	root catalog.DirectoryGeneration,
	plan outputSelectionPlan,
) error {
	_, _ = destination.Write(request)
	writeSelectionBytes(destination, root.Bytes())
	writeCanonicalCount(destination, plan.DirectoryCount())
	if err := plan.VisitRecords(func(record selectionPlanRecord) error {
		if record.kind != selectionPlanDirectoryKind {
			return nil
		}
		writeSelectionBytes(destination, []byte(record.path))
		writeSelectionBytes(destination, record.directory.directory.Bytes())
		writeSelectionBytes(destination, record.directory.generation.Bytes())
		writeCanonicalModifiedTime(destination, record.directory.modified)
		return nil
	}); err != nil {
		return err
	}
	writeCanonicalCount(destination, plan.FileCount())
	if err := plan.VisitRecords(func(record selectionPlanRecord) error {
		if record.kind != selectionPlanFileKind {
			return nil
		}
		writeSelectionBytes(destination, []byte(record.path))
		writeSelectionBytes(destination, record.file.file.Bytes())
		writeSelectionBytes(destination, record.file.parentDirectory.Bytes())
		writeSelectionBytes(destination, record.file.parentGeneration.Bytes())
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], record.file.expectedSize)
		writeSelectionBytes(destination, size[:])
		writeCanonicalModifiedTime(destination, record.file.modified)
		return nil
	}); err != nil {
		return err
	}
	writeSelectionBytes(destination, []byte("native-tree"))
	writeSelectionBytes(destination, []byte("no-replace"))
	return nil
}

func writeCanonicalModifiedTime(destination selectionHash, modified catalog.ModifiedTime) {
	present := byte(0)
	if modified.Present() {
		present = 1
	}
	writeSelectionBytes(destination, []byte{present})
	var seconds [8]byte
	binary.BigEndian.PutUint64(seconds[:], uint64(modified.Seconds()))
	writeSelectionBytes(destination, seconds[:])
	var nanoseconds [4]byte
	binary.BigEndian.PutUint32(nanoseconds[:], modified.Nanoseconds())
	writeSelectionBytes(destination, nanoseconds[:])
	writeSelectionBytes(destination, []byte{byte(modified.Precision())})
}

func appendCanonicalCount(destination []byte, count int) []byte {
	return appendCanonicalUint64Count(destination, uint64(count))
}

func appendCanonicalUint64Count(destination []byte, count uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], count)
	return append(destination, encoded[:]...)
}

func writeCanonicalCount(destination selectionHash, count uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], count)
	_, _ = destination.Write(encoded[:])
}

func appendCanonicalField(destination, value []byte) []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	destination = append(destination, length[:]...)
	return append(destination, value...)
}
