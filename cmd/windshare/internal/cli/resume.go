package cli

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/windshare/windshare/core/osfs"
)

const maxResumeDiscardConfirmationBytes = 64

var (
	errResumeConfirmationTooLong = errors.New("resume discard confirmation is too long")
	errResumeItemUnavailable     = errors.New("resume inventory item is unavailable")
)

type resumeListRequest struct {
	rootPath string
}

type resumeDiscardRequest struct {
	rootPath string
	item     uint64
}

// resumeStateSource keeps the CLI independent from native inventory mechanics.
// The production inventory remains the sole owner of the unforgeable core
// references; tests can exercise the destructive workflow without inventing one.
type resumeStateSource interface {
	Open(context.Context, string) (resumeStateInventory, error)
}

type resumeStateInventory interface {
	Items() []resumeStateItem
	Discard(context.Context, resumeStateItem) (osfs.DiscardSettlement, error)
	Close() error
}

type resumeStateAuthority struct {
	reference osfs.ResumeStateRef
}

type resumeStateItem struct {
	number         uint64
	kind           osfs.ResumeStateKind
	lifecycle      osfs.ResumeSessionLifecycle
	resumeIntent   string
	sessionID      string
	fileRecords    uint64
	allocatedBytes uint64
	attention      []osfs.ResumeAttention
	authority      *resumeStateAuthority
}

type resumeStateDiscardFunc func(
	context.Context,
	osfs.ResumeStateRef,
) (osfs.DiscardSettlement, error)

type resumeStateListFunc func(
	context.Context,
	osfs.FilesystemResumeRoot,
) (*osfs.ResumeStateInventory, error)

type filesystemResumeStateSource struct {
	list resumeStateListFunc
}

func (source filesystemResumeStateSource) Open(
	ctx context.Context,
	rootPath string,
) (resumeStateInventory, error) {
	list := source.list
	if list == nil {
		list = osfs.ListResumeState
	}
	inventory, err := list(ctx, osfs.FilesystemResumeRoot{RootPath: rootPath})
	if err != nil {
		return nil, err
	}
	if inventory == nil {
		return nil, errors.New("resume state backend returned no inventory")
	}
	return newFilesystemResumeStateInventory(
		inventory,
		inventory.Summaries(),
		osfs.DiscardResumeState,
	), nil
}

// filesystemResumeStateInventory deliberately captures the exact references
// returned by one live inventory. Item numbers are only a UI projection and can
// never be converted into authority after this inventory closes.
type filesystemResumeStateInventory struct {
	inventory *osfs.ResumeStateInventory
	items     []resumeStateItem
	discard   resumeStateDiscardFunc
}

func newFilesystemResumeStateInventory(
	inventory *osfs.ResumeStateInventory,
	summaries []osfs.ResumeStateSummary,
	discard resumeStateDiscardFunc,
) *filesystemResumeStateInventory {
	items := make([]resumeStateItem, len(summaries))
	for index, summary := range summaries {
		items[index] = resumeStateItem{
			number:         uint64(index + 1),
			kind:           summary.Reference.Kind(),
			lifecycle:      summary.Lifecycle,
			resumeIntent:   resumeIdentity(summary.Reference.ResumeIntent().Bytes()),
			sessionID:      resumeIdentity(summary.Reference.SessionID().Bytes()),
			fileRecords:    summary.FileRecords,
			allocatedBytes: summary.AllocatedBytes,
			attention:      append([]osfs.ResumeAttention(nil), summary.Attention...),
			authority:      &resumeStateAuthority{reference: summary.Reference},
		}
	}
	return &filesystemResumeStateInventory{inventory: inventory, items: items, discard: discard}
}

func (inventory *filesystemResumeStateInventory) Items() []resumeStateItem {
	if inventory == nil || inventory.inventory == nil {
		return nil
	}
	items := append([]resumeStateItem(nil), inventory.items...)
	for index := range items {
		items[index].attention = append([]osfs.ResumeAttention(nil), items[index].attention...)
	}
	return items
}

func (inventory *filesystemResumeStateInventory) Discard(
	ctx context.Context,
	item resumeStateItem,
) (osfs.DiscardSettlement, error) {
	if inventory == nil || inventory.inventory == nil || inventory.discard == nil ||
		item.number == 0 || item.number > uint64(len(inventory.items)) {
		return osfs.DiscardSettlement{}, errResumeItemUnavailable
	}
	captured := inventory.items[item.number-1]
	if item.authority == nil || item.authority != captured.authority {
		return osfs.DiscardSettlement{}, errResumeItemUnavailable
	}
	// Core consumes a reference on every discard attempt, including a failed
	// attempt. Clear the adapter's handle first so no test double or future
	// backend can accidentally make stale authority retryable.
	inventory.items[item.number-1].authority = nil
	// Passing the captured value directly is essential: ResumeStateRef is a live,
	// single-use capability and no serialized identifier can recreate it.
	return inventory.discard(ctx, captured.authority.reference)
}

func (inventory *filesystemResumeStateInventory) Close() error {
	if inventory == nil || inventory.inventory == nil {
		return nil
	}
	owned := inventory.inventory
	inventory.inventory = nil
	inventory.items = nil
	inventory.discard = nil
	return owned.Close()
}

type resumeInventoryLease struct {
	inventory resumeStateInventory
	closed    bool
}

func (lease *resumeInventoryLease) Close() error {
	if lease == nil || lease.closed || lease.inventory == nil {
		return nil
	}
	lease.closed = true
	return lease.inventory.Close()
}

func (a *App) runResume(ctx context.Context, args []string) int {
	if len(args) == 0 {
		a.logf("resume: exactly one action is required")
		a.resumeUsage()
		return ExitUsage
	}
	switch args[0] {
	case "list":
		return a.runResumeList(ctx, args[1:])
	case "discard":
		return a.runResumeDiscard(ctx, args[1:])
	case "help", "-h", "--help":
		a.resumeUsage()
		return ExitOK
	default:
		a.logf("resume: unknown action %q", args[0])
		a.resumeUsage()
		return ExitUsage
	}
}

func (a *App) runResumeList(ctx context.Context, args []string) (code int) {
	request, code := a.parseResumeListRequest(args)
	if code != ExitOK {
		return code
	}
	inventory, err := a.resumeStates().Open(ctx, request.rootPath)
	if err != nil {
		a.logf("resume list: inspect output root %q: %v", request.rootPath, err)
		return ExitFailure
	}
	if inventory == nil {
		a.logf("resume list: output backend returned no inventory")
		return ExitFailure
	}
	lease := &resumeInventoryLease{inventory: inventory}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			a.logf("resume list: close inventory: %v", closeErr)
			code = ExitFailure
		}
	}()

	rendered, err := renderResumeStateItems(inventory.Items())
	if err != nil {
		a.logf("resume list: render inventory: %v", err)
		return ExitFailure
	}
	// Release native pins before publishing a read-only snapshot. A close failure
	// means the operation did not settle cleanly, so it must not look successful.
	if err := lease.Close(); err != nil {
		a.logf("resume list: close inventory: %v", err)
		return ExitFailure
	}
	if err := a.writeResumeOutput(rendered); err != nil {
		a.logf("resume list: write inventory: %v", err)
		return ExitFailure
	}
	return ExitOK
}

func (a *App) runResumeDiscard(ctx context.Context, args []string) (code int) {
	request, code := a.parseResumeDiscardRequest(args)
	if code != ExitOK {
		return code
	}
	if !a.resumeDiscardStreamsAreInteractive() {
		a.logf("resume discard: confirmation requires an interactive terminal; nothing was removed")
		return ExitFailure
	}
	inventory, err := a.resumeStates().Open(ctx, request.rootPath)
	if err != nil {
		a.logf("resume discard: inspect output root %q: %v", request.rootPath, err)
		return ExitFailure
	}
	if inventory == nil {
		a.logf("resume discard: output backend returned no inventory")
		return ExitFailure
	}
	lease := &resumeInventoryLease{inventory: inventory}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			a.logf("resume discard: close inventory: %v", closeErr)
			code = ExitFailure
		}
	}()

	item, found := findResumeStateItem(inventory.Items(), request.item)
	if !found {
		a.logf("resume discard: item %d is not present in the current inventory", request.item)
		return ExitUsage
	}
	preview, err := renderResumeStateItems([]resumeStateItem{item})
	if err != nil {
		a.logf("resume discard: render item %d: %v", request.item, err)
		return ExitFailure
	}
	if err := a.writeResumePrompt("Selected recovery state:\n" + preview); err != nil {
		a.logf("resume discard: write item preview: %v", err)
		return ExitFailure
	}
	expected := fmt.Sprintf("discard %d", request.item)
	prompt := fmt.Sprintf("Type %q to remove only this recovery state: ", expected)
	if err := a.writeResumePrompt(prompt); err != nil {
		a.logf("resume discard: write confirmation prompt: %v", err)
		return ExitFailure
	}
	confirmation, err := readResumeDiscardConfirmation(ctx, a.Stdin)
	if err != nil {
		a.logf("resume discard: read confirmation: %v", err)
		return ExitFailure
	}
	if confirmation != expected {
		a.logf("resume discard: confirmation did not match; nothing was removed")
		return ExitUsage
	}
	if err := ctx.Err(); err != nil {
		a.logf("resume discard: confirmation was canceled: %v", err)
		return ExitFailure
	}

	settlement, err := inventory.Discard(ctx, item)
	if err != nil {
		// Core consumes a ResumeStateRef on every attempted discard. Exiting forces
		// the caller to obtain a fresh inventory rather than retry stale authority.
		a.logf("resume discard: remove item %d: %v; run resume list again before retrying", request.item, err)
		return ExitFailure
	}
	if err := lease.Close(); err != nil {
		switch settlement.Kind {
		case osfs.Discarded:
			a.logf(
				"resume discard: item %d was discarded (%d internal allocated bytes), but closing the remaining inventory failed: %v; do not reuse this item number; run resume list again before any further discard",
				request.item,
				settlement.RemovedBytes,
				err,
			)
		case osfs.DiscardAlreadyAbsent:
			a.logf(
				"resume discard: item %d was already absent, but closing the remaining inventory failed: %v; do not reuse this item number; run resume list again before any further discard",
				request.item,
				err,
			)
		default:
			a.logf("resume discard: close inventory after an invalid settlement: %v", err)
		}
		return ExitFailure
	}
	switch settlement.Kind {
	case osfs.Discarded:
		if err := a.writeResumeOutput(fmt.Sprintf(
			"Discarded recovery item %d (%d internal allocated bytes).\n",
			request.item,
			settlement.RemovedBytes,
		)); err != nil {
			a.logf("resume discard: state was discarded but settlement output failed: %v", err)
			return ExitFailure
		}
	case osfs.DiscardAlreadyAbsent:
		if err := a.writeResumeOutput(fmt.Sprintf("Recovery item %d was already absent.\n", request.item)); err != nil {
			a.logf("resume discard: state was already absent but settlement output failed: %v", err)
			return ExitFailure
		}
	default:
		a.logf("resume discard: core returned an invalid settlement")
		return ExitFailure
	}
	return ExitOK
}

func (a *App) parseResumeListRequest(args []string) (resumeListRequest, int) {
	flags := a.newFlagSet("resume list")
	rootPath := flags.String("o", ".", "output directory")
	positional, err := parseInterleaved(flags, args)
	if err != nil {
		return resumeListRequest{}, ExitUsage
	}
	if len(positional) != 0 {
		a.logf("resume list: positional arguments are not accepted")
		return resumeListRequest{}, ExitUsage
	}
	absolute, err := absoluteResumeRoot(*rootPath)
	if err != nil {
		a.logf("resume list: output directory %q is invalid: %v", *rootPath, err)
		return resumeListRequest{}, ExitUsage
	}
	return resumeListRequest{rootPath: absolute}, ExitOK
}

func (a *App) parseResumeDiscardRequest(args []string) (resumeDiscardRequest, int) {
	flags := a.newFlagSet("resume discard")
	rootPath := flags.String("o", "", "output directory (required)")
	itemText := flags.String("item", "", "current inventory item number (required)")
	positional, err := parseInterleaved(flags, args)
	if err != nil {
		return resumeDiscardRequest{}, ExitUsage
	}
	if len(positional) != 0 {
		a.logf("resume discard: positional arguments are not accepted")
		return resumeDiscardRequest{}, ExitUsage
	}
	if *rootPath == "" {
		a.logf("resume discard: -o is required")
		return resumeDiscardRequest{}, ExitUsage
	}
	if *itemText == "" {
		a.logf("resume discard: --item is required")
		return resumeDiscardRequest{}, ExitUsage
	}
	item, err := strconv.ParseUint(*itemText, 10, 64)
	if err != nil || item == 0 || strconv.FormatUint(item, 10) != *itemText {
		a.logf("resume discard: --item must be a positive decimal inventory number")
		return resumeDiscardRequest{}, ExitUsage
	}
	absolute, err := absoluteResumeRoot(*rootPath)
	if err != nil {
		a.logf("resume discard: output directory %q is invalid: %v", *rootPath, err)
		return resumeDiscardRequest{}, ExitUsage
	}
	return resumeDiscardRequest{rootPath: absolute, item: item}, ExitOK
}

func (a *App) resumeStates() resumeStateSource {
	if a.resumeSource != nil {
		return a.resumeSource
	}
	return filesystemResumeStateSource{}
}

func (a *App) resumeUsage() {
	fmt.Fprint(a.stderrWriter(), `Usage:
	  windshare resume list [-o <directory>]
	  windshare resume discard -o <directory> --item <number>
`)
}

func (a *App) resumeDiscardStreamsAreInteractive() bool {
	if a == nil {
		return false
	}
	if a.resumeInteractive != nil {
		return a.resumeInteractive(a.Stdin, a.Stderr)
	}
	input, ok := a.Stdin.(*os.File)
	if !ok {
		return false
	}
	prompt, ok := a.Stderr.(*os.File)
	return ok && resumeFileIsTerminal(input) && resumeFileIsTerminal(prompt)
}

func (a *App) writeResumeOutput(value string) error {
	if a == nil || a.Stdout == nil {
		return errors.New("standard output is unavailable")
	}
	written, err := io.WriteString(a.Stdout, value)
	if err == nil && written != len(value) {
		return io.ErrShortWrite
	}
	return err
}

func (a *App) writeResumePrompt(value string) error {
	if a == nil || a.Stderr == nil {
		return errors.New("standard error is unavailable")
	}
	written, err := io.WriteString(a.stderrWriter(), value)
	if err == nil && written != len(value) {
		return io.ErrShortWrite
	}
	return err
}

func absoluteResumeRoot(rootPath string) (string, error) {
	if rootPath == "" {
		return "", errors.New("output directory is empty")
	}
	return filepath.Abs(rootPath)
}

func findResumeStateItem(items []resumeStateItem, number uint64) (resumeStateItem, bool) {
	for _, item := range items {
		if item.number == number {
			return item, true
		}
	}
	return resumeStateItem{}, false
}

func renderResumeStateItems(items []resumeStateItem) (string, error) {
	if len(items) == 0 {
		return "No recovery state found.\n", nil
	}
	var output strings.Builder
	for _, item := range items {
		if item.number == 0 {
			return "", errResumeItemUnavailable
		}
		fmt.Fprintf(
			&output,
			"item=%d kind=%q lifecycle=%q files=%d allocated_bytes=%d intent=%q session=%q\n",
			item.number,
			resumeStateKindName(item.kind),
			resumeLifecycleName(item.lifecycle),
			item.fileRecords,
			item.allocatedBytes,
			item.resumeIntent,
			item.sessionID,
		)
		for _, attention := range item.attention {
			fmt.Fprintf(
				&output,
				"  attention scope=%q code=%q state=%q detail=%q\n",
				resumeAttentionScopeName(attention.Scope),
				attention.Code,
				attention.State,
				attention.Detail,
			)
		}
	}
	return output.String(), nil
}

func resumeIdentity(raw []byte) string {
	allZero := true
	for _, value := range raw {
		if value != 0 {
			allZero = false
			break
		}
	}
	if len(raw) == 0 || allZero {
		return "-"
	}
	return hex.EncodeToString(raw)
}

func resumeStateKindName(kind osfs.ResumeStateKind) string {
	switch kind {
	case osfs.ResumeStateRecoverable:
		return "recoverable"
	case osfs.ResumeStateNeedsAttention:
		return "needs-attention"
	case osfs.ResumeStateLegacyUntrusted:
		return "legacy-untrusted"
	case osfs.ResumeStateOpaqueUnsafe:
		return "opaque-unsafe"
	default:
		return "unknown"
	}
}

func resumeLifecycleName(lifecycle osfs.ResumeSessionLifecycle) string {
	switch lifecycle {
	case osfs.ResumeSessionActive,
		osfs.ResumeSessionPausing,
		osfs.ResumeSessionPaused,
		osfs.ResumeSessionPausedNeedsAttention,
		osfs.ResumeSessionCompleting,
		osfs.ResumeSessionDiscarding:
		return lifecycle.String()
	default:
		return "unknown"
	}
}

func resumeAttentionScopeName(scope osfs.ResumeAttentionScope) string {
	switch scope {
	case osfs.ResumeAttentionFile:
		return "file"
	case osfs.ResumeAttentionIntent:
		return "intent"
	case osfs.ResumeAttentionRoot:
		return "root"
	case osfs.ResumeAttentionLegacy:
		return "legacy"
	default:
		return "unknown"
	}
}

type resumeConfirmationResult struct {
	line string
	err  error
}

func readResumeDiscardConfirmation(ctx context.Context, input io.Reader) (string, error) {
	if input == nil {
		return "", errors.New("confirmation input is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	result := make(chan resumeConfirmationResult, 1)
	go func() {
		line, err := readBoundedResumeConfirmation(input)
		result <- resumeConfirmationResult{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case completed := <-result:
		if err := ctx.Err(); err != nil {
			return "", err
		}
		return completed.line, completed.err
	}
}

func readBoundedResumeConfirmation(input io.Reader) (string, error) {
	reader := bufio.NewReaderSize(input, maxResumeDiscardConfirmationBytes)
	raw, err := reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errResumeConfirmationTooLong
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if len(raw) > maxResumeDiscardConfirmationBytes {
		return "", errResumeConfirmationTooLong
	}
	if len(raw) != 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
		if len(raw) != 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
	}
	return string(raw), nil
}
