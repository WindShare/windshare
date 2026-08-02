package browsermatrixbroker

import (
	"errors"
	"os"
	"time"
)

type ServerPolicy struct {
	SchemaVersion            string `json:"schemaVersion"`
	ControllerOrigin         string `json:"controllerOrigin"`
	ProfileID                string `json:"profileId"`
	Audience                 string `json:"audience"`
	Issuer                   string `json:"issuer"`
	Repository               string `json:"repository"`
	Ref                      string `json:"ref"`
	WorkflowRef              string `json:"workflowRef"`
	IdentityRequestOrigin    string `json:"identityRequestOrigin"`
	IdentityRequestPath      string `json:"identityRequestPath"`
	IdentityRequestQuery     string `json:"identityRequestQuery"`
	LeaseMillis              int64  `json:"leaseMillis"`
	RetirementTimeoutMillis  int64  `json:"retirementTimeoutMillis"`
	TombstoneRetentionMillis int64  `json:"tombstoneRetentionMillis"`
	MaximumTombstones        int    `json:"maximumTombstones"`
	MaximumOIDCReplays       int    `json:"maximumOidcReplays"`
}

func LoadServerPolicy(path string) (ServerPolicy, error) {
	if !canonicalAbsolutePath(path) {
		return ServerPolicy{}, errors.New("credential broker server policy path is invalid")
	}
	document, err := os.ReadFile(path)
	if err != nil || len(document) == 0 || len(document) > maximumClientConfigBytes {
		erase(document)
		return ServerPolicy{}, errors.New("credential broker server policy is unavailable")
	}
	defer erase(document)
	var policy ServerPolicy
	if !decodeCanonicalLine(document, &policy) || validateServerPolicy(policy) != nil {
		return ServerPolicy{}, errors.New("credential broker server policy is invalid")
	}
	return policy, nil
}

func (policy ServerPolicy) ExpectedWorkloadIdentity() WorkloadIdentityBinding {
	return WorkloadIdentityBinding{
		ProtocolVersion: WorkloadIdentityProtocolVersion,
		Kind:            "github-actions-oidc", Audience: policy.Audience, Issuer: policy.Issuer,
		Repository: policy.Repository, Ref: policy.Ref, WorkflowRef: policy.WorkflowRef,
		RequestOrigin: policy.IdentityRequestOrigin, RequestPath: policy.IdentityRequestPath,
		RequestQuery: policy.IdentityRequestQuery,
	}
}

func validateServerPolicy(policy ServerPolicy) error {
	lease := time.Duration(policy.LeaseMillis) * time.Millisecond
	retirementTimeout := time.Duration(policy.RetirementTimeoutMillis) * time.Millisecond
	retention := time.Duration(policy.TombstoneRetentionMillis) * time.Millisecond
	identity := policy.ExpectedWorkloadIdentity()
	if policy.SchemaVersion != ServerPolicySchemaVersion ||
		policy.Issuer != GitHubActionsOIDCIssuer || !canonicalControllerOrigin(policy.ControllerOrigin) ||
		!validProfileID(policy.ProfileID) ||
		!validExpectedIdentity(identity) || lease <= 0 || lease > maximumControlLease ||
		retirementTimeout <= 0 || retirementTimeout > maximumRetirementTimeout ||
		retention <= 0 || retention > maximumTombstoneRetention ||
		policy.MaximumTombstones < 1 || policy.MaximumTombstones > maximumBrokerCapacity ||
		policy.MaximumOIDCReplays < 1 || policy.MaximumOIDCReplays > maximumBrokerCapacity {
		return errors.New("credential broker server policy is invalid")
	}
	return nil
}
