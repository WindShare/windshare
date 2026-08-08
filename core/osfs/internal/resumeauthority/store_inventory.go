package resumeauthority

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/checkpointstore"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
)

const (
	resumeAdapterAttentionDomain = "windshare/checkpoint-resume-adapter-attention/v1"
	resumeInventoryLimit         = checkpointmodel.MaxCheckpointRecordsPerIntent + 1
)

var (
	errResumePinReplaced      = errors.New("checkpoint resume pin was replaced")
	errResumeOwnershipBinding = errors.New("checkpoint resume ownership binding is not exact")
	errResumeNamespaceAbsent  = errors.New("checkpoint resume namespace is absent")
	errResumeUnknownChildren  = errors.New("checkpoint resume namespace exceeds its inspection bound")
)

// ResumeRepository is the checkpointstore implementation of the consumer-owned
// Repository port. CertifiedConfig.Root is borrowed; each inventory pins
// and owns its own duplicate of the certified root capability.
type ResumeRepository struct {
	config checkpointstore.CertifiedConfig
}

// NewResumeRepository borrows the certified root only as a capability factory;
// each live inventory duplicates and pins its own exact native lineage.
func NewResumeRepository(config checkpointstore.CertifiedConfig) (ResumeRepository, error) {
	if err := validateStoreConfig(config); err != nil {
		return ResumeRepository{}, err
	}
	return ResumeRepository{config: config}, nil
}

func (repository ResumeRepository) ListResumeState(
	ctx context.Context,
) (PinnedInventory, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	root, rootAttention, err := openResumeRoot(repository.config)
	if err != nil {
		if errors.Is(err, errResumeNamespaceAbsent) {
			return newResumeInventory(repository.config, nil), nil
		}
		if reason, attentionState := resumeOpenAttention(err); attentionState {
			return newUnavailableResumeInventory(repository.config, reason), nil
		}
		return nil, projectResumeError("list certified namespace", err)
	}

	inventory := newResumeInventory(repository.config, root)
	if err := inventory.list(ctx, rootAttention); err != nil {
		if errors.Is(err, errResumeUnknownChildren) {
			closeErr := inventory.Close()
			if closeErr != nil {
				return nil, closeErr
			}
			return newUnavailableResumeInventory(
				repository.config, AttentionUnknownChildren,
			), nil
		}
		return nil, errors.Join(err, inventory.Close())
	}
	return inventory, nil
}

type resumeInventoryItem struct {
	name      string
	intent    transfer.TransferIntentDigest
	pin       outputcap.CurrentEntryReference
	state     ListedState
	attention []Attention
}

type resumeInventory struct {
	mu        sync.Mutex
	cond      *sync.Cond
	config    checkpointstore.CertifiedConfig
	root      *resumeRootPins
	items     []resumeInventoryItem
	entries   []ListedState
	acquiring uint64
	closing   bool
	closed    bool
	closeErr  error
}

func newResumeInventory(config checkpointstore.CertifiedConfig, root *resumeRootPins) *resumeInventory {
	inventory := &resumeInventory{config: config, root: root}
	inventory.cond = sync.NewCond(&inventory.mu)
	return inventory
}

func newUnavailableResumeInventory(
	config checkpointstore.CertifiedConfig,
	reason AttentionReason,
) *resumeInventory {
	inventory := newResumeInventory(config, nil)
	attention := resumeAdapterAttention(reason, config.Ownership.CanonicalBytes())
	state, _ := NewListedState(ListedStateSpec{
		Status:    ListNeedsAttention,
		Backend:   config.Ownership.Backend(),
		Attention: []Attention{attention},
	})
	inventory.items = []resumeInventoryItem{{state: state}}
	inventory.entries = []ListedState{state}
	return inventory
}

func (inventory *resumeInventory) list(
	ctx context.Context,
	rootAttention []Attention,
) error {
	if inventory == nil || inventory.root == nil || inventory.root.intents == nil {
		return projectResumeError("list intent entries", transfer.ErrInvalidOutputBinding)
	}
	names, err := inventory.root.intents.Names(resumeInventoryLimit)
	if err != nil {
		return projectResumeError("list intent entries", err)
	}
	if len(names) >= resumeInventoryLimit {
		return errResumeUnknownChildren
	}
	slices.Sort(names)
	aliasAttention := make(map[transfer.TransferIntentDigest][]Attention)
	for _, name := range names {
		intent, decodable := decodeIntentNamespaceName(name)
		_, canonical := parseIntentNamespaceName(name)
		if decodable && !canonical {
			aliasAttention[intent] = append(aliasAttention[intent], resumeAdapterAttention(
				AttentionCorruptBinding, []byte(name),
			))
		}
	}
	for _, name := range names {
		if err := contextErr(ctx); err != nil {
			return err
		}
		intent, canonical := parseIntentNamespaceName(name)
		attention := slices.Clone(rootAttention)
		if canonical {
			attention = append(attention, aliasAttention[intent]...)
		}
		item, err := inventory.pinListedIntent(name, attention)
		if err != nil {
			return err
		}
		inventory.items = append(inventory.items, item)
		inventory.entries = append(inventory.entries, item.state)
	}
	if len(inventory.entries) == 0 && len(rootAttention) > 0 {
		state, err := NewListedState(ListedStateSpec{
			Status:    ListNeedsAttention,
			Backend:   inventory.config.Ownership.Backend(),
			Attention: slices.Clone(rootAttention),
		})
		if err != nil {
			return projectResumeError("project namespace attention", err)
		}
		inventory.items = append(inventory.items, resumeInventoryItem{state: state})
		inventory.entries = append(inventory.entries, state)
	}
	return nil
}

func (inventory *resumeInventory) pinListedIntent(
	name string,
	rootAttention []Attention,
) (resumeInventoryItem, error) {
	intent, canonical := parseIntentNamespaceName(name)
	kind, exact, err := inventory.root.intents.ClassifyExactEntry(name)
	if err != nil {
		return resumeInventoryItem{}, projectResumeError("classify intent entry", err)
	}
	pin, err := inventory.root.intents.OpenEntry(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			attention := append(slices.Clone(rootAttention),
				resumeAdapterAttention(AttentionReplacement, []byte(name)))
			return newResumeInventoryItem(
				name, intent, inventory.config.Ownership.Backend(), nil, false, attention,
			)
		}
		return resumeInventoryItem{}, projectResumeError("pin intent entry", err)
	}
	matches, matchErr := inventory.root.intents.EntryMatches(name, pin)
	if matchErr != nil {
		return resumeInventoryItem{}, errors.Join(
			projectResumeError("revalidate intent entry", matchErr), closeEntryReference(pin),
		)
	}
	attention := slices.Clone(rootAttention)
	available := canonical && exact && kind == outputcap.EntryDirectory &&
		pin.Kind() == outputcap.EntryDirectory && matches
	if !available {
		reason := AttentionUnknownChildren
		if canonical {
			reason = AttentionCorruptBinding
		}
		attention = append(attention, resumeAdapterAttention(reason, []byte(name)))
	}
	item, itemErr := newResumeInventoryItem(
		name, intent, inventory.config.Ownership.Backend(), pin, available, attention,
	)
	if itemErr != nil {
		return resumeInventoryItem{}, errors.Join(itemErr, closeEntryReference(pin))
	}
	return item, nil
}

func newResumeInventoryItem(
	name string,
	intent transfer.TransferIntentDigest,
	backend transfer.OutputBackendID,
	pin outputcap.CurrentEntryReference,
	available bool,
	attention []Attention,
) (resumeInventoryItem, error) {
	status := ListNeedsAttention
	if available && len(attention) == 0 {
		status = ListAvailable
	}
	if !available && len(attention) == 0 {
		attention = []Attention{
			resumeAdapterAttention(AttentionCorruptBinding, []byte(name)),
		}
	}
	if !available && intent.IsZero() {
		intent = transfer.TransferIntentDigest{}
	}
	state, err := NewListedState(ListedStateSpec{
		Status:  status,
		Intent:  intent,
		Backend: backend,
		// Child enumeration is deliberately deferred until Acquire holds this
		// intent's lease. Zero means "not inspected", never "proved empty".
		CheckpointRecordCount: 0,
		RecoveryArtifactBytes: 0,
		Attention:             attention,
	})
	if err != nil {
		return resumeInventoryItem{}, projectResumeError("project intent entry", err)
	}
	return resumeInventoryItem{
		name: name, intent: intent, pin: pin, state: state, attention: slices.Clone(attention),
	}, nil
}

func (inventory *resumeInventory) Entries() []ListedState {
	if inventory == nil {
		return nil
	}
	inventory.mu.Lock()
	defer inventory.mu.Unlock()
	return slices.Clone(inventory.entries)
}

func (inventory *resumeInventory) Acquire(
	ctx context.Context,
	index int,
) (LeasedRepository, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if inventory == nil {
		return nil, projectResumeError("acquire intent", transfer.ErrInvalidOutputBinding)
	}
	inventory.mu.Lock()
	if inventory.closing || inventory.closed || inventory.root == nil ||
		index < 0 || index >= len(inventory.items) {
		inventory.mu.Unlock()
		return nil, projectResumeError("acquire intent", transfer.ErrInvalidOutputBinding)
	}
	item := inventory.items[index]
	if item.intent.IsZero() || item.pin == nil {
		inventory.mu.Unlock()
		return nil, projectResumeError("acquire opaque intent", errResumeOwnershipBinding)
	}
	inventory.acquiring++
	root := inventory.root
	config := inventory.config
	inventory.mu.Unlock()

	leased, err := acquireResumeIntent(ctx, config, root, item)

	inventory.mu.Lock()
	inventory.acquiring--
	inventory.cond.Broadcast()
	inventory.mu.Unlock()
	return leased, err
}

func (inventory *resumeInventory) Close() error {
	if inventory == nil {
		return nil
	}
	inventory.mu.Lock()
	for inventory.closing && !inventory.closed {
		inventory.cond.Wait()
	}
	if inventory.closed {
		err := inventory.closeErr
		inventory.mu.Unlock()
		return err
	}
	inventory.closing = true
	for inventory.acquiring != 0 {
		inventory.cond.Wait()
	}
	items := inventory.items
	root := inventory.root
	inventory.root = nil
	inventory.mu.Unlock()

	closeErrors := make([]error, 0, len(items)+1)
	for _, item := range items {
		closeErrors = append(closeErrors, closeEntryReference(item.pin))
	}
	if root != nil {
		closeErrors = append(closeErrors, root.Close())
	}
	closeErr := projectResumeError("close resume inventory", errors.Join(closeErrors...))

	inventory.mu.Lock()
	inventory.closeErr = closeErr
	inventory.closed = true
	inventory.cond.Broadcast()
	inventory.mu.Unlock()
	return closeErr
}
