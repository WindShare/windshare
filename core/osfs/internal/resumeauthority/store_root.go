package resumeauthority

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

type resumeRootPins struct {
	root           outputcap.Directory
	controlPin     outputcap.CurrentEntryReference
	control        outputcap.Directory
	checkpointPin  outputcap.CurrentEntryReference
	checkpointRoot outputcap.Directory
	ownershipPin   outputcap.CurrentEntryReference
	intentsPin     outputcap.CurrentEntryReference
	intents        outputcap.Directory
	leasesPin      outputcap.CurrentEntryReference
	leases         outputcap.Directory
	namespace      checkpointstore.Namespace
	ownershipImage []byte
}

func openResumeRoot(
	config checkpointstore.CertifiedConfig,
) (result *resumeRootPins, attention []Attention, resultErr error) {
	if err := validateStoreConfig(config); err != nil {
		return nil, nil, err
	}
	root, err := config.Root.Duplicate()
	if err != nil || root == nil {
		return nil, nil, errors.Join(err, outputcap.ErrUnsafeNamespace)
	}
	result = &resumeRootPins{root: root}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, result.Close())
			result = nil
		}
	}()

	result.controlPin, result.control, err = pinExistingDirectory(root, checkpointstore.ControlDirectory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = errors.Join(errResumeNamespaceAbsent, err)
		}
		return nil, nil, err
	}
	result.checkpointPin, result.checkpointRoot, err =
		pinExistingDirectory(result.control, checkpointstore.CheckpointDirectory)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			err = errors.Join(errResumeNamespaceAbsent, err)
		}
		return nil, nil, err
	}
	result.ownershipImage, err = checkpointmodel.EncodeOwnership(config.Ownership)
	if err != nil {
		return nil, nil, err
	}
	result.ownershipPin, err = pinExactFile(
		result.checkpointRoot, checkpointstore.OwnershipFile, result.ownershipImage,
	)
	if err != nil {
		return nil, nil, errors.Join(errResumeOwnershipBinding, err)
	}
	result.intentsPin, result.intents, err =
		pinExistingDirectory(result.checkpointRoot, checkpointstore.IntentsDirectory)
	if err != nil {
		return nil, nil, err
	}
	result.leasesPin, result.leases, err =
		pinExistingDirectory(result.checkpointRoot, checkpointstore.LeasesDirectory)
	if err != nil {
		return nil, nil, err
	}
	result.namespace, err = checkpointstore.AdoptPinnedNamespace(
		result.checkpointRoot, result.intents, result.leases, config.Ownership,
	)
	if err != nil {
		return nil, nil, err
	}
	attention, err = inspectResumeRootEntries(result.checkpointRoot)
	if err != nil {
		return nil, nil, err
	}
	return result, attention, nil
}

func inspectResumeRootEntries(
	checkpointRoot outputcap.Directory,
) ([]Attention, error) {
	names, err := checkpointRoot.Names(checkpointstore.CheckpointRootEntryLimit() + 1)
	if err != nil {
		return nil, err
	}
	attention := make([]Attention, 0)
	if len(names) > checkpointstore.CheckpointRootEntryLimit() {
		attention = append(attention,
			resumeAdapterAttention(AttentionUnknownChildren, []byte("root-overflow")))
	}
	for _, name := range names {
		expected, known := checkpointstore.CheckpointRootEntryKind(name)
		if !known {
			attention = append(attention,
				resumeAdapterAttention(AttentionUnknownChildren, []byte(name)))
			continue
		}
		kind, exact, classifyErr := checkpointRoot.ClassifyExactEntry(name)
		if classifyErr != nil {
			return nil, classifyErr
		}
		if !exact || kind != expected {
			attention = append(attention,
				resumeAdapterAttention(AttentionCorruptBinding, []byte(name)))
		}
	}
	return attention, nil
}

func (pins *resumeRootPins) revalidate() (Evidence, error) {
	if pins == nil || pins.root == nil || pins.control == nil ||
		pins.checkpointRoot == nil || pins.intents == nil ||
		pins.leases == nil || len(pins.ownershipImage) == 0 {
		return EvidenceAmbiguous, transfer.ErrInvalidOutputBinding
	}
	checks := []struct {
		parent outputcap.Directory
		name   string
		pin    outputcap.CurrentEntryReference
	}{
		{pins.root, checkpointstore.ControlDirectory, pins.controlPin},
		{pins.control, checkpointstore.CheckpointDirectory, pins.checkpointPin},
		{pins.checkpointRoot, checkpointstore.OwnershipFile, pins.ownershipPin},
		{pins.checkpointRoot, checkpointstore.IntentsDirectory, pins.intentsPin},
		{pins.checkpointRoot, checkpointstore.LeasesDirectory, pins.leasesPin},
	}
	for _, check := range checks {
		evidence, err := pinnedEntryEvidence(check.parent, check.name, check.pin)
		if err != nil || evidence != EvidenceExact {
			return evidence, err
		}
	}
	if err := readPinnedExactFile(
		pins.checkpointRoot, checkpointstore.OwnershipFile, pins.ownershipPin, pins.ownershipImage,
	); err != nil {
		if errors.Is(err, errResumePinReplaced) || errors.Is(err, checkpointmodel.ErrRecordBinding) {
			return EvidenceReplaced, nil
		}
		return EvidenceAmbiguous, err
	}
	attention, err := inspectResumeRootEntries(pins.checkpointRoot)
	if err != nil {
		return EvidenceAmbiguous, err
	}
	if len(attention) > 0 {
		return EvidenceAmbiguous, nil
	}
	return EvidenceExact, nil
}

func (pins *resumeRootPins) sameLineage(previous *resumeRootPins) (bool, error) {
	if pins == nil || previous == nil {
		return false, transfer.ErrInvalidOutputBinding
	}
	checks := []struct {
		parent outputcap.Directory
		name   string
		pin    outputcap.CurrentEntryReference
	}{
		{pins.root, checkpointstore.ControlDirectory, previous.controlPin},
		{pins.control, checkpointstore.CheckpointDirectory, previous.checkpointPin},
		{pins.checkpointRoot, checkpointstore.OwnershipFile, previous.ownershipPin},
		{pins.checkpointRoot, checkpointstore.IntentsDirectory, previous.intentsPin},
		{pins.checkpointRoot, checkpointstore.LeasesDirectory, previous.leasesPin},
	}
	for _, check := range checks {
		matches, err := check.parent.EntryMatches(check.name, check.pin)
		if err != nil || !matches {
			return false, err
		}
	}
	return true, nil
}

func (pins *resumeRootPins) Close() error {
	if pins == nil {
		return nil
	}
	err := errors.Join(
		closeDirectory(pins.intents),
		closeDirectory(pins.leases),
		closeEntryReference(pins.intentsPin),
		closeEntryReference(pins.leasesPin),
		closeEntryReference(pins.ownershipPin),
		closeDirectory(pins.checkpointRoot),
		closeEntryReference(pins.checkpointPin),
		closeDirectory(pins.control),
		closeEntryReference(pins.controlPin),
		closeDirectory(pins.root),
	)
	*pins = resumeRootPins{}
	return err
}

func pinExistingDirectory(
	parent outputcap.Directory,
	name string,
) (outputcap.CurrentEntryReference, outputcap.Directory, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, nil, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, nil, fs.ErrNotExist
	}
	if !exact || kind != outputcap.EntryDirectory {
		return nil, nil, outputcap.ErrUnsafeNamespace
	}
	pin, err := parent.OpenEntry(name)
	if err != nil {
		return nil, nil, errors.Join(err, closeEntryReference(pin))
	}
	if pin == nil || pin.Kind() != outputcap.EntryDirectory {
		return nil, nil, errors.Join(outputcap.ErrUnsafeNamespace, closeEntryReference(pin))
	}
	matches, err := parent.EntryMatches(name, pin)
	if err != nil || !matches {
		return nil, nil, errors.Join(errResumePinReplaced, err, closeEntryReference(pin))
	}
	directory, err := parent.OpenPinnedDirectory(pin, true)
	if err != nil || directory == nil {
		return nil, nil, errors.Join(err, outputcap.ErrUnsafeNamespace,
			closeDirectory(directory), closeEntryReference(pin))
	}
	matches, err = parent.EntryMatches(name, pin)
	if err != nil || !matches {
		return nil, nil, errors.Join(errResumePinReplaced, err,
			closeDirectory(directory), closeEntryReference(pin))
	}
	return pin, directory, nil
}

func pinExactFile(
	parent outputcap.Directory,
	name string,
	expected []byte,
) (outputcap.CurrentEntryReference, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return nil, err
	}
	if kind == outputcap.EntryAbsent {
		return nil, fs.ErrNotExist
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return nil, outputcap.ErrUnsafeNamespace
	}
	pin, err := parent.OpenEntry(name)
	if err != nil {
		return nil, errors.Join(err, closeEntryReference(pin))
	}
	if pin == nil || pin.Kind() != outputcap.EntryRegularFile {
		return nil, errors.Join(outputcap.ErrUnsafeNamespace, closeEntryReference(pin))
	}
	if err := readPinnedExactFile(parent, name, pin, expected); err != nil {
		return nil, errors.Join(err, closeEntryReference(pin))
	}
	return pin, nil
}

func readPinnedExactFile(
	parent outputcap.Directory,
	name string,
	pin outputcap.CurrentEntryReference,
	expected []byte,
) error {
	actual, err := readPinnedFileBytes(parent, name, pin)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, expected) {
		return errors.Join(errResumePinReplaced, checkpointmodel.ErrRecordBinding)
	}
	return nil
}

func readPinnedFileBytes(
	parent outputcap.Directory,
	name string,
	pin outputcap.CurrentEntryReference,
) ([]byte, error) {
	evidence, err := pinnedEntryEvidence(parent, name, pin)
	if err != nil || evidence != EvidenceExact {
		return nil, errors.Join(errResumePinReplaced, err)
	}
	actual, readErr := checkpointstore.ReadFile(parent, name)
	evidence, matchErr := pinnedEntryEvidence(parent, name, pin)
	if readErr != nil || matchErr != nil {
		return nil, errors.Join(readErr, matchErr)
	}
	if evidence != EvidenceExact {
		return nil, errResumePinReplaced
	}
	return actual, nil
}

func pinnedEntryEvidence(
	parent outputcap.Directory,
	name string,
	pin outputcap.CurrentEntryReference,
) (Evidence, error) {
	if parent == nil || name == "" || pin == nil {
		return EvidenceAmbiguous, transfer.ErrInvalidOutputBinding
	}
	matches, err := parent.EntryMatches(name, pin)
	if err != nil {
		return EvidenceAmbiguous, err
	}
	if matches {
		return EvidenceExact, nil
	}
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return EvidenceAmbiguous, err
	}
	if kind == outputcap.EntryAbsent {
		return EvidenceAbsent, nil
	}
	if !exact {
		return EvidenceAmbiguous, nil
	}
	return EvidenceReplaced, nil
}

func parseIntentNamespaceName(name string) (transfer.TransferIntentDigest, bool) {
	if len(name) != hex.EncodedLen(sha256.Size) || name != string(bytes.ToLower([]byte(name))) {
		return transfer.TransferIntentDigest{}, false
	}
	return decodeIntentNamespaceName(name)
}

func decodeIntentNamespaceName(name string) (transfer.TransferIntentDigest, bool) {
	if len(name) != hex.EncodedLen(sha256.Size) {
		return transfer.TransferIntentDigest{}, false
	}
	raw, err := hex.DecodeString(name)
	if err != nil {
		return transfer.TransferIntentDigest{}, false
	}
	intent, err := transfer.TransferIntentDigestFromBytes(raw)
	return intent, err == nil
}

func resumeAdapterAttention(
	reason AttentionReason,
	scope []byte,
) Attention {
	hash := sha256.New()
	_, _ = hash.Write([]byte(resumeAdapterAttentionDomain))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write([]byte(reason))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(scope)
	attention, _ := NewAttention(reason, hex.EncodeToString(hash.Sum(nil)))
	return attention
}

func resumeOpenAttention(err error) (AttentionReason, bool) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return AttentionMissingOwnership, true
	case errors.Is(err, errResumePinReplaced):
		return AttentionReplacement, true
	case errors.Is(err, errResumeOwnershipBinding),
		errors.Is(err, checkpointmodel.ErrInvalidOwnership),
		errors.Is(err, checkpointmodel.ErrOwnershipChecksum),
		errors.Is(err, checkpointmodel.ErrOwnershipNonCanonical),
		errors.Is(err, outputcap.ErrUnsafeNamespace):
		return AttentionCorruptBinding, true
	default:
		return "", false
	}
}

func projectResumeError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	code := checkpointstore.ErrorCodeFor(err)
	if errors.Is(err, ErrInvalidContract) {
		code = checkpointstore.ErrorUnsafeInstall
	}
	if errors.Is(err, errResumeOwnershipBinding) {
		code = checkpointstore.ErrorOwnershipMismatch
	}
	projected := RepositoryStateIO
	switch code {
	case checkpointstore.ErrorBusy:
		projected = RepositoryBusy
	case checkpointstore.ErrorCorruptRecord:
		projected = RepositoryCorruptRecord
	case checkpointstore.ErrorUnsafeInstall:
		projected = RepositoryUnsafeInstall
	case checkpointstore.ErrorOwnershipMismatch:
		projected = RepositoryOwnershipMismatch
	case checkpointstore.ErrorStateIO:
		projected = RepositoryStateIO
	}
	return NewRepositoryError(projected, operation, err)
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalidContract
	}
	return ctx.Err()
}

func closeEntryReference(reference outputcap.CurrentEntryReference) error {
	if reference == nil {
		return nil
	}
	return reference.Close()
}

func closeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func validateStoreConfig(config checkpointstore.CertifiedConfig) error {
	if config.Root == nil || !config.Ownership.Valid() {
		return transfer.ErrInvalidOutputBinding
	}
	return nil
}

var _ Repository = ResumeRepository{}
var _ PinnedInventory = (*resumeInventory)(nil)
