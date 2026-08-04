package catalogflow

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/windshare/windshare/core/catalog"
)

var ErrPageCursorState = errors.New("catalog page cursor state is invalid")

// OpenDirectoryPages creates a forward-only generation cursor. Unlike
// LoadDirectory, it verifies and releases each sender object before requesting
// the next page, so a protocol-wide directory never becomes one Go allocation.
func (c *Client) OpenDirectoryPages(
	ctx context.Context,
	directory catalog.DirectoryID,
) (catalog.DirectoryPageCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if directory.IsZero() {
		return nil, ErrInvalidRequest
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return nil, ErrClientClosed
	}
	return &pageCursor{client: c, directory: directory}, nil
}

type pageCursor struct {
	client    *Client
	directory catalog.DirectoryID

	mu         sync.Mutex
	generation catalog.DirectoryGeneration
	previous   catalog.PageCommitment
	nextIndex  uint32
	terminal   bool
	closed     bool
	inflight   bool
	cancel     context.CancelFunc
}

func (cursor *pageCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	if err := ctx.Err(); err != nil {
		return catalog.CatalogPage{}, false, err
	}
	cursor.mu.Lock()
	if cursor.closed {
		cursor.mu.Unlock()
		return catalog.CatalogPage{}, false, ErrClientClosed
	}
	if cursor.inflight {
		cursor.mu.Unlock()
		return catalog.CatalogPage{}, false, ErrPageCursorState
	}
	if cursor.terminal {
		cursor.mu.Unlock()
		return catalog.CatalogPage{}, false, nil
	}
	if cursor.nextIndex >= cursor.client.maxPages {
		cursor.mu.Unlock()
		return catalog.CatalogPage{}, false, ErrClientBudget
	}
	var generation *catalog.DirectoryGeneration
	if cursor.nextIndex > 0 {
		copy := cursor.generation
		generation = &copy
	}
	request, err := NewListRequest(cursor.directory, generation, cursor.nextIndex)
	if err != nil {
		cursor.mu.Unlock()
		return catalog.CatalogPage{}, false, err
	}
	fetchContext, cancel := context.WithCancel(ctx)
	cursor.inflight = true
	cursor.cancel = cancel
	cursor.mu.Unlock()

	page, fetchErr := cursor.client.fetchCursorPage(fetchContext, request, cursor, cancel)
	cancel()

	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	cursor.inflight = false
	cursor.cancel = nil
	if fetchErr != nil {
		return catalog.CatalogPage{}, false, fetchErr
	}
	if cursor.closed {
		return catalog.CatalogPage{}, false, ErrClientClosed
	}
	if page.PageIndex() != cursor.nextIndex ||
		(cursor.nextIndex == 0 && !page.Previous().IsZero()) ||
		(cursor.nextIndex > 0 && (page.Generation() != cursor.generation || page.Previous() != cursor.previous)) {
		return catalog.CatalogPage{}, false, ErrPageConflict
	}
	if cursor.nextIndex == 0 {
		cursor.generation = page.Generation()
	}
	cursor.previous = page.Commitment()
	cursor.nextIndex++
	cursor.terminal = page.Terminal()
	return page, true, nil
}

func (cursor *pageCursor) Close() error {
	cursor.mu.Lock()
	if cursor.closed {
		cursor.mu.Unlock()
		return nil
	}
	cursor.closed = true
	cancel := cursor.cancel
	cursor.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (c *Client) fetchCursorPage(
	ctx context.Context,
	request ListRequest,
	cursor *pageCursor,
	cancel context.CancelFunc,
) (catalog.CatalogPage, error) {
	if err := c.beginCursorFetch(cursor, cancel); err != nil {
		return catalog.CatalogPage{}, err
	}
	defer c.endCursorFetch(cursor)
	objectBytes, err := c.transport.FetchPage(ctx, request)
	if err != nil {
		return catalog.CatalogPage{}, err
	}
	if len(objectBytes) == 0 || uint64(len(objectBytes)) > uint64(c.maxObjectBytes) {
		return catalog.CatalogPage{}, fmt.Errorf(
			"%w: catalog object length %d", ErrClientBudget, len(objectBytes),
		)
	}
	verified, err := c.verifier.Verify(ctx, c.shareInstance, request, objectBytes)
	if err != nil {
		return catalog.CatalogPage{}, fmt.Errorf("verify catalog object: %w", err)
	}
	if err := validateVerifiedResponse(c.shareInstance, request, verified); err != nil {
		return catalog.CatalogPage{}, err
	}
	if verified.Failure != nil {
		return catalog.CatalogPage{}, *verified.Failure
	}
	return verified.Page, nil
}

func (c *Client) beginCursorFetch(cursor *pageCursor, cancel context.CancelFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return ErrClientClosed
	}
	if len(c.inflight)+c.activeCursorFetches >= c.maxConcurrent {
		return ErrClientBudget
	}
	c.activeCursorFetches++
	c.cursorFetchCancels[cursor] = cancel
	c.loads.Add(1)
	return nil
}

func (c *Client) endCursorFetch(cursor *pageCursor) {
	c.mu.Lock()
	c.activeCursorFetches--
	delete(c.cursorFetchCancels, cursor)
	c.mu.Unlock()
	c.loads.Done()
}
