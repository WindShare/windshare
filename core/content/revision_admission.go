package content

import (
	"crypto/rand"
	"errors"
	"fmt"
	"time"
)

type randomLeaseIDs struct{}

func (randomLeaseIDs) NewLeaseID() (LeaseID, error) {
	var lease LeaseID
	if _, err := rand.Read(lease[:]); err != nil {
		return LeaseID{}, fmt.Errorf("generate revision lease identity: %w", err)
	}
	return lease, nil
}

type openAdmission struct {
	leaseQuota         *QuotaReservation
	sessionHandleQuota *QuotaReservation
}

func (a *openAdmission) release() {
	if a == nil {
		return
	}
	a.leaseQuota.Release()
	a.leaseQuota = nil
	a.sessionHandleQuota.Release()
	a.sessionHandleQuota = nil
}

// reserveOpenAdmissionLocked keeps shutdown from overtaking quota admission
// before the attempt is registered in s.opening. The caller must hold s.mu.
func (s *RevisionStore) reserveOpenAdmissionLocked(sessionQuota *QuotaAccount) (*openAdmission, error) {
	leaseQuota, err := ReserveQuota(QuotaHierarchy{Process: s.processQuota, Share: s.shareQuota, Session: sessionQuota}, QuotaUsage{ActiveLeases: 1})
	if err != nil {
		return nil, err
	}
	handleQuota, err := reserveQuotaAccounts([]*QuotaAccount{sessionQuota}, QuotaUsage{StableHandles: 1})
	if err != nil {
		leaseQuota.Release()
		return nil, err
	}
	return &openAdmission{leaseQuota: leaseQuota, sessionHandleQuota: handleQuota}, nil
}

// acquireLeaseLocked publishes every lease, per-session handle charge, and
// revision grace transition atomically with invalidation and shutdown.
func (s *RevisionStore) acquireLeaseLocked(revision *revisionState, sessionQuota *QuotaAccount, now time.Time, admission *openAdmission) (RevisionLease, error) {
	defer admission.release()
	if revision.lifecycle != revisionLifecycleActive || revision.closed {
		return RevisionLease{}, ErrRevisionDrift
	}
	var leaseQuota *QuotaReservation
	if admission != nil {
		leaseQuota = admission.leaseQuota
		admission.leaseQuota = nil
	}
	if leaseQuota == nil {
		var err error
		leaseQuota, err = ReserveQuota(QuotaHierarchy{Process: s.processQuota, Share: s.shareQuota, Session: sessionQuota}, QuotaUsage{ActiveLeases: 1})
		if err != nil {
			return RevisionLease{}, err
		}
	}
	sessionHandle := revision.sessionHandles[sessionQuota]
	newSessionHandle := sessionHandle == nil
	if newSessionHandle {
		var handleQuota *QuotaReservation
		if admission != nil {
			handleQuota = admission.sessionHandleQuota
			admission.sessionHandleQuota = nil
		}
		if handleQuota == nil {
			var reserveErr error
			handleQuota, reserveErr = reserveQuotaAccounts([]*QuotaAccount{sessionQuota}, QuotaUsage{StableHandles: 1})
			if reserveErr != nil {
				leaseQuota.Release()
				return RevisionLease{}, reserveErr
			}
		}
		sessionHandle = &sessionHandleState{quota: handleQuota}
	}
	leaseID, err := s.leaseIDs.NewLeaseID()
	if err != nil {
		leaseQuota.Release()
		if newSessionHandle {
			sessionHandle.quota.Release()
		}
		return RevisionLease{}, err
	}
	if leaseID.IsZero() {
		leaseQuota.Release()
		if newSessionHandle {
			sessionHandle.quota.Release()
		}
		return RevisionLease{}, errors.New("revision lease generator returned a zero identity")
	}
	if _, exists := s.leases[leaseID]; exists {
		leaseQuota.Release()
		if newSessionHandle {
			sessionHandle.quota.Release()
		}
		return RevisionLease{}, errors.New("revision lease generator reused an identity")
	}
	if _, exists := s.leaseTombstones[leaseID]; exists {
		leaseQuota.Release()
		if newSessionHandle {
			sessionHandle.quota.Release()
		}
		return RevisionLease{}, errors.New("revision lease generator reused an identity")
	}
	lease := RevisionLease{id: leaseID, descriptor: revision.descriptor, ttl: LeaseTTL, renewAfter: LeaseTTL - LeaseRenewWindow}
	state := &leaseState{
		lease: lease, revision: revision, quota: leaseQuota, sessionQuota: sessionQuota, status: leaseActive,
		createdAt: now, expiresAt: now.Add(LeaseTTL),
	}
	if newSessionHandle {
		revision.sessionHandles[sessionQuota] = sessionHandle
	}
	sessionHandle.leases++
	revision.leases[leaseID] = state
	revision.graceUntil = time.Time{}
	s.leases[leaseID] = state
	return lease, nil
}
