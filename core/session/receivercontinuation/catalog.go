package receivercontinuation

import (
	"context"

	"github.com/windshare/windshare/core/catalog"

	"github.com/windshare/windshare/core/session"
	receiversession "github.com/windshare/windshare/core/session/receiversession"
)

func (s *Session) OpenDirectoryPages(ctx context.Context, id catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
	return s.openDirectoryPages(ctx, id, false)
}

type continuationShapeCatalog struct {
	session *Session
}

func (c continuationShapeCatalog) OpenDirectoryPages(ctx context.Context, id catalog.DirectoryID) (catalog.DirectoryPageCursor, error) {
	return c.session.openDirectoryPages(ctx, id, true)
}

func (s *Session) openDirectoryPages(ctx context.Context, id catalog.DirectoryID, recordPrefetch bool) (catalog.DirectoryPageCursor, error) {
	cursor := &directoryCursor{
		owner:          s,
		directory:      id,
		live:           recordPrefetch,
		recordPrefetch: recordPrefetch,
	}
	if err := cursor.open(ctx); err != nil {
		return nil, err
	}
	return cursor, nil
}

type directoryCursor struct {
	owner           *Session
	directory       catalog.DirectoryID
	runtime         *receiversession.Runtime
	cursor          catalog.DirectoryPageCursor
	count           uint32
	commitment      catalog.PageCommitment
	closed          bool
	live            bool
	recordPrefetch  bool
	prefetchAborted bool
	prefetchPages   []catalog.CatalogPage
	prefetchBytes   uint64
}

func (c *directoryCursor) open(ctx context.Context) error {
	for {
		c.runtime = c.owner.Runtime()
		dependencies, err := c.runtime.TransferDependencies()
		if err != nil {
			return err
		}
		if c.live {
			c.cursor, err = dependencies.OpenDirectoryPagesLive(ctx, c.directory)
		} else {
			c.cursor, err = dependencies.OpenDirectoryPages(ctx, c.directory)
		}
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
		return catalog.CatalogPage{}, false, session.ErrRuntimeClosed
	}
	for {
		page, ok, err := c.cursor.Next(ctx)
		if err == nil {
			if ok {
				c.count++
				c.commitment = page.Commitment()
				if c.recordPrefetch && !c.prefetchAborted {
					pageBytes := page.EstimatedMemoryBytes()
					if pageBytes == 0 || pageBytes > receiversession.MaxPrefetchedDirectoryBytes-c.prefetchBytes {
						c.prefetchAborted = true
						c.prefetchPages = nil
					} else {
						c.prefetchPages = append(c.prefetchPages, page)
						c.prefetchBytes += pageBytes
					}
				}
			} else if c.recordPrefetch && !c.prefetchAborted {
				if len(c.prefetchPages) > 0 && c.prefetchPages[len(c.prefetchPages)-1].Terminal() {
					c.runtime.RecordPrefetchedDirectory(c.directory, c.prefetchPages)
				}
				c.prefetchAborted = true
				c.prefetchPages = nil
			}
			return page, ok, nil
		}
		if !c.runtime.AwaitPathRetirement(ctx) {
			c.prefetchAborted = true
			c.prefetchPages = nil
			return page, ok, err
		}
		c.prefetchAborted = true
		c.prefetchPages = nil
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
	c.prefetchAborted = true
	c.prefetchPages = nil
	err := c.cursor.Close()
	if c.runtime.PathsExhausted() {
		return nil
	}
	return err
}
