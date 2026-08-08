package directoryauthority

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
)

// PublicationPlatform is separate from the runtime claim authority. Resume has
// no process-local directory claims to reuse, but it must use the same guarded,
// handle-relative root and platform locator rules.
type PublicationPlatform interface {
	AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error)
	CanonicalLocatorKey(string) (string, error)
	CanonicalComponentKey(string) (string, error)
}

type PublicationObserver struct {
	platform    PublicationPlatform
	reservedKey string
}

func NewPublicationObserver(platform PublicationPlatform) (*PublicationObserver, error) {
	if platform == nil {
		return nil, ErrInvalidConfiguration
	}
	reservedKey, err := platform.CanonicalComponentKey(reservedControlComponent)
	if err != nil || reservedKey == "" {
		return nil, errors.Join(ErrInvalidConfiguration, err)
	}
	return &PublicationObserver{platform: platform, reservedKey: reservedKey}, nil
}

type publicationLineageEntry struct {
	parent    outputcap.Directory
	name      string
	reference outputcap.CurrentEntryReference
	directory outputcap.Directory
}

type publicationMissingEntry struct {
	parent outputcap.Directory
	name   string
}

type publicationPin struct {
	mu sync.Mutex

	recordID  checkpointmodel.RecordID
	exactSize uint64
	initial   resumeauthority.Evidence
	guard     outputcap.PublicOperationGuard
	root      outputcap.Directory
	lineage   []publicationLineageEntry
	parent    outputcap.Directory
	leaf      string
	missing   *publicationMissingEntry
	finalRef  outputcap.CurrentEntryReference
	final     outputcap.File
	closed    bool
	closeErr  error
}

func (observer *PublicationObserver) PinPublication(
	ctx context.Context,
	checkpoint resumeauthority.PinnedCheckpoint,
) (_ resumeauthority.PinnedPublication, resultErr error) {
	if observer == nil || observer.platform == nil || ctx == nil || checkpoint == nil {
		return nil, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request, err := observer.validatePublicationRequest(checkpoint.Record())
	if err != nil {
		return nil, err
	}
	pin, err := observer.acquirePublicationPin(request)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, pin.Close())
		}
	}()
	settled, err := pinPublicationLineage(ctx, pin, request.components[:len(request.components)-1])
	if err != nil {
		return nil, err
	}
	if !settled {
		if err := pinPublicationLeaf(ctx, checkpoint, request.record, pin); err != nil {
			return nil, err
		}
	}
	return observer.finishPublicationPin(pin, request.record.RecordID())
}

type publicationRequest struct {
	record     checkpointmodel.Record
	components []string
}

func (observer *PublicationObserver) validatePublicationRequest(
	record checkpointmodel.Record,
) (publicationRequest, error) {
	canonical, err := catalog.CanonicalPath(record.CanonicalPath())
	if err != nil || canonical != record.CanonicalPath() || canonical == "" || !record.Valid() {
		return publicationRequest{}, errors.Join(ErrInvalidLocator, err)
	}
	if key, keyErr := observer.platform.CanonicalLocatorKey(canonical); keyErr != nil || key == "" {
		return publicationRequest{}, errors.Join(ErrInvalidLocator, keyErr)
	}
	components := strings.Split(canonical, "/")
	for index, component := range components {
		key, keyErr := observer.platform.CanonicalComponentKey(component)
		if keyErr != nil || key == "" || index == 0 && key == observer.reservedKey {
			return publicationRequest{}, errors.Join(ErrInvalidLocator, keyErr)
		}
	}
	return publicationRequest{record: record, components: components}, nil
}

func (observer *PublicationObserver) acquirePublicationPin(
	request publicationRequest,
) (*publicationPin, error) {
	guard, err := observer.platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	root := guard.Root()
	if root == nil {
		return nil, errors.Join(ErrRetainedAuthorityChanged, guard.Close())
	}
	return &publicationPin{
		recordID: request.record.RecordID(), exactSize: request.record.ExactSize(),
		guard: guard, root: root, parent: root, leaf: request.components[len(request.components)-1],
	}, nil
}

func pinPublicationLineage(
	ctx context.Context,
	pin *publicationPin,
	components []string,
) (bool, error) {
	current := pin.root
	for _, component := range components {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		kind, exact, classifyErr := current.ClassifyExactEntry(component)
		if classifyErr != nil {
			return false, classifyErr
		}
		if kind == outputcap.EntryAbsent {
			pin.initial = resumeauthority.EvidenceAbsent
			pin.missing = &publicationMissingEntry{parent: current, name: component}
			return true, nil
		}
		if !exact || kind != outputcap.EntryDirectory {
			pin.initial = resumeauthority.EvidenceAmbiguous
			return true, nil
		}
		reference, openErr := current.OpenEntry(component)
		if openErr != nil || reference == nil || reference.Kind() != outputcap.EntryDirectory {
			return true, settleAmbiguousPublication(pin, openErr, closeEntry(reference))
		}
		directory, openErr := current.OpenPinnedDirectory(reference, false)
		if openErr != nil || directory == nil {
			return true, settleAmbiguousPublication(
				pin,
				openErr,
				errors.Join(closeDirectory(directory), closeEntry(reference)),
			)
		}
		matches, matchErr := current.EntryMatches(component, reference)
		if matchErr != nil || !matches {
			return true, settleAmbiguousPublication(
				pin,
				matchErr,
				errors.Join(directory.Close(), reference.Close()),
			)
		}
		pin.lineage = append(pin.lineage, publicationLineageEntry{
			parent: current, name: component, reference: reference, directory: directory,
		})
		current = directory
		pin.parent = current
	}
	return false, nil
}

func pinPublicationLeaf(
	ctx context.Context,
	checkpoint resumeauthority.PinnedCheckpoint,
	record checkpointmodel.Record,
	pin *publicationPin,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	current := pin.parent
	kind, exact, err := current.ClassifyExactEntry(pin.leaf)
	if err != nil {
		return err
	}
	switch {
	case kind == outputcap.EntryAbsent:
		pin.initial = resumeauthority.EvidenceAbsent
		return nil
	case !exact || kind != outputcap.EntryRegularFile:
		pin.initial = resumeauthority.EvidenceAmbiguous
		return nil
	}

	finalRef, err := current.OpenEntry(pin.leaf)
	if err != nil || finalRef == nil || finalRef.Kind() != outputcap.EntryRegularFile {
		return settleAmbiguousPublication(pin, err, closeEntry(finalRef))
	}
	matches, err := current.EntryMatches(pin.leaf, finalRef)
	if err != nil || !matches {
		return settleAmbiguousPublication(pin, err, closeEntry(finalRef))
	}
	final, err := current.OpenFile(pin.leaf, false, false)
	if err != nil || final == nil {
		return settleAmbiguousPublication(
			pin,
			err,
			errors.Join(closePublicationFile(final), closeEntry(finalRef)),
		)
	}
	matches, matchErr := current.EntryMatches(pin.leaf, finalRef)
	if matchErr != nil || !matches {
		return settleAmbiguousPublication(
			pin,
			matchErr,
			errors.Join(final.Close(), finalRef.Close()),
		)
	}
	evidence, compareErr := checkpoint.SameOwnedFile(ctx, final)
	if compareErr != nil {
		return errors.Join(compareErr, final.Close(), finalRef.Close())
	}
	if evidence == resumeauthority.EvidenceAbsent || !evidence.Valid() {
		evidence = resumeauthority.EvidenceAmbiguous
	}
	if evidence == resumeauthority.EvidenceExact {
		size, sizeErr := final.Size()
		if sizeErr != nil {
			return errors.Join(sizeErr, final.Close(), finalRef.Close())
		}
		if size != record.ExactSize() {
			evidence = resumeauthority.EvidenceReplaced
		}
	}
	pin.initial = evidence
	if evidence == resumeauthority.EvidenceExact {
		pin.finalRef, pin.final = finalRef, final
		return nil
	}
	return errors.Join(final.Close(), finalRef.Close())
}

func settleAmbiguousPublication(
	pin *publicationPin,
	observationErr error,
	closeErr error,
) error {
	if observationErr != nil && !publicationObservationChanged(observationErr) {
		return errors.Join(observationErr, closeErr)
	}
	if closeErr != nil {
		return closeErr
	}
	pin.initial = resumeauthority.EvidenceAmbiguous
	return nil
}

func (*PublicationObserver) finishPublicationPin(
	pin *publicationPin,
	recordID checkpointmodel.RecordID,
) (resumeauthority.PinnedPublication, error) {
	observation, err := resumeauthority.NewPublicationObservation(recordID, pin.initial)
	if err != nil {
		return nil, err
	}
	if observation.RecordID() != recordID {
		return nil, ErrInvalidClaim
	}
	return pin, nil
}

func publicationObservationChanged(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, outputcap.ErrUnsafeNamespace) ||
		errors.Is(err, ErrRetainedAuthorityChanged)
}

func (pin *publicationPin) Observation() resumeauthority.PublicationObservation {
	if pin == nil {
		return resumeauthority.PublicationObservation{}
	}
	observation, _ := resumeauthority.NewPublicationObservation(pin.recordID, pin.initial)
	return observation
}

func (pin *publicationPin) Revalidate(ctx context.Context) (resumeauthority.Evidence, error) {
	if pin == nil || ctx == nil {
		return resumeauthority.EvidenceAmbiguous, ErrInvalidClaim
	}
	if err := ctx.Err(); err != nil {
		return resumeauthority.EvidenceAmbiguous, err
	}
	pin.mu.Lock()
	defer pin.mu.Unlock()
	return pin.revalidateLocked(ctx)
}

func (pin *publicationPin) revalidateLocked(
	ctx context.Context,
) (resumeauthority.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return resumeauthority.EvidenceAmbiguous, err
	}
	if pin.closed || pin.guard == nil || pin.root == nil || pin.parent == nil {
		return resumeauthority.EvidenceAmbiguous, ErrAuthorityClosed
	}
	unchanged, err := publicationLineageUnchanged(pin.lineage)
	if err != nil {
		return resumeauthority.EvidenceAmbiguous, err
	}
	if !unchanged {
		return resumeauthority.EvidenceAmbiguous, nil
	}
	if pin.missing != nil {
		return revalidateMissingPublication(pin.missing)
	}
	if pin.initial == resumeauthority.EvidenceAbsent {
		return classifyChangedFinal(pin.parent, pin.leaf)
	}
	if pin.initial != resumeauthority.EvidenceExact || pin.finalRef == nil || pin.final == nil {
		return pin.initial, nil
	}
	return pin.revalidateExactPublication()
}

func publicationLineageUnchanged(lineage []publicationLineageEntry) (bool, error) {
	for _, entry := range lineage {
		matches, err := entry.parent.EntryMatches(entry.name, entry.reference)
		if err != nil {
			return false, err
		}
		if !matches {
			return false, nil
		}
	}
	return true, nil
}

func revalidateMissingPublication(
	missing *publicationMissingEntry,
) (resumeauthority.Evidence, error) {
	kind, exact, err := missing.parent.ClassifyExactEntry(missing.name)
	if err != nil {
		return resumeauthority.EvidenceAmbiguous, err
	}
	if exact && kind == outputcap.EntryAbsent {
		return resumeauthority.EvidenceAbsent, nil
	}
	return resumeauthority.EvidenceAmbiguous, nil
}

func (pin *publicationPin) revalidateExactPublication() (resumeauthority.Evidence, error) {
	matches, err := pin.parent.EntryMatches(pin.leaf, pin.finalRef)
	if err != nil {
		return resumeauthority.EvidenceAmbiguous, err
	}
	if !matches {
		return classifyChangedFinal(pin.parent, pin.leaf)
	}
	current, err := pin.parent.OpenFile(pin.leaf, false, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return resumeauthority.EvidenceAbsent, nil
		}
		return resumeauthority.EvidenceAmbiguous, err
	}
	if current == nil {
		return resumeauthority.EvidenceAmbiguous, nil
	}
	same, sameErr := current.SameFile(pin.final)
	closeErr := current.Close()
	matches, matchErr := pin.parent.EntryMatches(pin.leaf, pin.finalRef)
	size, sizeErr := pin.final.Size()
	if sameErr != nil || closeErr != nil || matchErr != nil || sizeErr != nil {
		return resumeauthority.EvidenceAmbiguous,
			errors.Join(sameErr, closeErr, matchErr, sizeErr)
	}
	if !same || !matches || size != pin.exactSize {
		return resumeauthority.EvidenceReplaced, nil
	}
	return resumeauthority.EvidenceExact, nil
}

func classifyChangedFinal(
	parent outputcap.Directory,
	name string,
) (resumeauthority.Evidence, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil {
		return resumeauthority.EvidenceAmbiguous, err
	}
	if exact && kind == outputcap.EntryAbsent {
		return resumeauthority.EvidenceAbsent, nil
	}
	if exact && kind == outputcap.EntryRegularFile {
		return resumeauthority.EvidenceReplaced, nil
	}
	return resumeauthority.EvidenceAmbiguous, nil
}

func (pin *publicationPin) Close() error {
	if pin == nil {
		return nil
	}
	pin.mu.Lock()
	defer pin.mu.Unlock()
	if pin.closed {
		return pin.closeErr
	}
	pin.closed = true
	errs := []error{closePublicationFile(pin.final), closeEntry(pin.finalRef)}
	for index := range slices.Backward(pin.lineage) {
		entry := pin.lineage[index]
		errs = append(errs, closeDirectory(entry.directory), closeEntry(entry.reference))
	}
	if pin.guard != nil {
		errs = append(errs, pin.guard.Close())
	}
	pin.closeErr = errors.Join(errs...)
	return pin.closeErr
}

func closePublicationFile(file outputcap.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

var _ resumeauthority.PublicationObserver = (*PublicationObserver)(nil)
var _ resumeauthority.PinnedPublication = (*publicationPin)(nil)
