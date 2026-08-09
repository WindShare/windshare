package transfer

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

// DecodeReceiveIntent is the only persistence decoder for ReceiveIntentV1. It
// reconstructs every nested value through the validated constructors and then
// requires an exact canonical re-encode before returning durable authority.
func DecodeReceiveIntent(encoded []byte) (ReceiveIntent, error) {
	cursor, err := newReceiveIntentDecoder(encoded, receiveIntentDomain, ReceiveIntentV1)
	if err != nil {
		return ReceiveIntent{}, err
	}
	selectionRaw, selectionFrameErr := cursor.frame(cursor.remaining())
	artifactRaw, artifactFrameErr := cursor.frame(cursor.remaining())
	planRaw, planFrameErr := cursor.frame(cursor.remaining())
	selection, selectionErr := decodeSelectionSpec(selectionRaw)
	artifact, artifactErr := receivecontract.DecodeArtifactSpec(artifactRaw)
	plan, planErr := receivecontract.DecodeMaterializationPlan(planRaw, artifact)
	if firstReceiveIntentDecodeError(
		selectionFrameErr, artifactFrameErr, planFrameErr,
		selectionErr, artifactErr, planErr,
	) != nil || !cursor.done() {
		return ReceiveIntent{}, ErrInvalidReceiveIntent
	}
	intent, err := NewReceiveIntent(selection, artifact, plan)
	derivedDigest := ReceiveIntentDigest(sha256.Sum256(encoded))
	if err != nil || !bytes.Equal(intent.CanonicalBytes(), encoded) || intent.Digest() != derivedDigest {
		return ReceiveIntent{}, ErrInvalidReceiveIntent
	}
	return intent, nil
}

func decodeSelectionSpec(encoded []byte) (SelectionSpec, error) {
	cursor, err := newReceiveIntentDecoder(encoded, selectionSpecDomain, SelectionSpecV1)
	if err != nil {
		return SelectionSpec{}, err
	}
	shareRaw, shareFrameErr := cursor.fixedFrame(catalog.IdentityBytes)
	rootRaw, rootFrameErr := cursor.fixedFrame(catalog.IdentityBytes)
	mode, modeErr := cursor.framedByte()
	defaultSelected, defaultErr := cursor.framedBool()
	ruleCount, countErr := cursor.rawUint64()
	share, shareErr := catalog.ShareInstanceFromBytes(shareRaw)
	root, rootErr := catalog.DirectoryIDFromBytes(rootRaw)
	if firstReceiveIntentDecodeError(
		shareFrameErr, rootFrameErr, modeErr, defaultErr, countErr, shareErr, rootErr,
	) != nil {
		return SelectionSpec{}, ErrInvalidReceiveIntent
	}

	var rules SelectionRules
	switch SelectionMode(mode) {
	case SelectionByNodeID:
		if ruleCount > MaxSelectionRuleOverrides {
			return SelectionSpec{}, ErrInvalidReceiveIntent
		}
		overrides := make([]SelectionOverride, 0, int(ruleCount))
		for range ruleCount {
			kind, kindErr := cursor.framedByte()
			identityRaw, identityErr := cursor.fixedFrame(catalog.IdentityBytes)
			selected, selectedErr := cursor.framedBool()
			if firstReceiveIntentDecodeError(kindErr, identityErr, selectedErr) != nil {
				return SelectionSpec{}, ErrInvalidReceiveIntent
			}
			switch kind {
			case 1:
				directory, idErr := catalog.DirectoryIDFromBytes(identityRaw)
				if idErr != nil {
					return SelectionSpec{}, ErrInvalidReceiveIntent
				}
				overrides = append(overrides, SelectionOverride{DirectoryID: directory, Selected: selected})
			case 2:
				file, idErr := catalog.FileIDFromBytes(identityRaw)
				if idErr != nil {
					return SelectionSpec{}, ErrInvalidReceiveIntent
				}
				overrides = append(overrides, SelectionOverride{FileID: file, Selected: selected})
			default:
				return SelectionSpec{}, ErrInvalidReceiveIntent
			}
		}
		rules, err = NewSelectionRules(defaultSelected, overrides)
	case SelectionByCatalogPath:
		if defaultSelected || ruleCount == 0 || ruleCount > MaxSelectionPathTargets {
			return SelectionSpec{}, ErrInvalidReceiveIntent
		}
		paths := make([]string, 0, int(ruleCount))
		totalBytes := uint64(0)
		for range ruleCount {
			pathRaw, pathErr := cursor.frame(catalog.MaxPathBytes)
			if pathErr != nil {
				return SelectionSpec{}, ErrInvalidReceiveIntent
			}
			totalBytes += uint64(len(pathRaw))
			if totalBytes > MaxSelectionPathTargetBytes {
				return SelectionSpec{}, ErrInvalidReceiveIntent
			}
			paths = append(paths, string(pathRaw))
		}
		rules, err = NewPathSelectionRules(paths)
	default:
		return SelectionSpec{}, ErrInvalidReceiveIntent
	}
	if err != nil || !cursor.done() {
		return SelectionSpec{}, ErrInvalidReceiveIntent
	}
	selection, err := NewSelectionSpec(share, root, rules)
	if err != nil || !bytes.Equal(selection.CanonicalBytes(), encoded) {
		return SelectionSpec{}, ErrInvalidReceiveIntent
	}
	return selection, nil
}

type receiveIntentDecoder struct {
	encoded []byte
	offset  int
}

func newReceiveIntentDecoder(encoded []byte, domain string, version uint8) (receiveIntentDecoder, error) {
	prefix := append(append([]byte(nil), domain...), 0, version)
	if !bytes.HasPrefix(encoded, prefix) {
		return receiveIntentDecoder{}, ErrInvalidReceiveIntent
	}
	return receiveIntentDecoder{encoded: encoded, offset: len(prefix)}, nil
}

func (cursor *receiveIntentDecoder) remaining() int { return len(cursor.encoded) - cursor.offset }
func (cursor *receiveIntentDecoder) done() bool     { return cursor.offset == len(cursor.encoded) }

func (cursor *receiveIntentDecoder) rawUint64() (uint64, error) {
	if cursor.remaining() < 8 {
		return 0, ErrInvalidReceiveIntent
	}
	value := binary.BigEndian.Uint64(cursor.encoded[cursor.offset : cursor.offset+8])
	cursor.offset += 8
	return value, nil
}

func (cursor *receiveIntentDecoder) frame(maximum int) ([]byte, error) {
	length, err := cursor.rawUint64()
	if err != nil || maximum < 0 || length > uint64(maximum) || length > uint64(cursor.remaining()) {
		return nil, ErrInvalidReceiveIntent
	}
	value := cursor.encoded[cursor.offset : cursor.offset+int(length)]
	cursor.offset += int(length)
	return value, nil
}

func (cursor *receiveIntentDecoder) fixedFrame(size int) ([]byte, error) {
	value, err := cursor.frame(size)
	if err != nil || len(value) != size {
		return nil, ErrInvalidReceiveIntent
	}
	return value, nil
}

func (cursor *receiveIntentDecoder) framedByte() (byte, error) {
	value, err := cursor.fixedFrame(1)
	if err != nil {
		return 0, err
	}
	return value[0], nil
}

func (cursor *receiveIntentDecoder) framedBool() (bool, error) {
	value, err := cursor.framedByte()
	if err != nil || value > 1 {
		return false, ErrInvalidReceiveIntent
	}
	return value == 1, nil
}

func firstReceiveIntentDecodeError(errors ...error) error {
	for _, err := range errors {
		if err != nil {
			return err
		}
	}
	return nil
}
