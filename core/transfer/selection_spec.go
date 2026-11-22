package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"sort"

	"github.com/windshare/windshare/core/catalog"
)

const (
	SelectionSpecDigestBytes = sha256.Size
	selectionSpecDomain      = "windshare/selection-spec/v1"
)

type SelectionSpecDigest [SelectionSpecDigestBytes]byte

func (digest SelectionSpecDigest) Bytes() []byte { return append([]byte(nil), digest[:]...) }
func (digest SelectionSpecDigest) IsZero() bool  { return digest == SelectionSpecDigest{} }

// SelectionSpec freezes caller selection semantics without projection epoch,
// discovery evidence, catalog generations, or runtime identities.
type SelectionSpec struct {
	share  catalog.ShareInstance
	root   catalog.DirectoryID
	rules  SelectionRules
	digest SelectionSpecDigest
	valid  bool
}

func NewSelectionSpec(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
) (SelectionSpec, error) {
	if share.IsZero() || root.IsZero() || !rules.validSnapshot() {
		return SelectionSpec{}, ErrInvalidSelectionRules
	}
	sum, err := computeSelectionDigest(share, root, rules)
	if err != nil {
		return SelectionSpec{}, err
	}
	return SelectionSpec{
		share:  share,
		root:   root,
		rules:  rules,
		digest: SelectionSpecDigest(sum),
		valid:  true,
	}, nil
}

func computeSelectionDigest(
	share catalog.ShareInstance,
	root catalog.DirectoryID,
	rules SelectionRules,
) ([sha256.Size]byte, error) {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(selectionSpecDomain))
	_, _ = hasher.Write([]byte{0, 1})
	writeHashField(hasher, share.Bytes())
	writeHashField(hasher, root.Bytes())
	writeHashField(hasher, []byte{byte(rules.mode)})
	defaultSelected := byte(0)
	if rules.defaultSelected {
		defaultSelected = 1
	}
	writeHashField(hasher, []byte{defaultSelected})
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
		writeHashUint64(hasher, uint64(len(nodes)))
		for _, node := range nodes {
			selected := byte(0)
			if node.selected {
				selected = 1
			}
			writeHashField(hasher, []byte{node.kind})
			writeHashField(hasher, node.identity)
			writeHashField(hasher, []byte{selected})
		}
	case SelectionByCatalogPath:
		paths := slices.Clone(rules.pathTargets)
		sort.Strings(paths)
		writeHashUint64(hasher, uint64(len(paths)))
		for _, path := range paths {
			writeHashField(hasher, []byte(path))
		}
	default:
		return [sha256.Size]byte{}, ErrInvalidSelectionRules
	}
	var sum [sha256.Size]byte
	hasher.Sum(sum[:0])
	return sum, nil
}

func (selection SelectionSpec) ShareInstance() catalog.ShareInstance { return selection.share }
func (selection SelectionSpec) SyntheticRoot() catalog.DirectoryID   { return selection.root }
func (selection SelectionSpec) SelectionRules() SelectionRules       { return selection.rules }
func (selection SelectionSpec) Digest() SelectionSpecDigest          { return selection.digest }
func (selection SelectionSpec) IsZero() bool                         { return !selection.valid }

func (selection SelectionSpec) Equal(other SelectionSpec) bool {
	return selection.valid && other.valid && selection.digest == other.digest
}

type selectionSpecJSON struct {
	Share           string                  `json:"share"`
	Root            string                  `json:"root"`
	Mode            string                  `json:"mode"`
	DefaultSelected bool                    `json:"default_selected,omitempty"`
	Overrides       []selectionOverrideJSON `json:"overrides,omitempty"`
	Paths           []string                `json:"paths,omitempty"`
}

type selectionOverrideJSON struct {
	DirectoryID string `json:"directory_id,omitempty"`
	FileID      string `json:"file_id,omitempty"`
	Selected    bool   `json:"selected"`
}

func (selection SelectionSpec) MarshalJSON() ([]byte, error) {
	if !selection.valid {
		return nil, ErrInvalidSelectionRules
	}
	payload := selectionSpecJSON{
		Share: hex.EncodeToString(selection.share.Bytes()),
		Root:  hex.EncodeToString(selection.root.Bytes()),
	}
	switch selection.rules.mode {
	case SelectionByNodeID:
		payload.Mode = "node_id"
		payload.DefaultSelected = selection.rules.defaultSelected
		overrides := make([]selectionOverrideJSON, 0, len(selection.rules.directories)+len(selection.rules.files))
		for id, selected := range selection.rules.directories {
			overrides = append(overrides, selectionOverrideJSON{
				DirectoryID: hex.EncodeToString(id.Bytes()),
				Selected:    selected,
			})
		}
		for id, selected := range selection.rules.files {
			overrides = append(overrides, selectionOverrideJSON{
				FileID:   hex.EncodeToString(id.Bytes()),
				Selected: selected,
			})
		}
		sort.Slice(overrides, func(left, right int) bool {
			leftKey := overrides[left].DirectoryID + overrides[left].FileID
			rightKey := overrides[right].DirectoryID + overrides[right].FileID
			if leftKey != rightKey {
				return leftKey < rightKey
			}
			return overrides[left].DirectoryID != "" && overrides[right].DirectoryID == ""
		})
		payload.Overrides = overrides
	case SelectionByCatalogPath:
		payload.Mode = "catalog_path"
		payload.Paths = slices.Clone(selection.rules.pathTargets)
	default:
		return nil, ErrInvalidSelectionRules
	}
	return json.Marshal(payload)
}

func (selection *SelectionSpec) UnmarshalJSON(data []byte) error {
	var payload selectionSpecJSON
	if err := decodeStrictJSON(data, &payload); err != nil {
		return ErrInvalidSelectionRules
	}
	shareRaw, err := hex.DecodeString(payload.Share)
	if err != nil {
		return ErrInvalidSelectionRules
	}
	share, err := catalog.ShareInstanceFromBytes(shareRaw)
	if err != nil {
		return ErrInvalidSelectionRules
	}
	rootRaw, err := hex.DecodeString(payload.Root)
	if err != nil {
		return ErrInvalidSelectionRules
	}
	root, err := catalog.DirectoryIDFromBytes(rootRaw)
	if err != nil {
		return ErrInvalidSelectionRules
	}
	var rules SelectionRules
	switch payload.Mode {
	case "node_id":
		rules, err = unmarshalNodeIDSelectionRules(payload)
	case "catalog_path":
		if payload.DefaultSelected || len(payload.Overrides) != 0 {
			return ErrInvalidSelectionRules
		}
		rules, err = NewPathSelectionRules(payload.Paths)
	default:
		return ErrInvalidSelectionRules
	}
	if err != nil {
		return err
	}
	rebuilt, err := NewSelectionSpec(share, root, rules)
	if err != nil {
		return err
	}
	*selection = rebuilt
	return nil
}

func unmarshalNodeIDSelectionRules(payload selectionSpecJSON) (SelectionRules, error) {
	if len(payload.Paths) != 0 || len(payload.Overrides) > MaxSelectionRuleOverrides {
		return SelectionRules{}, ErrInvalidSelectionRules
	}
	overrides := make([]SelectionOverride, 0, len(payload.Overrides))
	for _, item := range payload.Overrides {
		override, err := unmarshalSelectionOverride(item)
		if err != nil {
			return SelectionRules{}, err
		}
		overrides = append(overrides, override)
	}
	return NewSelectionRules(payload.DefaultSelected, overrides)
}

func unmarshalSelectionOverride(item selectionOverrideJSON) (SelectionOverride, error) {
	hasDir := item.DirectoryID != ""
	hasFile := item.FileID != ""
	if hasDir == hasFile {
		return SelectionOverride{}, ErrInvalidSelectionRules
	}
	if hasDir {
		raw, err := hex.DecodeString(item.DirectoryID)
		if err != nil {
			return SelectionOverride{}, ErrInvalidSelectionRules
		}
		id, err := catalog.DirectoryIDFromBytes(raw)
		if err != nil {
			return SelectionOverride{}, ErrInvalidSelectionRules
		}
		return SelectionOverride{DirectoryID: id, Selected: item.Selected}, nil
	}
	raw, err := hex.DecodeString(item.FileID)
	if err != nil {
		return SelectionOverride{}, ErrInvalidSelectionRules
	}
	id, err := catalog.FileIDFromBytes(raw)
	if err != nil {
		return SelectionOverride{}, ErrInvalidSelectionRules
	}
	return SelectionOverride{FileID: id, Selected: item.Selected}, nil
}

func writeHashUint64(w io.Writer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = w.Write(encoded[:])
}

func writeHashField(w io.Writer, value []byte) {
	writeHashUint64(w, uint64(len(value)))
	_, _ = w.Write(value)
}

func decodeStrictJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return errors.New("trailing json token")
	}
	return nil
}
