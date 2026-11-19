package account

import (
	"errors"

	"github.com/windshare/windshare/core/serviceauthority"
)

type shareAttribution uint8

const (
	attributionPending shareAttribution = iota
	attributionAnonymous
	attributionAccount
	attributionClosed
)

// ShareAuthorization owns the identity shared by all official relay registrations
// in one share. Its state and credential references are guarded by Service.mu.
type ShareAuthorization struct {
	service     *Service
	attribution shareAttribution
	credential  *credentialSession
}

func (service *Service) NewShareAuthorization() *ShareAuthorization {
	if service == nil {
		return nil
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	// A selected credential belongs to this share even if its ticket needs
	// refresh before the first registration. Setup can still finish in the background.
	return &ShareAuthorization{service: service, credential: service.retainCredentialLocked(service.record)}
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
		share.releaseCredentialLocked()
		service.notifyLocked()
	}
}

type Source struct {
	share   *ShareAuthorization
	address string
}

func (share *ShareAuthorization) Source(address string) *Source {
	if share == nil {
		return nil
	}
	share.service.Start()
	share.service.startNode(address)
	return &Source{share: share, address: address}
}

func (source *Source) RegistrationAuthorization() serviceauthority.RegistrationAuthorization {
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
	return source.selectAuthorizationLocked(node)
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
	if service.validTicket(record) && service.currentGenerationLocked(record) {
		if share.attribution == attributionPending {
			if share.credential == nil {
				share.credential = service.retainCredentialLocked(record)
			}
			share.attribution = attributionAccount
		}
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady, Bearer: record.Ticket}
	}
	if share.attribution == attributionPending && source.permitsAnonymousLocked(node) {
		// Commit before returning a usable authorization so concurrent nodes
		// cannot send different identities, even if one reply is lost.
		share.attribution = attributionAnonymous
		share.releaseCredentialLocked()
		service.notifyLocked()
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady}
	}
	authorization := serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionPendingSetup, Changed: service.changed}
	switch {
	case share.attribution == attributionAccount || share.credential != nil && !sameIdentity(record, service.record):
		service.refreshCredentialLocked(share.credential)
		authorization.State = serviceauthority.AdmissionExpired
	case service.state == Unavailable:
		authorization.State, authorization.Cause = serviceauthority.AdmissionTemporaryFailure, errors.New(service.reason)
	case record.Credential != nil:
		authorization.State = serviceauthority.AdmissionExpired
	}
	return authorization
}

func (source *Source) permitsAnonymousLocked(node Node) bool {
	service := source.share.service
	cached := service.nodes[source.address]
	// An unreadable account cache might conceal a denial. Use anonymous
	// admission only after loading local state and verifying the node's policy.
	return service.loaded && cached != nil && cached.key == node.Key &&
		cached.expires.After(service.config.Now()) && cached.anonymous && cached.available
}

func deniedAuthorization(reason string) serviceauthority.RegistrationAuthorization {
	return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionDenied, Cause: errors.New(reason)}
}
