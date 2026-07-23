package content

import (
	"context"
	"errors"
	"time"

	"github.com/windshare/windshare/core/catalog"
)

var (
	ErrRevisionStale        = errors.New("catalog file version candidate is stale")
	ErrRevisionNotFound     = errors.New("catalog file was not found")
	ErrUnsupportedStability = errors.New("backend cannot prove stable file reads")
	ErrSourceDrift          = errors.New("stable file source changed")
	ErrRevisionDrift        = errors.New("file revision drifted")
	ErrLeaseExpired         = errors.New("file revision lease expired")
	ErrInvalidLease         = errors.New("file revision lease is invalid")
	ErrRenewTooEarly        = errors.New("file revision lease does not need renewal yet")
	ErrLeaseLifetime        = errors.New("file revision lease reached its maximum lifetime")
	ErrOpenBatchLimit       = errors.New("open revision batch exceeds its item limit")
	ErrRevisionStoreClosed  = errors.New("revision store is closed")
)

const (
	MaxOpenRevisionBatch = 64
	LeaseTTL             = 120 * time.Second
	LeaseRenewWindow     = 60 * time.Second
	MaxLeaseLifetime     = 2 * time.Hour
	RevisionResumeGrace  = 120 * time.Second
)

type FileRevisionDescriptor struct {
	shareInstance catalog.ShareInstance
	fileID        catalog.FileID
	revision      FileRevision
	geometry      FileGeometry
	modified      catalog.ModifiedTime
}

func NewFileRevisionDescriptor(share catalog.ShareInstance, file catalog.FileID, revision FileRevision, geometry FileGeometry, modified catalog.ModifiedTime) (FileRevisionDescriptor, error) {
	if share.IsZero() || file.IsZero() || revision.IsZero() {
		return FileRevisionDescriptor{}, errors.New("file revision descriptor requires share, file, and revision identities")
	}
	if _, err := NewFileGeometry(geometry.ExactSize(), geometry.ChunkSize()); err != nil {
		return FileRevisionDescriptor{}, err
	}
	return FileRevisionDescriptor{shareInstance: share, fileID: file, revision: revision, geometry: geometry, modified: modified}, nil
}

func (d FileRevisionDescriptor) ShareInstance() catalog.ShareInstance { return d.shareInstance }
func (d FileRevisionDescriptor) FileID() catalog.FileID               { return d.fileID }
func (d FileRevisionDescriptor) FileRevision() FileRevision           { return d.revision }
func (d FileRevisionDescriptor) ExactSize() uint64                    { return d.geometry.ExactSize() }
func (d FileRevisionDescriptor) ModifiedTime() catalog.ModifiedTime   { return d.modified }
func (d FileRevisionDescriptor) Geometry() FileGeometry               { return d.geometry }
func (d FileRevisionDescriptor) BlockCountFieldPresent() bool         { return false }

type RevisionLease struct {
	id         LeaseID
	descriptor FileRevisionDescriptor
	ttl        time.Duration
	renewAfter time.Duration
}

// NewRevisionLease lets consumer-side RevisionStore implementations satisfy the
// contentflow contract without exposing mutable lease fields. The authoritative
// store still owns expiry; the wire carries only these relative durations.
func NewRevisionLease(
	id LeaseID,
	descriptor FileRevisionDescriptor,
	ttl time.Duration,
	renewAfter time.Duration,
) (RevisionLease, error) {
	if id.IsZero() || descriptor.ShareInstance().IsZero() || descriptor.FileID().IsZero() ||
		descriptor.FileRevision().IsZero() || ttl <= 0 || renewAfter < 0 || renewAfter > ttl {
		return RevisionLease{}, errors.New("revision lease requires valid identities and relative timing")
	}
	return RevisionLease{id: id, descriptor: descriptor, ttl: ttl, renewAfter: renewAfter}, nil
}

func (l RevisionLease) ID() LeaseID                        { return l.id }
func (l RevisionLease) Descriptor() FileRevisionDescriptor { return l.descriptor }
func (l RevisionLease) TTL() time.Duration                 { return l.ttl }
func (l RevisionLease) RenewAfter() time.Duration          { return l.renewAfter }

// StableFile is returned by a backend only after it has verified the private
// catalog candidate. Verify must use a reliable, same-object stability proof;
// the backend must implement the platform support matrix frozen by the plan.
type StableFile interface {
	ExactSize() uint64
	ModifiedTime() catalog.ModifiedTime
	Verify(context.Context) error
	ReadAt(context.Context, []byte, uint64) (int, error)
	Close() error
}

type RevisionSource interface {
	OpenStable(context.Context, catalog.NodeRecord) (StableFile, error)
}

type CatalogNodeSource interface {
	Node(context.Context, catalog.NodeID) (catalog.NodeRecord, bool, error)
}

type CacheInvalidator interface {
	InvalidateRevision(catalog.FileID, FileRevision)
}

type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

type IdentityGenerator interface {
	NewFileRevision() (FileRevision, error)
	NewLeaseID() (LeaseID, error)
}
