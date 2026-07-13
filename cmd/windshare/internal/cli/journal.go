package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"unicode/utf8"

	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
)

// Journal v2 is the durable authority for one download transaction. The file
// name only namespaces shares; the complete fingerprint and PlanID inside are
// authoritative, and exact owned paths are the only reopen capability. Its
// unkeyed checksum detects accidental corruption before have-state can suppress
// writes; resisting a same-account attacker requires the separately deferred MAC.
const (
	journalNamePrefix         = ".wsresume-"
	journalFpPrefixBytes      = 8
	journalVersion       byte = 2

	journalPayloadFixedBytes = 8 + 1 + manifest.FingerprintBytes + share.PlanIDBytes + 8 + 8
	journalChecksumBytes     = sha256.Size
	journalFixedBytes        = journalPayloadFixedBytes + journalChecksumBytes
	journalPathLengthBytes   = 4
	maxJournalBytes          = 2 * manifest.MaxManifestSize
)

var journalMagic = [8]byte{'W', 'S', 'R', 'E', 'S', 'U', 'M', 'E'}

var (
	errJournalCorrupt     = errors.New("cli: resume journal is corrupt")
	errJournalFingerprint = errors.New("cli: resume journal manifest fingerprint mismatch")
	errJournalPlan        = errors.New("cli: resume journal transfer plan mismatch")
	errJournalUnbound     = errors.New("cli: resume journal is not bound to a transfer plan")
)

func journalPath(outDir string, fingerprint manifest.Fingerprint) string {
	name := journalNamePrefix + base64.RawURLEncoding.EncodeToString(fingerprint[:journalFpPrefixBytes])
	return filepath.Join(outDir, name)
}

type journalState struct {
	fingerprint manifest.Fingerprint
	planID      share.PlanID
	have        session.Bitfield
	owned       []string
}

// resumeJournal implements osfs.OwnershipLedger. RecordCreated persists the
// expanded capability before Sink is allowed to write into the new file.
type resumeJournal struct {
	mu sync.Mutex

	path        string
	fingerprint manifest.Fingerprint
	planID      share.PlanID
	have        session.Bitfield
	owned       map[string]struct{}
	selected    map[string]struct{}
	loaded      bool
	bound       bool
}

func loadResume(path string, want manifest.Fingerprint) (*resumeJournal, error) {
	state, err := readJournal(path)
	if errors.Is(err, os.ErrNotExist) {
		return &resumeJournal{
			path:        path,
			fingerprint: want,
			owned:       make(map[string]struct{}),
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if state.fingerprint != want {
		return nil, fmt.Errorf("%w: %q", errJournalFingerprint, path)
	}
	owned := make(map[string]struct{}, len(state.owned))
	for _, canonicalPath := range state.owned {
		owned[canonicalPath] = struct{}{}
	}
	return &resumeJournal{
		path:        path,
		fingerprint: state.fingerprint,
		planID:      state.planID,
		have:        state.have,
		owned:       owned,
		loaded:      true,
	}, nil
}

func (j *resumeJournal) Bind(plan *share.TransferPlan) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.bound {
		if j.planID != plan.PlanID() {
			return fmt.Errorf("%w: journal already bound to %s, requested=%s", errJournalPlan, j.planID, plan.PlanID())
		}
		return nil
	}
	if j.loaded && j.planID != plan.PlanID() {
		return fmt.Errorf("%w: journal=%s requested=%s", errJournalPlan, j.planID, plan.PlanID())
	}

	selectedFiles := make(map[string]struct{})
	for _, entry := range plan.SelectedEntries() {
		if !entry.IsDir {
			selectedFiles[entry.Path] = struct{}{}
		}
	}
	for ownedPath := range j.owned {
		if _, selected := selectedFiles[ownedPath]; !selected {
			return fmt.Errorf("%w: owned path %q is outside transfer plan", errJournalPlan, ownedPath)
		}
	}

	target := plan.Sink().Have()
	if j.loaded {
		if j.have.Len() != target.Len() {
			return fmt.Errorf("%w: have length %d, manifest geometry %d", errJournalCorrupt, j.have.Len(), target.Len())
		}
		chunks := plan.Chunks()
		for index := range j.have.SetBits() {
			if !chunks.Contains(index) {
				return fmt.Errorf("%w: completed chunk %d is outside transfer plan", errJournalPlan, index)
			}
		}
		if err := target.Restore(j.have); err != nil {
			return fmt.Errorf("%w: restore have state: %w", errJournalCorrupt, err)
		}
	}
	j.planID = plan.PlanID()
	j.have = target
	j.selected = selectedFiles
	j.bound = true
	return nil
}

func (j *resumeJournal) Resuming() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.loaded
}

func (j *resumeJournal) Owns(path string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.bound {
		return false
	}
	_, ok := j.owned[path]
	return ok
}

func (j *resumeJournal) RecordCreated(path string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.bound {
		return errJournalUnbound
	}
	if _, selected := j.selected[path]; !selected {
		return fmt.Errorf("%w: cannot own unselected path %q", errJournalPlan, path)
	}
	if _, exists := j.owned[path]; exists {
		return nil
	}
	j.owned[path] = struct{}{}
	if err := j.persistLocked(); err != nil {
		delete(j.owned, path)
		return err
	}
	return nil
}

func (j *resumeJournal) Checkpoint() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.bound {
		return errJournalUnbound
	}
	return j.persistLocked()
}

func (j *resumeJournal) RemoveIfUnowned() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.owned) != 0 {
		return nil
	}
	if err := os.Remove(j.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cli: remove empty resume journal %q: %w", j.path, err)
	}
	return nil
}

func (j *resumeJournal) Remove() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := os.Remove(j.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("cli: remove resume journal %q: %w", j.path, err)
	}
	return nil
}

func (j *resumeJournal) persistLocked() error {
	owned := make([]string, 0, len(j.owned))
	for canonicalPath := range j.owned {
		owned = append(owned, canonicalPath)
	}
	slices.Sort(owned)
	return writeJournal(j.path, journalState{
		fingerprint: j.fingerprint,
		planID:      j.planID,
		have:        j.have,
		owned:       owned,
	})
}

func writeJournal(path string, state journalState) error {
	encoded, err := encodeJournal(state)
	if err != nil {
		return err
	}
	// A unique exclusive temp file prevents a pre-existing predictable .tmp
	// path (including a symlink) from being truncated before the atomic rename.
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("cli: create temporary resume journal for %q: %w", path, err)
	}
	tmp := file.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmp)
		}
	}()
	written, writeErr := file.Write(encoded)
	if writeErr == nil && written != len(encoded) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return fmt.Errorf("cli: write resume journal %q: %w", tmp, errors.Join(writeErr, file.Close()))
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("cli: sync resume journal %q: %w", tmp, errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("cli: close resume journal %q: %w", tmp, err)
	}
	if err := installJournal(tmp, path); err != nil {
		return fmt.Errorf("cli: install resume journal %q: %w", path, err)
	}
	removeTemp = false
	return nil
}

func encodeJournal(state journalState) ([]byte, error) {
	bitfield, err := state.have.MarshalBinary()
	if err != nil {
		return nil, fmt.Errorf("cli: encode resume have-state: %w", err)
	}
	if len(bitfield) > maxJournalBytes-journalFixedBytes {
		return nil, fmt.Errorf("%w: have-state size %d exceeds journal budget", errJournalCorrupt, len(bitfield))
	}
	remaining := maxJournalBytes - journalFixedBytes - len(bitfield)
	ownedBytes := 0
	for i, canonicalPath := range state.owned {
		if uint64(len(canonicalPath)) > uint64(^uint32(0)) {
			return nil, fmt.Errorf("cli: owned path is too long")
		}
		// Check against the small journal budget before adding lengths. This keeps
		// the encoder correct on 32-bit systems even if an in-memory caller supplies
		// a path set whose aggregate length would overflow int.
		encodedPathBytes, fits := journalPathEncodedSize(remaining, len(canonicalPath))
		if !fits {
			return nil, fmt.Errorf("%w: owned path length %d exceeds remaining journal budget %d", errJournalCorrupt, len(canonicalPath), max(remaining-journalPathLengthBytes, 0))
		}
		if err := manifest.ValidatePath(canonicalPath); err != nil {
			return nil, fmt.Errorf("cli: encode owned path %q: %w", canonicalPath, err)
		}
		if i > 0 && state.owned[i-1] >= canonicalPath {
			return nil, fmt.Errorf("cli: owned paths are not strictly sorted")
		}
		ownedBytes += encodedPathBytes
		remaining -= encodedPathBytes
	}
	total := journalFixedBytes + len(bitfield) + ownedBytes
	encoded := make([]byte, 0, total)
	encoded = append(encoded, journalMagic[:]...)
	encoded = append(encoded, journalVersion)
	encoded = append(encoded, state.fingerprint[:]...)
	encoded = append(encoded, state.planID[:]...)
	var number [8]byte
	binary.LittleEndian.PutUint64(number[:], uint64(len(bitfield)))
	encoded = append(encoded, number[:]...)
	encoded = append(encoded, bitfield...)
	binary.LittleEndian.PutUint64(number[:], uint64(len(state.owned)))
	encoded = append(encoded, number[:]...)
	var pathLength [journalPathLengthBytes]byte
	for _, canonicalPath := range state.owned {
		binary.LittleEndian.PutUint32(pathLength[:], uint32(len(canonicalPath)))
		encoded = append(encoded, pathLength[:]...)
		encoded = append(encoded, canonicalPath...)
	}
	checksum := sha256.Sum256(encoded)
	encoded = append(encoded, checksum[:]...)
	return encoded, nil
}

func journalPathEncodedSize(remaining, pathBytes int) (int, bool) {
	if remaining < journalPathLengthBytes || pathBytes < 0 || pathBytes > remaining-journalPathLengthBytes {
		return 0, false
	}
	return journalPathLengthBytes + pathBytes, true
}

func readJournal(path string) (journalState, error) {
	file, err := os.Open(path)
	if err != nil {
		return journalState{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxJournalBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return journalState{}, fmt.Errorf("cli: read resume journal %q: %w", path, errors.Join(readErr, closeErr))
	}
	if len(data) > maxJournalBytes {
		return journalState{}, fmt.Errorf("%w: %q exceeds %d bytes", errJournalCorrupt, path, maxJournalBytes)
	}
	state, err := decodeJournal(data)
	if err != nil {
		return journalState{}, fmt.Errorf("%w: %q: %w", errJournalCorrupt, path, err)
	}
	return state, nil
}

func decodeJournal(data []byte) (journalState, error) {
	if len(data) < journalFixedBytes || !bytes.Equal(data[:len(journalMagic)], journalMagic[:]) {
		return journalState{}, errors.New("header mismatch")
	}
	payloadEnd := len(data) - journalChecksumBytes
	wantChecksum := data[payloadEnd:]
	gotChecksum := sha256.Sum256(data[:payloadEnd])
	if !bytes.Equal(wantChecksum, gotChecksum[:]) {
		return journalState{}, errors.New("checksum mismatch")
	}
	data = data[:payloadEnd]
	cursor := len(journalMagic)
	if version := data[cursor]; version != journalVersion {
		return journalState{}, fmt.Errorf("unsupported version %d", version)
	}
	cursor++
	var state journalState
	copy(state.fingerprint[:], data[cursor:cursor+manifest.FingerprintBytes])
	cursor += manifest.FingerprintBytes
	copy(state.planID[:], data[cursor:cursor+share.PlanIDBytes])
	cursor += share.PlanIDBytes
	bitfieldLength := binary.LittleEndian.Uint64(data[cursor : cursor+8])
	cursor += 8
	if bitfieldLength > uint64(len(data)-cursor) {
		return journalState{}, fmt.Errorf("invalid have-state length %d", bitfieldLength)
	}
	if err := state.have.UnmarshalBinary(data[cursor : cursor+int(bitfieldLength)]); err != nil {
		return journalState{}, fmt.Errorf("invalid have-state: %w", err)
	}
	cursor += int(bitfieldLength)
	if len(data)-cursor < 8 {
		return journalState{}, errors.New("missing owned-path count")
	}
	ownedCount := binary.LittleEndian.Uint64(data[cursor : cursor+8])
	cursor += 8
	if ownedCount > uint64((len(data)-cursor)/journalPathLengthBytes) {
		return journalState{}, fmt.Errorf("invalid owned-path count %d", ownedCount)
	}
	// The authenticated count is still hostile input because the deferred journal
	// MAC does not protect against a same-account writer. Grow only after each path
	// validates so a forged count cannot amplify a bounded file into a large eager
	// string-slice allocation.
	for range ownedCount {
		if len(data)-cursor < journalPathLengthBytes {
			return journalState{}, errors.New("truncated owned-path length")
		}
		pathLength := binary.LittleEndian.Uint32(data[cursor : cursor+journalPathLengthBytes])
		cursor += journalPathLengthBytes
		if uint64(pathLength) > uint64(len(data)-cursor) {
			return journalState{}, errors.New("truncated owned path")
		}
		pathBytes := data[cursor : cursor+int(pathLength)]
		cursor += int(pathLength)
		if !utf8.Valid(pathBytes) {
			return journalState{}, errors.New("owned path is not UTF-8")
		}
		canonicalPath := string(pathBytes)
		if err := manifest.ValidatePath(canonicalPath); err != nil {
			return journalState{}, fmt.Errorf("invalid owned path %q: %w", canonicalPath, err)
		}
		if len(state.owned) != 0 && state.owned[len(state.owned)-1] >= canonicalPath {
			return journalState{}, errors.New("owned paths are not strictly sorted")
		}
		state.owned = append(state.owned, canonicalPath)
	}
	if cursor != len(data) {
		return journalState{}, fmt.Errorf("%d trailing bytes", len(data)-cursor)
	}
	return state, nil
}
