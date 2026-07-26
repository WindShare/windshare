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
	encoded      []byte
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
	encoded := slices.Clone(request.encoded)
	encoded = appendCanonicalField(encoded, plan.RootGeneration().Bytes())
	directories := plan.Directories()
	encoded = appendCanonicalCount(encoded, len(directories))
	for _, directory := range directories {
		encoded = appendCanonicalField(encoded, []byte(directory.Path))
		encoded = appendCanonicalField(encoded, directory.DirectoryID.Bytes())
		encoded = appendCanonicalField(encoded, directory.Generation.Bytes())
		encoded = appendCanonicalModifiedTime(encoded, directory.ModifiedTime)
	}
	files := plan.Files()
	encoded = appendCanonicalCount(encoded, len(files))
	for _, file := range files {
		encoded = appendCanonicalField(encoded, []byte(file.Path))
		encoded = appendCanonicalField(encoded, file.FileID.Bytes())
		encoded = appendCanonicalField(encoded, file.ParentDirectoryID.Bytes())
		encoded = appendCanonicalField(encoded, file.ParentGeneration.Bytes())
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], file.ExpectedSize)
		encoded = appendCanonicalField(encoded, size[:])
		encoded = appendCanonicalModifiedTime(encoded, file.ModifiedTime)
	}
	encoded = appendCanonicalField(encoded, []byte("native-tree"))
	encoded = appendCanonicalField(encoded, []byte("no-replace"))
	hash := sha256.New()
	_, _ = hash.Write([]byte("windshare/output-resume-intent/v3"))
	_, _ = hash.Write(encoded)
	var intent ResumeIntent
	copy(intent[:], hash.Sum(nil))
	return CanonicalSelectionV1{encoded: encoded, intent: intent, planIdentity: plan.Identity()}, nil
}

func (selection CanonicalSelectionV1) Bytes() []byte              { return slices.Clone(selection.encoded) }
func (selection CanonicalSelectionV1) ResumeIntent() ResumeIntent { return selection.intent }

func (selection CanonicalSelectionV1) BindPlan(plan OutputSelection) (OutputSelection, error) {
	if selection.intent.IsZero() || selection.planIdentity.IsZero() || plan.Identity() != selection.planIdentity {
		return OutputSelection{}, ErrInvalidOutputSelection
	}
	plan.resumeIntent = selection.intent
	plan.canonical = selection
	return plan, nil
}

func appendCanonicalModifiedTime(destination []byte, modified catalog.ModifiedTime) []byte {
	present := byte(0)
	if modified.Present() {
		present = 1
	}
	destination = appendCanonicalField(destination, []byte{present})
	var seconds [8]byte
	binary.BigEndian.PutUint64(seconds[:], uint64(modified.Seconds()))
	destination = appendCanonicalField(destination, seconds[:])
	var nanoseconds [4]byte
	binary.BigEndian.PutUint32(nanoseconds[:], modified.Nanoseconds())
	destination = appendCanonicalField(destination, nanoseconds[:])
	return appendCanonicalField(destination, []byte{byte(modified.Precision())})
}

func appendCanonicalCount(destination []byte, count int) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(count))
	return append(destination, encoded[:]...)
}

func appendCanonicalField(destination, value []byte) []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	destination = append(destination, length[:]...)
	return append(destination, value...)
}
