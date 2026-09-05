package receivercontinuation

import (
	"context"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func (s *Session) OpenDirectoryPages(ctx context.Context, id catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
	cursor := &directoryCursor{owner: s, directory: id}
	if err := cursor.open(ctx); err != nil {
		return nil, err
	}
	return cursor, nil
}

type directoryCursor struct {
	owner      *Session
	directory  catalog.DirectoryID
	runtime    *sessionruntime.ReceiverRuntime
	cursor     catalog.DirectoryPageCursor
	count      uint32
	commitment catalog.PageCommitment
	closed     bool
}

func (c *directoryCursor) open(ctx context.Context) error {
	for {
		c.runtime = c.owner.Runtime()
		dependencies, err := c.runtime.TransferDependencies()
		if err != nil {
			return err
		}
		c.cursor, err = dependencies.OpenDirectoryPages(ctx, c.directory)
		if err == nil {
			return nil
		}
		if !c.runtime.AwaitPathRetirement(ctx) {
			return err
		}
		if err = c.owner.recover(ctx, c.runtime); err != nil {
			return err
		}
	}
}
func (c *directoryCursor) Next(ctx context.Context) (catalog.CatalogPage, bool, error) {
	if c.closed {
		return catalog.CatalogPage{}, false, sessionruntime.ErrRuntimeClosed
	}
	for {
		page, ok, err := c.cursor.Next(ctx)
		if err == nil {
			if ok {
				c.count++
				c.commitment = page.Commitment()
			}
			return page, ok, nil
		}
		if !c.runtime.AwaitPathRetirement(ctx) {
			return page, ok, err
		}
		_ = c.cursor.Close()
		if err = c.owner.recover(ctx, c.runtime); err != nil {
			return catalog.CatalogPage{}, false, err
		}
		if err = c.restore(ctx); err != nil {
			return catalog.CatalogPage{}, false, err
		}
	}
}
func (c *directoryCursor) restore(ctx context.Context) error {
	for {
		if err := c.open(ctx); err != nil {
			return err
		}
		err := c.replay(ctx)
		if err == nil || !c.runtime.AwaitPathRetirement(ctx) {
			return err
		}
		_ = c.cursor.Close()
		if err = c.owner.recover(ctx, c.runtime); err != nil {
			return err
		}
	}
}

// The last commitment authenticates the complete prefix, so recovery stores no
// duplicate catalog payload and never merges entries from changed generations.
func (c *directoryCursor) replay(ctx context.Context) error {
	for index := uint32(0); index < c.count; index++ {
		replay, exists, err := c.cursor.Next(ctx)
		if err != nil {
			return err
		}
		if !exists || (index+1 == c.count && replay.Commitment() != c.commitment) {
			return catalog.ErrDirectoryStale
		}
	}
	return nil
}

func (c *directoryCursor) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.cursor.Close()
	if c.runtime.PathsExhausted() {
		return nil
	}
	return err
}
