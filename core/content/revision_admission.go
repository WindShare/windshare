package content

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/core/capacityledger"
	"github.com/windshare/windshare/core/catalog"
)

type randomRevisionIDs struct{}

func (randomRevisionIDs) NewFileRevision() (FileRevision, error) {
	var revision FileRevision
	if _, err := rand.Read(revision[:]); err != nil {
		return FileRevision{}, fmt.Errorf("generate file revision identity: %w", err)
	}
	return revision, nil
}

type randomLeaseIDs struct{}

func (randomLeaseIDs) NewLeaseID() (LeaseID, error) {
	var lease LeaseID
	if _, err := rand.Read(lease[:]); err != nil {
		return LeaseID{}, fmt.Errorf("generate revision lease identity: %w", err)
	}
	return lease, nil
}

func capacityRevisionID(identity revisionIdentity) string {
	return fmt.Sprintf("%x:%x", identity.file.Bytes(), identity.revision.Bytes())
}

func (s *RevisionStore) newFileRevisionLocked(file catalog.FileID) (FileRevision, error) {
	revisionID, err := s.revisionIDs.NewFileRevision()
	if err != nil {
		return FileRevision{}, err
	}
	if revisionID.IsZero() {
		return FileRevision{}, errors.New("file revision generator returned a zero identity")
	}
	candidate := revisionIdentity{file: file, revision: revisionID}
	if current := s.revisions[file]; current != nil && current.identity() == candidate {
		return FileRevision{}, errors.New("file revision generator reused a live identity")
	}
	for reading := range s.readingRevisions {
		if reading.identity() == candidate {
			return FileRevision{}, errors.New("file revision generator reused an active reading identity")
		}
	}
	return revisionID, nil
}

func (s *RevisionStore) newLeaseIDLocked() (LeaseID, error) {
	leaseID, err := s.leaseIDs.NewLeaseID()
	if err != nil {
		return LeaseID{}, err
	}
	if leaseID.IsZero() {
		return LeaseID{}, errors.New("revision lease generator returned a zero identity")
	}
	if _, exists := s.leases[leaseID]; exists {
		return LeaseID{}, errors.New("revision lease generator reused an identity")
	}
	if _, exists := s.leaseTombstones[leaseID]; exists {
		return LeaseID{}, errors.New("revision lease generator reused an identity")
	}
	return leaseID, nil
}

func (s *RevisionStore) publishLeaseLocked(
	revision *revisionState,
	session *RevisionSession,
	now time.Time,
	leaseID LeaseID,
	leaseHold capacityledger.Hold,
	fileHold capacityledger.Hold,
) (RevisionLease, error) {
	if revision.lifecycle != revisionLifecycleActive || revision.closed || revision.closing {
		return RevisionLease{}, ErrRevisionDrift
	}
	sessionHandle := revision.sessionHandles[session]
	if sessionHandle == nil {
		sessionHandle = &sessionHandleState{hold: fileHold}
		revision.sessionHandles[session] = sessionHandle
	}
	lease := RevisionLease{id: leaseID, descriptor: revision.descriptor, ttl: LeaseTTL, renewAfter: LeaseTTL - LeaseRenewWindow}
	state := &leaseState{
		lease: lease, revision: revision, leaseHold: leaseHold, session: session, status: leaseActive,
		createdAt: now, expiresAt: now.Add(LeaseTTL),
	}
	sessionHandle.leases++
	revision.activeLeases++
	revision.leases[leaseID] = state
	s.leases[leaseID] = state
	return lease, nil
}
