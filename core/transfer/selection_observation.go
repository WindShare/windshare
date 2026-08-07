package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sort"

	"github.com/windshare/windshare/core/catalog"
)

const (
	SelectionObservationV1Bytes  = sha256.Size
	selectionObservationV1Domain = "windshare/selection-observation/v1"
)

// SelectionObservationV1 is a non-durable audit digest of one terminal catalog
// selection. It must never identify checkpoint records, output namespaces,
// or recovery admission; TransferIntentDigest owns all durable identity.
type SelectionObservationV1 [SelectionObservationV1Bytes]byte

func SelectionObservationV1FromBytes(raw []byte) (SelectionObservationV1, error) {
	if len(raw) != SelectionObservationV1Bytes {
		return SelectionObservationV1{}, ErrInvalidOutputSelection
	}
	var observation SelectionObservationV1
	copy(observation[:], raw)
	if observation.IsZero() {
		return SelectionObservationV1{}, ErrInvalidOutputSelection
	}
	return observation, nil
}

func (observation SelectionObservationV1) Bytes() []byte {
	return append([]byte(nil), observation[:]...)
}
func (observation SelectionObservationV1) IsZero() bool {
	return observation == SelectionObservationV1{}
}

// CanonicalSelectionRequest freezes only caller selection semantics. It cannot
// identify durable state because output target, backend, and format belong to
// TransferIntent, while terminal discovery remains an observation of one run.
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

// TerminalSelectionObservationV1 combines normalized request semantics and the
// authenticated selection observed at terminal discovery. It exists for diagnostics
// and audit only; incremental output and recovery must not consume it.
type TerminalSelectionObservationV1 struct {
	request           []byte
	root              catalog.DirectoryGeneration
	directories       []OutputSelectionDirectory
	files             []OutputSelectionFile
	observation       SelectionObservationV1
	selectionIdentity SelectionIdentity
}

func NewTerminalSelectionObservationV1(
	request CanonicalSelectionRequest,
	selection OutputSelection,
) (TerminalSelectionObservationV1, error) {
	if request.share.IsZero() || request.root.IsZero() || len(request.encoded) == 0 ||
		selection.ShareInstance() != request.share || selection.SyntheticRoot() != request.root ||
		selection.Identity().IsZero() {
		return TerminalSelectionObservationV1{}, ErrInvalidOutputSelection
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(selectionObservationV1Domain))
	writeTerminalSelectionObservationV1(
		hash, request.encoded, selection.RootGeneration(), selection.directories, selection.files,
	)
	var observation SelectionObservationV1
	copy(observation[:], hash.Sum(nil))
	return TerminalSelectionObservationV1{
		request: slices.Clone(request.encoded), root: selection.RootGeneration(),
		directories: selection.directories, files: selection.files,
		observation: observation, selectionIdentity: selection.Identity(),
	}, nil
}

// Bytes materializes the diagnostic artifact only for explicit audit consumers.
func (selection TerminalSelectionObservationV1) Bytes() []byte {
	if len(selection.request) == 0 || selection.root.IsZero() || selection.selectionIdentity.IsZero() {
		return nil
	}
	var encoded bytes.Buffer
	writeTerminalSelectionObservationV1(
		&encoded, selection.request, selection.root, selection.directories, selection.files,
	)
	return encoded.Bytes()
}
func (selection TerminalSelectionObservationV1) Observation() SelectionObservationV1 {
	return selection.observation
}

// BindPlan attaches the observation to the exact in-memory selection it describes.
// It does not grant output or recovery authority.
func (selection TerminalSelectionObservationV1) BindPlan(plan OutputSelection) (OutputSelection, error) {
	if selection.observation.IsZero() || selection.selectionIdentity.IsZero() ||
		plan.Identity() != selection.selectionIdentity {
		return OutputSelection{}, ErrInvalidOutputSelection
	}
	plan.selectionObservation = selection.observation
	plan.terminalObservation = selection
	return plan, nil
}

func writeTerminalSelectionObservationV1(
	destination selectionHash,
	request []byte,
	root catalog.DirectoryGeneration,
	directories []OutputSelectionDirectory,
	files []OutputSelectionFile,
) {
	_, _ = destination.Write(request)
	writeSelectionBytes(destination, root.Bytes())
	writeCanonicalCount(destination, uint64(len(directories)))
	for _, directory := range directories {
		writeSelectionBytes(destination, []byte(directory.Path))
		writeSelectionBytes(destination, directory.DirectoryID.Bytes())
		writeSelectionBytes(destination, directory.Generation.Bytes())
		writeCanonicalModifiedTime(destination, directory.ModifiedTime)
	}
	writeCanonicalCount(destination, uint64(len(files)))
	for _, file := range files {
		writeSelectionBytes(destination, []byte(file.Path))
		writeSelectionBytes(destination, file.FileID.Bytes())
		writeSelectionBytes(destination, file.ParentDirectoryID.Bytes())
		writeSelectionBytes(destination, file.ParentGeneration.Bytes())
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], file.ExpectedSize)
		writeSelectionBytes(destination, size[:])
		writeCanonicalModifiedTime(destination, file.ModifiedTime)
	}
	writeSelectionBytes(destination, []byte("native-tree"))
	writeSelectionBytes(destination, []byte("no-replace"))
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
