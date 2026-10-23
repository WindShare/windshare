package turnpath

import "testing"

func TestPreparationKeepsServingPathAndRejectsStaleOwners(t *testing.T) {
	current, candidate := &delegation{}, &delegation{}
	service := &service{current: current}
	request, other := &credentialRequest{}, &credentialRequest{}
	preparation := &service.preparation
	if preparation.phase() != preparationIdle || !preparation.begin(request) {
		t.Fatal("idle service cannot begin preparation")
	}
	if preparation.begin(other) || preparation.connect(candidate) || preparation.complete(other) {
		t.Fatal("another operation displaced the credential owner")
	}
	if preparation.phase() != preparationIssuing || preparation.issuance() != request || preparation.pending() != nil {
		t.Fatal("issuance and candidate ownership overlap")
	}
	if !preparation.complete(request) || !preparation.connect(candidate) {
		t.Fatal("completed issuance cannot delegate its candidate")
	}
	if preparation.complete(request) || preparation.detach(&delegation{}) {
		t.Fatal("stale completion or retirement displaced the candidate")
	}
	if preparation.phase() != preparationConnecting || preparation.pending() != candidate || preparation.issuance() != nil {
		t.Fatal("candidate ownership was lost")
	}
	if !preparation.detach(candidate) || preparation.phase() != preparationIdle || service.current != current {
		t.Fatal("failed successor interrupted the serving path")
	}
	if preparation.begin(nil) || preparation.connect(nil) || preparation.complete(nil) || preparation.detach(nil) {
		t.Fatal("absent operation acquired preparation ownership")
	}
}
