package browsermatrixpion

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (service *Service) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if status, code := service.exactHTTPRequestStatus(request); status != 0 {
		writeProtocolError(w, status, code)
		return
	}
	if status, code := service.exactAttemptRequestStatus(request); status != 0 {
		writeProtocolError(w, status, code)
		return
	}
	authorization, authorized := service.authorizeRequest(request)
	if !authorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProtocolError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if authorization.finish != nil {
		defer authorization.finish()
	}
	if service.unavailable() && request.Method != http.MethodDelete {
		writeProtocolError(w, http.StatusServiceUnavailable, "service-containment-required")
		return
	}
	switch {
	case request.URL.Path == authorityProbePath:
		service.handleAuthorityProbe(w, request, authorization)
	case request.URL.Path == probePath:
		service.handleProbe(w, request, authorization)
	case request.URL.Path == turnCredentialPath:
		service.handleTURNCredential(w, request, authorization)
	case request.URL.Path == attemptsPath:
		service.handleAttempts(w, request, authorization)
	case strings.HasPrefix(request.URL.Path, attemptsPath+"/"):
		service.handleAttempt(w, request, authorization)
	default:
		writeProtocolError(w, http.StatusNotFound, "not-found")
	}
}

func (service *Service) handleAuthorityProbe(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	var input AuthorityProbeRequest
	if service.decodeBody(w, request, &input) != nil || validateAuthorityProbeRequest(input) != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-authority-probe")
		return
	}
	credential, _ := bearerCredential(request)
	response, status := service.issueAuthorityLease(
		input,
		authorization.controlLeaseID,
		credential,
	)
	if status != 0 {
		outcome := "credential-claim-rejected"
		switch status {
		case http.StatusTooManyRequests:
			outcome = "lease-capacity-rejected"
		case http.StatusServiceUnavailable:
			outcome = "issuance-unavailable"
		}
		emitTrace(service.trace, TraceEvent{
			Milestone: traceAuthorityProbe, InstanceID: service.instanceID,
			RunID: input.ControlAuthority.SampleAuthority.RunID, Outcome: outcome,
		})
		writeProtocolError(w, status, "authority-attestation-unavailable")
		return
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceAuthorityProbe, InstanceID: service.instanceID,
		RunID:             input.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: response.AttestationSHA256, Outcome: "available",
	})
	writeJSON(w, http.StatusOK, response)
}

func (service *Service) issueAuthorityLease(
	input AuthorityProbeRequest,
	controlLeaseID string,
	credential string,
) (AuthorityProbeResponse, int) {
	service.attestationMu.Lock()
	defer service.attestationMu.Unlock()

	now := service.clock.Now().UTC().Truncate(time.Millisecond)
	service.mu.Lock()
	service.pruneAuthorityLeasesLocked(now)
	if service.closed || len(service.containmentFailures) != 0 {
		service.mu.Unlock()
		return AuthorityProbeResponse{}, http.StatusServiceUnavailable
	}
	if len(service.authorityLeases) >= service.maximumTombstones {
		service.mu.Unlock()
		return AuthorityProbeResponse{}, http.StatusTooManyRequests
	}
	service.mu.Unlock()

	response, claimedControlLeaseID, expiresAt, accepted :=
		service.controlCredentials.consumeAuthorityProbe(
			controlLeaseID,
			credential,
			input,
			service.profileID,
		)
	if !accepted {
		return AuthorityProbeResponse{}, http.StatusConflict
	}
	if response.Attestation.Fixture.ProfileID != service.profileID ||
		response.Attestation.Fixture.RemoteServiceInstanceID != service.instanceID {
		return AuthorityProbeResponse{}, http.StatusInternalServerError
	}
	issuedAt, err := parseCanonicalTimestamp(response.Attestation.IssuedAt)
	if err != nil || response.Attestation.ExpiresAt != expiresAt.UTC().Format(canonicalTimestampLayout) {
		return AuthorityProbeResponse{}, http.StatusInternalServerError
	}
	requested := time.Duration(input.RequestedLeaseMillis) * time.Millisecond
	networkBinding, bindingErr := NetworkBindingSHA256(response.Attestation.Fixture)
	remotePeerBinding, remoteBindingErr := RemotePeerBindingSHA256FromFixture(response.Attestation.Fixture)
	if bindingErr != nil || remoteBindingErr != nil {
		return AuthorityProbeResponse{}, http.StatusInternalServerError
	}
	lease := authorityLease{
		response: response,
		binding: AttemptBinding{
			ControlAuthority: input.ControlAuthority,
			FixtureBinding: AttemptFixtureBinding{
				AttestationSHA256:       response.AttestationSHA256,
				AuthorityInstanceID:     response.Attestation.Fixture.AuthorityInstanceID,
				RemoteServiceInstanceID: service.instanceID,
				NetworkBindingSHA256:    networkBinding,
				RemotePeerBindingSHA256: remotePeerBinding,
			},
		},
		controlLeaseID:   claimedControlLeaseID,
		controlExpiresAt: expiresAt,
		issuedAt:         issuedAt,
		expiresAt:        expiresAt,
		requested:        requested,
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.closed || len(service.containmentFailures) != 0 {
		return AuthorityProbeResponse{}, http.StatusServiceUnavailable
	}
	if _, collision := service.authorityLeases[response.AttestationSHA256]; collision {
		return AuthorityProbeResponse{}, http.StatusInternalServerError
	}
	service.authorityLeases[response.AttestationSHA256] = lease
	return response, 0
}

func (service *Service) pruneAuthorityLeasesLocked(now time.Time) {
	for digest, lease := range service.authorityLeases {
		if !now.Before(lease.expiresAt) {
			delete(service.authorityLeases, digest)
		}
	}
}

func (service *Service) handleProbe(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	var input ProbeRequest
	if service.decodeBody(w, request, &input) != nil || validateProbeRequest(input) != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-probe")
		return
	}
	lease, ok := service.authorityLeaseForBinding(input.AttemptBinding)
	if !ok || !authorization.owns(lease.controlLeaseID) ||
		service.stunEndpoint == "" || input.STUNURI != service.stunEndpoint ||
		input.Nonce != lease.response.Attestation.Nonce {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceSTUNProbe, InstanceID: service.instanceID,
			RunID:             input.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: input.FixtureBinding.AttestationSHA256,
			Outcome:           "binding-mismatch",
		})
		writeProtocolError(w, http.StatusConflict, "authority-binding-mismatch")
		return
	}
	probeDeadline := service.clock.Now().UTC().Add(service.probeTimeout)
	if lease.expiresAt.Before(probeDeadline) {
		probeDeadline = lease.expiresAt
	}
	ctx, cancel := context.WithDeadline(request.Context(), probeDeadline)
	stop := context.AfterFunc(service.lifecycleContext, cancel)
	defer func() { stop(); cancel() }()
	if err := service.stunProber.Probe(ctx, service.stunEndpoint); err != nil ||
		!service.clock.Now().UTC().Before(lease.expiresAt) {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceSTUNProbe, InstanceID: service.instanceID,
			RunID:             input.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: input.FixtureBinding.AttestationSHA256,
			Outcome:           "failed",
		})
		writeProtocolError(w, http.StatusBadGateway, "stun-probe-failed")
		return
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceSTUNProbe, InstanceID: service.instanceID,
		RunID:             input.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: input.FixtureBinding.AttestationSHA256,
		Outcome:           "server-reflexive-observed",
	})
	writeJSON(w, http.StatusOK, ProbeResponse{
		ProtocolVersion: ProtocolVersion, AttemptBinding: input.AttemptBinding,
		Nonce:                   input.Nonce,
		ServerReflexiveObserved: true,
	})
}

func (service *Service) handleTURNCredential(
	w http.ResponseWriter,
	request *http.Request,
	authorization requestAuthorization,
) {
	if request.Method != http.MethodPost {
		writeProtocolError(w, http.StatusMethodNotAllowed, "method-not-allowed")
		return
	}
	var input TURNCredentialRequest
	if service.decodeBody(w, request, &input) != nil || validateTURNCredentialRequest(input) != nil {
		writeProtocolError(w, http.StatusBadRequest, "invalid-turn-credential-request")
		return
	}
	lease, ok := service.authorityLeaseForBinding(input.AttemptBinding)
	if !ok || !authorization.dynamic || !authorization.owns(lease.controlLeaseID) ||
		service.profileID != "scheduled-coturn" {
		emitTrace(service.trace, TraceEvent{
			Milestone: traceTURNCredential, InstanceID: service.instanceID,
			RunID:             input.ControlAuthority.SampleAuthority.RunID,
			AttestationSHA256: input.FixtureBinding.AttestationSHA256,
			Outcome:           "binding-mismatch",
		})
		writeProtocolError(w, http.StatusConflict, "authority-binding-mismatch")
		return
	}
	service.mu.Lock()
	if service.closed || len(service.containmentFailures) != 0 {
		service.mu.Unlock()
		writeProtocolError(w, http.StatusServiceUnavailable, "service-containment-required")
		return
	}
	turnLease, credentialBytes, available := service.beginControlTURNCredentialDeliveryLocked(
		lease.controlLeaseID,
		input.FixtureBinding.AttestationSHA256,
		service.clock.Now().UTC(),
	)
	service.mu.Unlock()
	if !available {
		writeProtocolError(w, http.StatusConflict, "turn-credential-capability-consumed")
		return
	}
	defer eraseCredentialBytes(credentialBytes)
	deliveryErr := writeControlTURNCredentialResponse(w, input.AttemptBinding, turnLease, credentialBytes)
	service.mu.Lock()
	service.finishControlTURNCredentialDeliveryLocked(lease.controlLeaseID)
	service.mu.Unlock()
	outcome := "issued"
	if deliveryErr != nil {
		outcome = "delivery-ambiguous-revoking"
	}
	emitTrace(service.trace, TraceEvent{
		Milestone: traceTURNCredential, InstanceID: service.instanceID,
		RunID:             input.ControlAuthority.SampleAuthority.RunID,
		AttestationSHA256: input.FixtureBinding.AttestationSHA256,
		Outcome:           outcome,
	})
	if deliveryErr != nil {
		// Retirement waits for this request's authorization owner to return, so the
		// asynchronous call cannot deadlock the response path it is containing.
		go func() { _, _ = service.RevokeControlCredentialAndWait(lease.controlLeaseID) }()
	}
}

func (service *Service) authorityLeaseForBinding(binding AttemptBinding) (authorityLease, bool) {
	if validateAttemptBinding(binding) != nil {
		return authorityLease{}, false
	}
	now := service.clock.Now().UTC()
	service.mu.Lock()
	defer service.mu.Unlock()
	service.pruneAuthorityLeasesLocked(now)
	lease, exists := service.authorityLeases[binding.FixtureBinding.AttestationSHA256]
	return lease, exists && lease.binding == binding && now.Before(lease.expiresAt)
}

func (service *Service) decodeBody(w http.ResponseWriter, request *http.Request, destination any) error {
	controller := http.NewResponseController(w)
	deadline := time.Now().Add(service.bodyReadTimeout)
	if err := controller.SetReadDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return errors.New("request body deadline could not be established")
	}
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	return decodeRequest(w, request, destination)
}

func (service *Service) authorizeRequest(
	request *http.Request,
) (requestAuthorization, bool) {
	provided, ok := bearerCredential(request)
	if !ok {
		return requestAuthorization{}, false
	}
	controlLeaseID := request.Header.Get(ControlLeaseIDHeader)
	service.mu.Lock()
	staticAuthorized := len(provided) == len(service.credential) &&
		subtle.ConstantTimeCompare([]byte(provided), service.credential) == 1
	service.mu.Unlock()
	if staticAuthorized {
		return requestAuthorization{}, controlLeaseID == ""
	}
	finish, authorized := service.controlCredentials.beginRequest(controlLeaseID, provided)
	if !authorized {
		return requestAuthorization{}, false
	}
	return requestAuthorization{
		controlLeaseID: controlLeaseID,
		finish:         finish,
		dynamic:        true,
	}, true
}

func bearerCredential(request *http.Request) (string, bool) {
	provided, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	return provided, ok && provided != ""
}

func (service *Service) unavailable() bool {
	service.mu.Lock()
	defer service.mu.Unlock()
	return service.closed || len(service.containmentFailures) != 0
}
