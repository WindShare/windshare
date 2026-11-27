package account

import (
	"context"
	"errors"
	"fmt"

	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/serviceauthority"
)

type shareAttribution uint8

const (
	attributionPending shareAttribution = iota
	attributionAnonymous
	attributionAccount
	attributionClosed
)

type attributionReservation struct {
	identity           shareAttribution
	inFlight           int
	retainedCredential bool
}

// ShareAuthorization owns the identity shared by all official relay registrations
// in one share. Its state and credential references are guarded by Service.mu.
type ShareAuthorization struct {
	service      *Service
	attribution  shareAttribution
	reservation  *attributionReservation
	credential   *credentialSession
	nodes        map[string]struct{}
	selectionErr error
	loading      chan struct{}
}

// NewShareAuthorization binds the selected local credential before any official
// relay attempts registration. Setup and ticket renewal can finish in the background.
func (service *Service) NewShareAuthorization(ctx context.Context) *ShareAuthorization {
	if service == nil {
		return nil
	}
	share := &ShareAuthorization{service: service}
	share.loadSelection(ctx)
	return share
}

// PrepareShareAuthorization lets independent routes register while the local
// account store resolves. Every official route waits on the same selection.
func (service *Service) PrepareShareAuthorization(ctx context.Context) *ShareAuthorization {
	if service == nil {
		return nil
	}
	share := &ShareAuthorization{service: service, loading: make(chan struct{})}
	service.workers.Go(func() {
		share.loadSelection(ctx)
		service.mu.Lock()
		close(share.loading)
		share.loading = nil
		service.mu.Unlock()
	})
	return share
}

func (share *ShareAuthorization) loadSelection(ctx context.Context) {
	service := share.service
	bounded, cancel := context.WithTimeout(ctx, CacheLoadTimeout)
	defer cancel()
	_, err := service.syncSelection(bounded, service.load, share.bindSelectionLocked)
	if err != nil {
		service.mu.Lock()
		if share.attribution == attributionClosed {
			service.mu.Unlock()
			return
		}
		share.selectionErr = fmt.Errorf("read selected account: %w", err)
		if service.unconfirmedShares == nil {
			service.unconfirmedShares = make(map[*ShareAuthorization]struct{})
		}
		service.unconfirmedShares[share] = struct{}{}
		service.mu.Unlock()
	}
}

func (share *ShareAuthorization) bindSelectionLocked(record Record) {
	if share.attribution == attributionClosed {
		return
	}
	share.credential = share.service.retainCredentialLocked(record)
	share.selectionErr = nil
}

func (share *ShareAuthorization) Close() {
	if share == nil {
		return
	}
	service := share.service
	service.mu.Lock()
	defer service.mu.Unlock()
	if share.attribution != attributionClosed {
		share.attribution = attributionClosed
		share.reservation = nil
		delete(service.unconfirmedShares, share)
		delete(service.officialShares, share)
		share.releaseNodesLocked()
		share.releaseCredentialLocked()
		service.refreshDemandChangedLocked()
	}
}

type Source struct {
	share   *ShareAuthorization
	address string
	// discover is true for sources returned by ShareAuthorization.Source,
	// enabling on-demand node policy polling while attribution is pending.
	discover bool
}

func (share *ShareAuthorization) Source(address string) *Source {
	if share == nil {
		return nil
	}
	share.beginOfficialUse()
	return &Source{share: share, address: address, discover: true}
}

func (share *ShareAuthorization) retainNodeLocked(address string) {
	service := share.service
	if share.attribution != attributionPending || service.ctx.Err() != nil {
		return
	}
	if share.nodes == nil {
		share.nodes = make(map[string]struct{})
	}
	if _, retained := share.nodes[address]; retained {
		return
	}
	share.nodes[address] = struct{}{}
	service.nodes.retain(address)
}

func (share *ShareAuthorization) releaseNodesLocked() {
	for address := range share.nodes {
		share.service.nodes.release(address)
	}
	clear(share.nodes)
}

func (share *ShareAuthorization) beginOfficialUse() {
	service := share.service
	service.mu.Lock()
	defer service.mu.Unlock()
	if share.attribution == attributionClosed || service.ctx.Err() != nil {
		return
	}
	if _, active := service.officialShares[share]; active {
		return
	}
	if service.officialShares == nil {
		service.officialShares = make(map[*ShareAuthorization]struct{})
	}
	service.officialShares[share] = struct{}{}
	service.refreshDemandChangedLocked()
}

func (source *Source) ReserveRegistrationAuthorization() serviceauthority.RegistrationAuthorization {
	if source == nil {
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady}
	}
	share := source.share
	service := share.service
	if service.config.ResolveNode == nil {
		return deniedAuthorization("official node resolver missing")
	}
	node, official := service.config.ResolveNode(source.address)
	if !official {
		return deniedAuthorization("relay is no longer a current official node")
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	switch share.attribution {
	case attributionClosed:
		return deniedAuthorization("share authorization is closed")
	case attributionAnonymous:
		// The relay enforces its current anonymous policy. A stale discovery
		// result need not hold up recovery once the share is already anonymous.
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady}
	}
	if share.loading != nil {
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionPendingSetup, Changed: share.loading}
	}
	if share.selectionErr != nil {
		// Unreadable state can conceal a different login or an explicit denial.
		// Surface persistent local file errors for user repair instead of looping
		// forever in temporary-failure wait.
		state := serviceauthority.AdmissionTemporaryFailure
		if isLocalStoreError(share.selectionErr) || service.state == StorageError {
			state = serviceauthority.AdmissionUserActionRequired
		}
		return serviceauthority.RegistrationAuthorization{
			State: state, Changed: service.changed, Cause: share.selectionErr,
		}
	}
	authorization := source.selectAuthorizationLocked(node)
	if authorization.State == serviceauthority.AdmissionReady && share.attribution == attributionPending {
		authorization.Reservation = share.reserveAttributionLocked(authorization.Bearer)
	}
	return authorization
}

func (share *ShareAuthorization) reserveAttributionLocked(bearer string) serviceauthority.RegistrationReservation {
	service := share.service
	if share.reservation == nil {
		identity := attributionAnonymous
		retainedCredential := false
		if bearer != "" {
			identity = attributionAccount
			if share.credential == nil {
				share.credential = service.retainCredentialLocked(service.record)
				retainedCredential = share.credential != nil
			}
		}
		share.reservation = &attributionReservation{
			identity:           identity,
			retainedCredential: retainedCredential,
		}
		if identity == attributionAnonymous {
			service.notifyLocked()
		}
	}
	reservation := share.reservation
	reservation.inFlight++
	finished := false
	return func(delivery serviceauthority.RegistrationDelivery) {
		service.mu.Lock()
		defer service.mu.Unlock()
		if finished || share.reservation != reservation {
			return
		}
		finished = true
		reservation.inFlight--
		if delivery == serviceauthority.RegistrationNotSent {
			if reservation.inFlight == 0 {
				share.reservation = nil
				if reservation.retainedCredential {
					share.releaseCredentialLocked()
					service.refreshDemandChangedLocked()
				} else {
					service.notifyLocked()
				}
			}
			return
		}

		// An acknowledgement can be lost after admission. Keep later attempts
		// on the same attribution as any registration the relay could retain.
		share.attribution = reservation.identity
		share.reservation = nil
		// Anonymous policy is only consulted while attribution is pending; once
		// a share commits to account or anonymous admission, stop polling nodes.
		share.releaseNodesLocked()
		if share.attribution == attributionAnonymous {
			fallback := share.fallbackCredentialLocked()
			share.releaseCredentialLocked()
			service.refreshDemandChangedLocked()
			service.emitAnonymousFallback(fallback, service.state)
		}
	}
}

func (source *Source) selectAuthorizationLocked(node Node) serviceauthority.RegistrationAuthorization {
	share := source.share
	service := share.service
	record := service.record
	if share.credential != nil {
		record = share.credential.record
	}
	record = service.withDenialLocked(record)
	if record.Denial != "" {
		return deniedAuthorization(record.Denial)
	}
	identity := share.attribution
	if share.reservation != nil {
		identity = share.reservation.identity
	}
	if identity == attributionAnonymous {
		if source.permitsAnonymousLocked(node) {
			return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady}
		}
		// Only an anonymous-capable node can join this reservation. A ticket
		// becoming available cannot replace the identity of an in-flight dial.
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionPendingAttribution, Changed: service.changed}
	}
	if service.validTicket(record) && service.currentGenerationLocked(record) {
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady, Bearer: record.Ticket}
	}
	if identity == attributionPending && share.allowsAnonymousFallbackLocked(record) && source.permitsAnonymousLocked(node) {
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady}
	}
	authorization := serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionPendingSetup, Changed: service.changed}
	switch {
	case identity == attributionAccount || share.credential != nil && !sameIdentity(record, service.record):
		if !sameIdentity(record, service.record) {
			service.refreshCredentialLocked(share.credential)
		}
		authorization.State = serviceauthority.AdmissionExpired
	case service.state == StorageError:
		authorization.State, authorization.Cause = serviceauthority.AdmissionUserActionRequired, errors.New(service.reason)
	case service.state == Unavailable:
		authorization.State, authorization.Cause = serviceauthority.AdmissionTemporaryFailure, errors.New(service.reason)
	case record.Credential != nil:
		authorization.State = serviceauthority.AdmissionExpired
	case service.state == RegistrationRequired || service.state == EmailRequired:
		authorization.State = serviceauthority.AdmissionUserActionRequired
	}
	return authorization
}

// A logged-in share waits for a pending ticket renewal so a faster anonymous
// node discovery cannot downgrade it and lock out account-only nodes. Once
// renewal enters failure backoff, permitted anonymous nodes remain usable.
func (share *ShareAuthorization) allowsAnonymousFallbackLocked(record Record) bool {
	if share.service.state == StorageError {
		return false
	}
	if record.Credential == nil {
		return true
	}
	if share.credential != nil && !sameIdentity(record, share.service.record) {
		return share.credential.retry > 0
	}
	return share.service.state == Unavailable && share.service.config.Now().Before(share.service.retryAt)
}

func (source *Source) permitsAnonymousLocked(node Node) bool {
	if source.share.service.nodes.permitsAnonymous(source.address, node) {
		return true
	}
	if source.discover {
		source.share.retainNodeLocked(source.address)
	}
	return false
}

func deniedAuthorization(reason string) serviceauthority.RegistrationAuthorization {
	return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionDenied, Cause: errors.New(reason)}
}

func (share *ShareAuthorization) fallbackCredentialLocked() *Credential {
	if share.credential == nil || share.credential.record.Credential == nil {
		return nil
	}
	credential := *share.credential.record.Credential
	return &credential
}

func (service *Service) emitAnonymousFallback(credential *Credential, state State) {
	if credential == nil || !service.config.Diagnostics.Enabled() {
		return
	}
	fields := diagnostics.Fields{"state": diagnostics.Text(string(state))}
	credentialFields(fields, credential)
	service.emitDiagnostic("account_share_anonymous_fallback", fields)
}
