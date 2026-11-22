package catalog

import (
	"context"
	"errors"
	"slices"
	"strings"
)

type ResourceMeter interface {
	Consume(ResourceUsage) error
	Release(ResourceUsage) error
}

type ScannedChild struct {
	DirectoryID      DirectoryID
	FileID           FileID
	Name             string
	SourceReference  SourceReference
	SourceIdentity   SourceIdentity
	VersionCandidate VersionCandidate
	ExpectedSize     uint64
	ModifiedTime     ModifiedTime
}

func (child ScannedChild) nodeRecord(parent DirectoryID) (NodeRecord, error) {
	switch {
	case !child.DirectoryID.IsZero() && child.FileID.IsZero():
		if !child.VersionCandidate.IsZero() || child.ExpectedSize != 0 {
			return NodeRecord{}, errors.New("catalog scanned directory carries file metadata")
		}
		return NewDirectoryNodeRecord(
			child.DirectoryID, parent, child.Name, child.SourceReference, child.SourceIdentity, child.ModifiedTime,
		)
	case child.DirectoryID.IsZero() && !child.FileID.IsZero():
		return NewFileNodeRecord(
			child.FileID, parent, child.Name, child.SourceReference, child.SourceIdentity,
			child.VersionCandidate, child.ExpectedSize, child.ModifiedTime,
		)
	default:
		return NodeRecord{}, errors.New("catalog scanned child must have exactly one kind-safe identity")
	}
}

type directorySortResult struct {
	records        []NodeRecord
	omittedCount   uint64
	omittedSamples []SourceOmission
}

// directorySorter collects scanned children in memory under the attempt's
// resource meter, rejects duplicate node identities, withholds portable-name
// sibling collision groups in place, and emits admitted records in canonical
// byte order.
type directorySorter struct {
	parent       DirectoryID
	meter        ResourceMeter
	seenIDs      map[NodeID]struct{}
	records      []NodeRecord
	rawNames     map[int]string
	rawNameBytes uint64
}

func newDirectorySorter(parent DirectoryID, meter ResourceMeter) (*directorySorter, error) {
	if meter == nil {
		return nil, errors.New("catalog directory sorter requires a resource meter")
	}
	return &directorySorter{
		parent:  parent,
		meter:   meter,
		seenIDs: make(map[NodeID]struct{}),
	}, nil
}

func (s *directorySorter) Add(ctx context.Context, child ScannedChild) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	node, err := child.nodeRecord(s.parent)
	if err != nil {
		return err
	}
	if _, duplicate := s.seenIDs[node.NodeID()]; duplicate {
		return ErrSiblingCollision
	}
	var extra uint64
	if child.Name != node.name {
		// Retain the scanner's pre-NFC spelling separately only when it differs so
		// NFC/NFD collision feedback can report exact differing scalars.
		extra = uint64(len(child.Name))
	}
	if err := s.meter.Consume(ResourceUsage{MemoryBytes: node.EstimatedMemoryBytes() + extra}); err != nil {
		return err
	}
	s.seenIDs[node.NodeID()] = struct{}{}
	if extra > 0 {
		if s.rawNames == nil {
			s.rawNames = make(map[int]string)
		}
		s.rawNames[len(s.records)] = child.Name
		s.rawNameBytes += extra
	}
	s.records = append(s.records, node)
	return nil
}

func (s *directorySorter) Finish(ctx context.Context) (directorySortResult, error) {
	if err := ctx.Err(); err != nil {
		return directorySortResult{}, err
	}
	records := s.records
	rawNames := s.rawNames
	releasedBytes := s.rawNameBytes
	// Drop deduplication state before collision filtering so seenIDs does not
	// overlap with transient name-key maps.
	s.seenIDs = nil
	s.records = nil
	s.rawNames = nil
	s.rawNameBytes = 0

	admitted, omittedCount, samples, omittedBytes := admitSortedSiblings(records, rawNames)
	releasedBytes += omittedBytes
	if releasedBytes > 0 {
		if err := s.meter.Release(ResourceUsage{MemoryBytes: releasedBytes}); err != nil {
			return directorySortResult{}, err
		}
	}
	slices.SortFunc(admitted, func(left, right NodeRecord) int {
		return strings.Compare(left.Entry().Name(), right.Entry().Name())
	})
	return directorySortResult{
		records:        admitted,
		omittedCount:   omittedCount,
		omittedSamples: samples,
	}, nil
}

func (s *directorySorter) Close() {
	if s == nil {
		return
	}
	s.records = nil
	s.seenIDs = nil
	s.rawNames = nil
	s.rawNameBytes = 0
}
