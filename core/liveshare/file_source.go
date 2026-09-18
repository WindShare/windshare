package liveshare

import (
	"context"
	"errors"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
)

type FileSourceContext struct {
	ShareInstance catalog.ShareInstance
	SyntheticRoot catalog.DirectoryID
	NewIdentity   func() ([catalog.IdentityBytes]byte, error)
}

// FileSource owns the selected scope's handles and grant-use leases. Selected
// roots require publishable metadata; unknown sizes and unsupported stable range
// reads fail explicitly rather than publishing guessed metadata. Descendants and
// content are accessed only on demand. StableFile handles own their own Close.
//
// A source with handle-only stability must also implement
// content.RevisionContinuitySource; omitting it asserts catalog continuity.
type FileSource interface {
	catalog.DirectoryScanner
	content.RevisionSource
	SelectedRoots() []catalog.NodeRecord
	Close() error
}

// FileSourceFactory binds a selection to one share. PrepareSender owns every
// non-nil returned source, including one returned alongside an error. A factory
// must release partially acquired resources when it cannot return an owner.
type FileSourceFactory interface {
	OpenFileSource(context.Context, FileSourceContext) (FileSource, error)
}

type FileSourceFactoryFunc func(context.Context, FileSourceContext) (FileSource, error)

func (factory FileSourceFactoryFunc) OpenFileSource(ctx context.Context, sourceContext FileSourceContext) (FileSource, error) {
	if factory == nil {
		return nil, errors.New("file source factory is nil")
	}
	return factory(ctx, sourceContext)
}
