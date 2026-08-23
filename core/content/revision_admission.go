package content

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/windshare/windshare/core/content/revisioncapacity"
)

type randomLeaseIDs struct{}

func (randomLeaseIDs) NewLeaseID() (LeaseID, error) {
	var lease LeaseID
	if _, err := rand.Read(lease[:]); err != nil {
		return LeaseID{}, fmt.Errorf("generate revision lease identity: %w", err)
	}
	return lease, nil
}

type capacityChargeRelease struct {
	active        revisioncapacity.ActiveLeaseCharge
	sessionHandle revisioncapacity.SessionHandleCharge
}

func (r capacityChargeRelease) run() error {
	var err error
	if r.active.Valid() {
		err = errors.Join(err, r.active.Release())
	}
	if r.sessionHandle.Valid() {
		err = errors.Join(err, r.sessionHandle.Release())
	}
	return err
}

func capacityRevisionID(identity revisionIdentity) revisioncapacity.RevisionID {
	return revisioncapacity.RevisionID(fmt.Sprintf("%x:%x", identity.file.Bytes(), identity.revision.Bytes()))
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

// publishLeaseLocked performs only store-owned publication. Coordinator charges
// have already been committed outside s.mu, keeping both lock domains unnested.
func (s *RevisionStore) publishLeaseLocked(
	revision *revisionState,
	session *revisioncapacity.SessionRegistration,
	now time.Time,
	leaseID LeaseID,
	activeCharge revisioncapacity.ActiveLeaseCharge,
	sessionCharge revisioncapacity.SessionHandleCharge,
) (RevisionLease, error) {
	if revision.lifecycle != revisionLifecycleActive || revision.closed || revision.closing {
		return RevisionLease{}, ErrRevisionDrift
	}
	sessionHandle := revision.sessionHandles[session]
	newSessionHandle := sessionHandle == nil
	if newSessionHandle {
		if !sessionCharge.Valid() {
			return RevisionLease{}, errors.New("revision capacity admission omitted the first session-handle charge")
		}
		sessionHandle = &sessionHandleState{charge: sessionCharge}
	} else if sessionCharge.Valid() {
		return RevisionLease{}, errors.New("revision capacity admission duplicated a session-handle charge")
	}
	lease := RevisionLease{id: leaseID, descriptor: revision.descriptor, ttl: LeaseTTL, renewAfter: LeaseTTL - LeaseRenewWindow}
	state := &leaseState{
		lease: lease, revision: revision, activeCharge: activeCharge, session: session, status: leaseActive,
		createdAt: now, expiresAt: now.Add(LeaseTTL),
	}
	if newSessionHandle {
		revision.sessionHandles[session] = sessionHandle
	}
	sessionHandle.leases++
	revision.activeLeases++
	revision.leases[leaseID] = state
	s.leases[leaseID] = state
	return lease, nil
}

func (s *RevisionStore) capacityAdmissionContext(ctx context.Context) (context.Context, func()) {
	admissionContext, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.capacityContext, cancel)
	return admissionContext, func() {
		stop()
		cancel()
	}
}
