package browsermatrixbroker

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/windshare/windshare/internal/browsermatrixpion"
)

var (
	errOperationConflict     = errors.New("credential broker operation conflicts with existing authority")
	errCapacityExhausted     = errors.New("credential broker replay capacity is exhausted")
	errCapabilityUnavailable = errors.New("credential broker revocable capability is unavailable")
)

func brokerStatus(err error) int {
	switch {
	case errors.Is(err, errCapacityExhausted):
		return http.StatusTooManyRequests
	case errors.Is(err, errOperationConflict):
		return http.StatusConflict
	default:
		return http.StatusServiceUnavailable
	}
}

func writeBrokerError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Length", "0")
	writer.WriteHeader(status)
}

func validateControlLease(
	scope requestScope,
	lease browsermatrixpion.ControlCredentialLease,
	now time.Time,
	turn *browsermatrixpion.ControlTURNDeclaration,
) (time.Time, time.Time, error) {
	issuedAt, issuedErr := parseTimestamp(lease.IssuedAt)
	expiresAt, expiresErr := parseTimestamp(lease.ExpiresAt)
	if lease.RequestID != scope.RequestID || lease.RunID != scope.RunID ||
		lease.ProfileID != scope.ProfileID || lease.ProbeNonce != scope.ProbeNonce ||
		!validOpaqueID(lease.LeaseID) || !canonicalIDPattern.MatchString(lease.AuthorityInstanceID) ||
		!sha256Pattern.MatchString(lease.AttestationSHA256) || lease.MaxAttempts != 1 ||
		!validCredentialBytes(lease.Credential) || issuedErr != nil || expiresErr != nil ||
		issuedAt.After(now.Add(maximumAllowedClockSkew)) || !now.Before(expiresAt) ||
		!expiresAt.After(issuedAt) || expiresAt.Sub(issuedAt) > maximumControlLease {
		return time.Time{}, time.Time{}, errors.New("control credential lease is invalid")
	}
	if turn == nil {
		if lease.TURN != nil {
			return time.Time{}, time.Time{}, errors.New("control credential lease injected TURN authority")
		}
	} else if lease.TURN == nil || *lease.TURN != *turn || lease.ExpiresAt != turn.ExpiresAt {
		return time.Time{}, time.Time{}, errors.New("control credential lease changed TURN authority")
	}
	return issuedAt, expiresAt, nil
}

func exactTURNReservation(request TURNAcquireRequest, lease TURNReservation) bool {
	return validOpaqueID(lease.ProviderLeaseID) && lease.RequestID == request.RequestID &&
		lease.RunID == request.RunID && lease.ProfileID == request.ProfileID &&
		lease.ProbeNonce == request.ProbeNonce && canonicalIDPattern.MatchString(lease.CredentialID) &&
		lease.Username != "" && lease.ExpiresAt == request.ExpiresAt && lease.MaxAttempts == 1
}

func exactTURNBoundLease(request TURNBindRequest, lease TURNBoundLease) bool {
	return TURNBindRequest(lease) == request
}

func validExpectedIdentity(identity WorkloadIdentityBinding) bool {
	return identity.ProtocolVersion == WorkloadIdentityProtocolVersion &&
		identity.Kind == "github-actions-oidc" && canonicalHTTPSOrigin(identity.Issuer) &&
		len(identity.Audience) >= 8 && validRepository(identity.Repository) &&
		validGitRef(identity.Ref) && validWorkflowRef(identity.WorkflowRef) &&
		canonicalHTTPSOrigin(identity.RequestOrigin) && identity.RequestPath != "" &&
		identity.RequestPath[0] == '/' && identity.RequestQuery != "" && identity.RequestQuery[0] == '?'
}

func canonicalControllerOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "/" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.String() == value
}

func TargetsBrokerEndpoint(request *http.Request) bool {
	return request != nil && request.URL != nil &&
		(request.URL.Path == BrokerPath || request.URL.EscapedPath() == BrokerPath)
}

func (handler *Handler) exactHTTPRequest(request *http.Request) bool {
	if request == nil || request.URL == nil || request.TLS == nil || request.Method != http.MethodPost ||
		request.URL.Scheme != "" || request.URL.Host != "" || request.URL.User != nil ||
		request.URL.Path != BrokerPath || request.URL.RawPath != "" || request.URL.RawQuery != "" ||
		request.URL.ForceQuery || request.URL.Fragment != "" || request.URL.RawFragment != "" ||
		request.RequestURI != BrokerPath || request.ContentLength <= 0 ||
		request.ContentLength > MaximumFrameBytes || len(request.TransferEncoding) != 0 ||
		len(request.Header.Values("Transfer-Encoding")) != 0 ||
		len(request.Header.Values("Content-Encoding")) != 0 {
		return false
	}
	origin, _ := url.Parse(handler.controllerOrigin)
	return request.Host == origin.Host &&
		exactHeader(request.Header, "Content-Type", BrokerContentType) &&
		exactHeader(request.Header, "Accept", BrokerContentType)
}

func exactHeader(header http.Header, name string, expected string) bool {
	values := header.Values(name)
	return len(values) == 1 && values[0] == expected
}
