// Package resumeauthority reduces ordinary native operations after an exact
// operation lease has been acquired. It does not retain terminal history.
package resumeauthority

import (
	"bytes"
	"errors"
	"slices"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const MaximumDiagnosticReferenceBytes = 256

var (
	ErrInvalidContract = errors.New("resume authority contract is invalid")
	ErrBusy            = errors.New("resume authority operation is busy")
)

type OperationState uint8

const (
	OperationIncomplete OperationState = iota + 1
	OperationResumable
	OperationCleanupPending
	OperationNeedsAttention
	OperationDiscarded
)

func (state OperationState) Valid() bool {
	return state >= OperationIncomplete && state <= OperationDiscarded
}

func (state OperationState) String() string {
	switch state {
	case OperationIncomplete:
		return "incomplete"
	case OperationResumable:
		return "resumable"
	case OperationCleanupPending:
		return "cleanup-pending"
	case OperationNeedsAttention:
		return "operation-needs-attention"
	case OperationDiscarded:
		return "discarded"
	default:
		return ""
	}
}

type ItemState uint8

const (
	ItemIncomplete ItemState = iota + 1
	ItemResumable
	ItemPublished
	ItemFailed
	ItemBlocked
)

func (state ItemState) Valid() bool {
	return state >= ItemIncomplete && state <= ItemBlocked
}

func (state ItemState) String() string {
	switch state {
	case ItemIncomplete:
		return "incomplete"
	case ItemResumable:
		return "resumable"
	case ItemPublished:
		return "published"
	case ItemFailed:
		return "failed"
	case ItemBlocked:
		return "item-blocked"
	default:
		return ""
	}
}

type ItemBlockReason uint8

const (
	ItemBlockNone ItemBlockReason = iota + 1
	ItemBlockPublicationUnknown
	ItemBlockCheckpointInvalid
	ItemBlockOwnedObjectUnknown
)

func (reason ItemBlockReason) Valid() bool {
	return reason >= ItemBlockNone && reason <= ItemBlockOwnedObjectUnknown
}

func (reason ItemBlockReason) String() string {
	switch reason {
	case ItemBlockNone:
		return "none"
	case ItemBlockPublicationUnknown:
		return "publication-unknown"
	case ItemBlockCheckpointInvalid:
		return "checkpoint-invalid"
	case ItemBlockOwnedObjectUnknown:
		return "owned-object-unknown"
	default:
		return ""
	}
}

type Item struct {
	path      string
	state     ItemState
	reason    ItemBlockReason
	reference string
}

func NewItem(path string, state ItemState, reason ItemBlockReason) (Item, error) {
	canonical, err := catalog.CanonicalPath(path)
	if err != nil || canonical != path || path == "" || !state.Valid() || !reason.Valid() ||
		state == ItemBlocked && reason == ItemBlockNone ||
		state != ItemBlocked && reason != ItemBlockNone {
		return Item{}, errors.Join(ErrInvalidContract, err)
	}
	return Item{path: path, state: state, reason: reason}, nil
}

func NewBlockedReference(reference string) (Item, error) {
	if reference == "" || len(reference) > MaximumDiagnosticReferenceBytes {
		return Item{}, ErrInvalidContract
	}
	return Item{
		state: ItemBlocked, reason: ItemBlockCheckpointInvalid, reference: reference,
	}, nil
}

func (item Item) CanonicalPath() string        { return item.path }
func (item Item) State() ItemState             { return item.state }
func (item Item) BlockReason() ItemBlockReason { return item.reason }
func (item Item) Reference() string            { return item.reference }
func (item Item) Valid() bool {
	if item.reference != "" {
		return item.path == "" && item.state == ItemBlocked &&
			item.reason == ItemBlockCheckpointInvalid &&
			len(item.reference) <= MaximumDiagnosticReferenceBytes
	}
	rebuilt, err := NewItem(item.path, item.state, item.reason)
	return err == nil && rebuilt == item
}

type Header struct {
	record checkpointmodel.OrdinaryOperationRecord
}

func NewHeader(record checkpointmodel.OrdinaryOperationRecord) (Header, error) {
	if !record.Valid() {
		return Header{}, ErrInvalidContract
	}
	if _, err := record.VerifyIntent(transfer.DecodeReceiveIntent); err != nil {
		return Header{}, errors.Join(ErrInvalidContract, err)
	}
	return Header{record: record}, nil
}

func (header Header) Record() checkpointmodel.OrdinaryOperationRecord { return header.record }
func (header Header) Valid() bool {
	rebuilt, err := NewHeader(header.record)
	return err == nil && checkpointmodel.SameOrdinaryOperation(rebuilt.record, header.record) &&
		rebuilt.record.LifecycleGeneration() == header.record.LifecycleGeneration() &&
		rebuilt.record.Lifecycle() == header.record.Lifecycle() &&
		rebuilt.record.Lease() == header.record.Lease() &&
		rebuilt.record.ClosedReason() == header.record.ClosedReason()
}

type Snapshot struct {
	header Header
	items  []Item
}

func NewSnapshot(header Header, items []Item) (Snapshot, error) {
	if !header.Valid() {
		return Snapshot{}, ErrInvalidContract
	}
	paths := make(map[string]struct{}, len(items))
	references := make(map[string]struct{}, len(items))
	canonical := slices.Clone(items)
	for _, item := range canonical {
		if !item.Valid() {
			return Snapshot{}, ErrInvalidContract
		}
		if item.path != "" {
			if _, duplicate := paths[item.path]; duplicate {
				return Snapshot{}, ErrInvalidContract
			}
			paths[item.path] = struct{}{}
		} else {
			if _, duplicate := references[item.reference]; duplicate {
				return Snapshot{}, ErrInvalidContract
			}
			references[item.reference] = struct{}{}
		}
	}
	slices.SortFunc(canonical, compareItems)
	return Snapshot{header: header, items: canonical}, nil
}

func (snapshot Snapshot) Header() Header { return snapshot.header }
func (snapshot Snapshot) Items() []Item  { return slices.Clone(snapshot.items) }
func (snapshot Snapshot) Valid() bool {
	_, err := NewSnapshot(snapshot.header, snapshot.items)
	return err == nil
}

func compareItems(left, right Item) int {
	if compared := bytes.Compare([]byte(left.path), []byte(right.path)); compared != 0 {
		return compared
	}
	if compared := bytes.Compare([]byte(left.reference), []byte(right.reference)); compared != 0 {
		return compared
	}
	if left.state != right.state {
		return int(left.state) - int(right.state)
	}
	return int(left.reason) - int(right.reason)
}

type Summary struct {
	operationID receivecontract.OperationID
	intent      transfer.ReceiveIntentDigest
	generation  uint64
	state       OperationState
	reason      checkpointmodel.OrdinaryClosedReason
	items       []Item
	busy        bool
}

func newSummary(
	header Header,
	state OperationState,
	items []Item,
	busy bool,
) (Summary, error) {
	if !header.Valid() || !state.Valid() {
		return Summary{}, ErrInvalidContract
	}
	record := header.record
	if state == OperationNeedsAttention && !record.ClosedReason().IsAttentionReason() {
		return Summary{}, ErrInvalidContract
	}
	if state != OperationNeedsAttention && record.ClosedReason().IsAttentionReason() &&
		record.Lifecycle() != checkpointmodel.OrdinaryOperationDiscarded {
		return Summary{}, ErrInvalidContract
	}
	return Summary{
		operationID: record.OperationID(), intent: record.ReceiveIntentDigest(),
		generation: record.LifecycleGeneration(), state: state,
		reason: record.ClosedReason(), items: slices.Clone(items), busy: busy,
	}, nil
}

func (summary Summary) OperationID() receivecontract.OperationID          { return summary.operationID }
func (summary Summary) ReceiveIntentDigest() transfer.ReceiveIntentDigest { return summary.intent }
func (summary Summary) StateGeneration() uint64                           { return summary.generation }
func (summary Summary) State() OperationState                             { return summary.state }
func (summary Summary) NeedsAttentionReason() checkpointmodel.OrdinaryClosedReason {
	return summary.reason
}
func (summary Summary) Items() []Item { return slices.Clone(summary.items) }
func (summary Summary) Busy() bool    { return summary.busy }
func (summary Summary) Valid() bool {
	return !summary.operationID.IsZero() && !summary.intent.IsZero() && summary.generation > 0 &&
		summary.state.Valid() && (!summary.reason.IsAttentionReason() ||
		summary.state == OperationNeedsAttention || summary.state == OperationDiscarded)
}

type ListStatus uint8

const (
	ListReady ListStatus = iota + 1
	ListNeedsAttention
)

type Inventory struct {
	status    ListStatus
	summaries []Summary
	unknown   bool
}

func newInventory(summaries []Summary, unknown bool) Inventory {
	canonical := slices.Clone(summaries)
	slices.SortFunc(canonical, func(left, right Summary) int {
		return bytes.Compare(left.operationID.Bytes(), right.operationID.Bytes())
	})
	status := ListReady
	if unknown {
		status = ListNeedsAttention
	}
	for _, summary := range canonical {
		if summary.state == OperationNeedsAttention {
			status = ListNeedsAttention
			break
		}
	}
	return Inventory{status: status, summaries: canonical, unknown: unknown}
}

func (inventory Inventory) Status() ListStatus   { return inventory.status }
func (inventory Inventory) Summaries() []Summary { return slices.Clone(inventory.summaries) }
func (inventory Inventory) UnknownEntries() bool { return inventory.unknown }

type PageCursor struct {
	after receivecontract.OperationID
}

func NewPageCursor(after receivecontract.OperationID) PageCursor {
	return PageCursor{after: after}
}
func (cursor PageCursor) After() receivecontract.OperationID { return cursor.after }
func (cursor PageCursor) IsZero() bool                       { return cursor.after.IsZero() }

type Page struct {
	headers []Header
	next    PageCursor
	unknown bool
}

func NewPage(headers []Header, next PageCursor, unknown bool) (Page, error) {
	canonical := slices.Clone(headers)
	for _, header := range canonical {
		if !header.Valid() {
			return Page{}, ErrInvalidContract
		}
	}
	slices.SortFunc(canonical, func(left, right Header) int {
		return bytes.Compare(left.record.OperationID().Bytes(), right.record.OperationID().Bytes())
	})
	return Page{headers: canonical, next: next, unknown: unknown}, nil
}

func (page Page) Headers() []Header { return slices.Clone(page.headers) }
func (page Page) Next() PageCursor  { return page.next }
func (page Page) Unknown() bool     { return page.unknown }

type CleanupState uint8

const (
	CleanupComplete CleanupState = iota + 1
	CleanupPending
)

func (state CleanupState) Valid() bool {
	return state == CleanupComplete || state == CleanupPending
}
