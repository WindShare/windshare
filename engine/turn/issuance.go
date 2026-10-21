package turn

import (
	"context"
	"errors"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turn/internal/provider"
)

type issuance struct {
	account         *accountLedger
	profileID       string
	profileRevision uint64
	cancel          context.CancelCauseFunc
}

func (m *accountManager) Acquire(ctx context.Context, profileID string, subject turnpath.Subject) (*turnpath.Lease, error) {
	return m.acquire(ctx, profileID, subject, nil)
}

func (m *accountManager) acquire(ctx context.Context, profileID string, subject turnpath.Subject, previous *leaseState) (*turnpath.Lease, error) {
	m.mu.Lock()
	profile, ok := m.current.profiles[profileID]
	account := m.current.accounts[profile.AccountID]
	ledger := m.ledgers[profile.AccountID]
	var admissionErr error
	switch {
	case ctx.Err() != nil:
		admissionErr = context.Cause(ctx)
	case m.closed:
		admissionErr = ErrClosed
	case !ok:
		admissionErr = ErrConfiguration
	case exhausted(account, ledger):
		admissionErr = ErrBudget
	case ledger.active+ledger.pendingCleanup >= MaximumOutstandingLeases:
		admissionErr = ErrCapacity
	}
	if admissionErr != nil {
		m.mu.Unlock()
		return nil, admissionErr
	}
	request, cancel := context.WithCancelCause(ctx)
	job := &issuance{account: ledger, profileID: profileID, profileRevision: profile.revision, cancel: cancel}
	m.requests[job] = struct{}{}
	ledger.active++
	m.work.Add(1)
	revision := m.current.revision
	identity := iceconfig.Identity{Provider: account.Provider, Account: profile.AccountID, Profile: profile.ID}
	m.mu.Unlock()
	defer m.work.Done()
	defer cancel(nil)

	m.emit(Event{Identity: identity, Subject: subject, Revision: revision, ProfileRevision: profile.revision, Milestone: "issuance_started"})
	bounded, stop := context.WithTimeout(request, IssuanceTimeout)
	defer stop()
	issued, err := issueCredentials(bounded, account.Source, subject, previous)
	if err == nil && !issued.Credentials.ValidAt(m.now()) {
		err = provider.ErrExpired
	}
	urls := profile.URLs
	if len(urls) == 0 {
		urls = issued.URLs
	}
	var relay iceconfig.Relay
	if err == nil {
		relay, err = iceconfig.NewRelay(identity, urls, issued.Credentials)
	}

	m.mu.Lock()
	delete(m.requests, job)
	if cause := context.Cause(bounded); cause != nil {
		err = cause
	}
	if err == nil {
		err = m.issuanceUnavailableLocked(job)
	}
	if err != nil {
		ledger.active--
		milestone := issuanceOutcome(err, request)
		if milestone == "issuance_failed" {
			ledger.lastFailure = err.Error()
		}
		event := Event{Identity: identity, Subject: subject, Revision: revision, ProfileRevision: profile.revision, Milestone: milestone, Reason: err.Error(), ResourceID: cleanupID(issued.Cleanup)}
		cleanup := m.retainCleanupLocked(ledger, issued.Cleanup, event)
		m.mu.Unlock()
		m.emit(event)
		if cleanup != nil {
			go m.clean(cleanup)
		}
		return nil, err
	}
	ledger.lastFailure = ""
	authority, revoke := context.WithCancelCause(context.Background())
	state := &leaseState{manager: m, account: ledger, identity: identity,
		subject: subject, revision: revision, profileRevision: profile.revision, issuer: account.Source,
		issued: issued, authority: authority, cancel: revoke}
	m.leases[state] = struct{}{}
	// Retain work until revocation completes; shutdown can safely join leases
	// and issuances without adding work after the last reference disappears.
	m.work.Add(1)
	m.mu.Unlock()
	leaseConfig := turnpath.LeaseConfig{Relay: relay, Authority: authority, Charge: state.charge, Release: state.release}
	if _, ok := account.Source.source.(renewable); ok {
		leaseConfig.Renew = func(ctx context.Context, next turnpath.Subject) (*turnpath.Lease, error) {
			return m.acquire(ctx, profileID, next, state)
		}
	}
	lease := turnpath.NewLease(leaseConfig)
	m.emit(Event{Identity: identity, Subject: subject, Revision: revision, ProfileRevision: profile.revision, Milestone: "credentials_issued", ResourceID: cleanupID(issued.Cleanup)})
	return lease, nil
}

func issueCredentials(ctx context.Context, issuer *credentialIssuer, subject turnpath.Subject, previous *leaseState) (provider.Acquisition, error) {
	providerSubject := provider.Subject{SessionID: subject.SessionID, PathID: subject.PathID}
	// Inventory edits do not change token ownership. A rotated issuer or revoked
	// predecessor must acquire fresh authority instead of reusing renewal material.
	if previous == nil || previous.issuer != issuer || previous.authority.Err() != nil {
		return issuer.source.Issue(ctx, providerSubject)
	}
	source, ok := issuer.source.(renewable)
	if !ok {
		return provider.Acquisition{}, ErrConfiguration
	}
	return source.Renew(ctx, providerSubject, previous.issued)
}

func issuanceOutcome(err error, request context.Context) string {
	switch {
	case errors.Is(err, turnpath.ErrSuperseded):
		return "issuance_superseded"
	case context.Cause(request) != nil:
		return "issuance_canceled"
	default:
		return "issuance_failed"
	}
}
