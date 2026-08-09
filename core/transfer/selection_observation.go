package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"slices"
	"sort"

	"github.com/windshare/windshare/core/catalog"
)

const (
	SelectionObservationV1Bytes = sha256.Size
	SelectionSpecV1             = uint8(1)
	SelectionSpecDigestBytes    = sha256.Size
	selectionSpecDomain         = "windshare/selection-spec/v1"
)

var ErrInvalidSelectionObservation = errors.New("transfer selection observation is invalid")

// SelectionObservationV1 is a non-durable audit digest of one terminal catalog
// selection. ReceiveIntentDigest, not an observation of one run, owns durable
// materialization identity.
type SelectionObservationV1 [SelectionObservationV1Bytes]byte

func SelectionObservationV1FromBytes(raw []byte) (SelectionObservationV1, error) {
	if len(raw) != SelectionObservationV1Bytes {
		return SelectionObservationV1{}, ErrInvalidSelectionObservation
	}
	var observation SelectionObservationV1
	copy(observation[:], raw)
	if observation.IsZero() {
		return SelectionObservationV1{}, ErrInvalidSelectionObservation
	}
	return observation, nil
}

func (observation SelectionObservationV1) Bytes() []byte {
	return append([]byte(nil), observation[:]...)
}
func (observation SelectionObservationV1) IsZero() bool {
	return observation == SelectionObservationV1{}
}

type SelectionSpecDigest [SelectionSpecDigestBytes]byte

func (digest SelectionSpecDigest) Bytes() []byte { return append([]byte(nil), digest[:]...) }
func (digest SelectionSpecDigest) IsZero() bool  { return digest == SelectionSpecDigest{} }

// SelectionSpec freezes caller selection semantics without projection epoch,
// discovery evidence, catalog generations, or runtime identities.
type SelectionSpec struct {
	share   catalog.ShareInstance
	root    catalog.DirectoryID
	rules   SelectionRules
	encoded []byte
	digest  SelectionSpecDigest
}

func NewSelectionSpec(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
) (SelectionSpec, error) {
	if share.IsZero() || root.IsZero() || !rules.validSnapshot() {
		return SelectionSpec{}, ErrInvalidSelectionRules
	}
	encoded := make([]byte, 0, 256)
	encoded = append(encoded, selectionSpecDomain...)
	encoded = append(encoded, 0, SelectionSpecV1)
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
		return SelectionSpec{}, ErrInvalidSelectionRules
	}
	digest := sha256.Sum256(encoded)
	return SelectionSpec{
		share: share, root: root, rules: rules, encoded: encoded,
		digest: SelectionSpecDigest(digest),
	}, nil
}

func (selection SelectionSpec) ShareInstance() catalog.ShareInstance { return selection.share }
func (selection SelectionSpec) SyntheticRoot() catalog.DirectoryID   { return selection.root }
func (selection SelectionSpec) SelectionRules() SelectionRules       { return selection.rules }
func (selection SelectionSpec) CanonicalBytes() []byte               { return slices.Clone(selection.encoded) }
func (selection SelectionSpec) Bytes() []byte                        { return selection.CanonicalBytes() }
func (selection SelectionSpec) Digest() SelectionSpecDigest          { return selection.digest }
func (selection SelectionSpec) IsZero() bool {
	if selection.share.IsZero() || selection.root.IsZero() || !selection.rules.validSnapshot() ||
		selection.digest.IsZero() {
		return true
	}
	rebuilt, err := NewSelectionSpec(selection.share, selection.root, selection.rules)
	return err != nil || !bytes.Equal(selection.encoded, rebuilt.encoded) || selection.digest != rebuilt.digest
}

func appendCanonicalCount(destination []byte, count int) []byte {
	return appendCanonicalUint64Count(destination, uint64(count))
}

func appendCanonicalUint64Count(destination []byte, count uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], count)
	return append(destination, encoded[:]...)
}

func appendCanonicalField(destination, value []byte) []byte {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	destination = append(destination, length[:]...)
	return append(destination, value...)
}
