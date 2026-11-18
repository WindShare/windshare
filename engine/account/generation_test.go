package account

import (
	"encoding/json"
	"os"
	"testing"
)

func TestSlowerCacheReadCannotRollBackIssuerOrAllowance(t *testing.T) {
	config, response := accountFixture(t)
	previous := cachedRecord(response)
	config.Store = &MemoryStore{record: previous}
	service := testService(t, config)
	data, err := os.ReadFile("../../testdata/network/authority.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Certificate string `json:"next_certificate"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	service.mu.Lock()
	latest := cloneRecord(previous)
	latest.Certificate = fixture.Certificate
	latest.Entitlement.Version++
	latest.Entitlement.RemainingBytes = 0
	service.adoptLocked(latest)
	service.adoptLocked(previous)
	record := cloneRecord(service.record)
	service.mu.Unlock()
	if record.Certificate != fixture.Certificate || service.validTicket(record) {
		t.Fatal("slower read restored an old signing generation")
	}
	if record.Entitlement.Version != latest.Entitlement.Version || record.Entitlement.RemainingBytes != 0 {
		t.Fatal("slower read restored old allowance")
	}
}

func TestVerifiedAccountUnblockPreservesOtherCredentialRevocations(t *testing.T) {
	config, response := accountFixture(t)
	record := cachedRecord(response)
	config.Store = &MemoryStore{record: record}
	service := testService(t, config)
	other := cloneRecord(record)
	other.Credential.ID[0] ^= 1
	service.mu.Lock()
	service.credentials = map[[16]byte]*credentialSession{
		record.Credential.ID: {record: record}, other.Credential.ID: {record: other},
	}
	service.rememberDenialLocked(other, "credential_revoked")
	service.rememberDenialLocked(record, "account_blocked")
	service.issuedLocked(record)
	first := service.credentials[record.Credential.ID].record
	second := service.withDenialLocked(service.credentials[other.Credential.ID].record)
	service.mu.Unlock()
	if first.Denial != "" || !service.validTicket(first) {
		t.Fatal("verified account unblock did not release the live credential")
	}
	if second.Denial != "credential_revoked" || service.validTicket(second) {
		t.Fatal("unblocking an account restored a separately revoked credential")
	}
}
