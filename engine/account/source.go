package account

import (
	"errors"

	"github.com/windshare/windshare/core/serviceauthority"
)

type Source struct {
	service    *Service
	address    string
	bound      bool
	credential *credentialSession
}

func (service *Service) Source(address string) *Source {
	if service == nil {
		return nil
	}
	service.Start()
	service.startNode(address)
	return &Source{service: service, address: address}
}

func (source *Source) RegistrationAuthorization() serviceauthority.RegistrationAuthorization {
	if source == nil {
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionReady}
	}
	service := source.service
	if service.config.ResolveNode == nil {
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionDenied, Cause: errors.New("official node resolver missing")}
	}
	node, official := service.config.ResolveNode(source.address)
	if !official {
		return serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionDenied, Cause: errors.New("relay is no longer a current official node")}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	authorization := serviceauthority.RegistrationAuthorization{State: serviceauthority.AdmissionPendingSetup, Changed: service.changed}
	record := service.record
	if source.bound {
		if source.credential == nil {
			// A publication that reached the relay anonymously never adopts a later login.
			authorization.State = serviceauthority.AdmissionReady
			return authorization
		}
		record = source.credential.record
		if sameIdentity(record, service.record) {
			source.credential.record = cloneRecord(service.record)
			record = service.record
		}
		if service.record.Denial == "account_blocked" && service.record.Credential != nil &&
			service.record.Credential.Account == record.Credential.Account {
			record.Denial = service.record.Denial
		}
	}
	record = service.withDenialLocked(record)
	if record.Denial != "" {
		if source.bound {
			service.refreshCredentialLocked(source.credential)
		}
		authorization.State, authorization.Cause = serviceauthority.AdmissionDenied, errors.New(record.Denial)
		return authorization
	}
	if service.validTicket(record) && service.currentGenerationLocked(record) {
		authorization.State, authorization.Bearer = serviceauthority.AdmissionReady, record.Ticket
		authorization.Retain = source.retain(record)
		return authorization
	}
	if source.bound {
		service.refreshCredentialLocked(source.credential)
		authorization.State = serviceauthority.AdmissionExpired
		return authorization
	}
	// Unknown local state might conceal a block. A corrupt or locked cache must
	// neither block independent relays nor silently discard a previous denial.
	if service.loaded {
		if cached := service.nodes[source.address]; cached != nil && cached.key == node.Key &&
			cached.expires.After(service.config.Now()) && cached.anonymous && cached.available {
			authorization.State = serviceauthority.AdmissionReady
			authorization.Retain = source.retain(Record{})
			return authorization
		}
	}
	if service.state == Unavailable {
		authorization.State, authorization.Cause = serviceauthority.AdmissionTemporaryFailure, errors.New(service.reason)
	} else if record.Credential != nil {
		authorization.State = serviceauthority.AdmissionExpired
	}
	return authorization
}
