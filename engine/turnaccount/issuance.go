package turnaccount

import (
	"context"
	"errors"

	"github.com/windshare/windshare/connectivity/iceconfig"
	"github.com/windshare/windshare/connectivity/turnpath"
	"github.com/windshare/windshare/engine/turnprovider"
)

type issuance struct {
	account *accountLedger
	cancel  context.CancelCauseFunc
}

func (m *Manager) Acquire(ctx context.Context, profileID string, subject turnpath.Subject) (*turnpath.Lease, error) {
	return m.acquire(ctx, profileID, subject, nil)
}

func (m *Manager) acquire(ctx context.Context, profileID string, subject turnpath.Subject, previous *leaseState) (*turnpath.Lease, error) {
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
	case ledger.active >= MaximumActiveLeases:
		admissionErr = ErrCapacity
	}
	if admissionErr != nil {
		m.mu.Unlock()
		return nil, admissionErr
	}
	request, cancel := context.WithCancelCause(ctx)
	job := &issuance{account: ledger, cancel: cancel}
	m.requests[job] = struct{}{}
	ledger.active++
	m.work.Add(1)
	revision := m.current.revision
	identity := iceconfig.Identity{Provider: account.Provider, Account: profile.AccountID, Profile: profile.ID}
	m.mu.Unlock()
	defer m.work.Done()
	defer cancel(nil)

	m.emit(Event{Identity: identity, Subject: subject, Revision: revision, Milestone: "issuance_started"})
	bounded, stop := context.WithTimeout(request, IssuanceTimeout)
	defer stop()
	issued, err := issueCredentials(bounded, account.Source, subject, revision, previous)
	if err == nil && !issued.Credentials.ValidAt(m.now()) {
		err = turnprovider.ErrExpired
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
	if err == nil && revision != m.current.revision {
		err = turnpath.ErrSuperseded
	}
	if err != nil {
		ledger.active--
		milestone := issuanceOutcome(err, request)
		if milestone == "issuance_failed" {
			ledger.lastFailure = err.Error()
		}
		m.mu.Unlock()
		m.emit(Event{Identity: identity, Subject: subject, Revision: revision, Milestone: milestone, Reason: err.Error()})
		if issued.Credentials.Kind() != 0 {
			m.work.Add(1)
			go func() {
				defer m.work.Done()
				m.revoke(account.Source, issued, identity, subject, revision)
			}()
		}
		return nil, err
	}
	ledger.lastFailure = ""
	authority, revoke := context.WithCancelCause(context.Background())
	state := &leaseState{manager: m, account: ledger, source: account.Source, identity: identity,
		subject: subject, revision: revision, issued: issued, authority: authority, cancel: revoke}
	m.leases[state] = struct{}{}
	// Retain work until revocation completes; shutdown can safely join leases
	// and issuances without adding work after the last reference disappears.
	m.work.Add(1)
	m.mu.Unlock()
	leaseConfig := turnpath.LeaseConfig{Relay: relay, Authority: authority, Charge: state.charge, Release: state.release}
	if _, ok := account.Source.(renewable); ok {
		leaseConfig.Renew = func(ctx context.Context, next turnpath.Subject) (*turnpath.Lease, error) {
			return m.acquire(ctx, profileID, next, state)
		}
	}
	lease := turnpath.NewLease(leaseConfig)
	m.emit(Event{Identity: identity, Subject: subject, Revision: revision, Milestone: "credentials_issued"})
	return lease, nil
}

func issueCredentials(ctx context.Context, source Source, subject turnpath.Subject, revision uint64, previous *leaseState) (turnprovider.Issued, error) {
	providerSubject := turnprovider.Subject{SessionID: subject.SessionID, PathID: subject.PathID}
	// Renewal tokens belong to the issuer that created them. A new revision
	// obtains fresh credentials instead of passing old tokens to a new source.
	if previous == nil || previous.revision != revision {
		return source.Issue(ctx, providerSubject)
	}
	issuer, ok := source.(renewable)
	if !ok {
		return turnprovider.Issued{}, ErrConfiguration
	}
	return issuer.Renew(ctx, providerSubject, previous.issued)
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
