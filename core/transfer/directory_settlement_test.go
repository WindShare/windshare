package transfer

import (
	"testing"

	"github.com/windshare/windshare/core/transfer/fault"
)

func TestDirectorySettlementIsAnExactImmutableSum(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x26)
	intent := admissionTestIntent(t, root, 0x86)
	scope := admissionTestScope(t, intent)
	admission, err := NewDirectoryAdmissionWithSecret(
		admissionTestSequence(0xa0, directoryAdmissionSecretBytes),
		scope,
		OutputDirectory{DirectoryID: root, Generation: admissionTestGeneration(t, 0x56)},
	)
	if err != nil {
		t.Fatal(err)
	}

	finalized, err := NewFinalizedDirectorySettlement(admission)
	if err != nil {
		t.Fatal(err)
	}
	if finalized.Kind() != DirectoryFinalized || !finalized.Admission().Equal(admission) {
		t.Fatalf("finalized=%+v", finalized)
	}
	if _, isolated := finalized.IsolatedFault(); isolated {
		t.Fatal("finalized settlement exposed an isolated fault")
	}

	metadataFault, err := fault.NewOutput(fault.ScopeDirectoryLocal, fault.OutputDirectoryMetadata)
	if err != nil {
		t.Fatal(err)
	}
	isolated, err := NewIsolatedDirectorySettlement(admission, metadataFault)
	if err != nil {
		t.Fatal(err)
	}
	gotFault, ok := isolated.IsolatedFault()
	if isolated.Kind() != DirectoryIsolatedFailure || !isolated.Admission().Equal(admission) ||
		!ok || gotFault != metadataFault {
		t.Fatalf("isolated=%+v fault=%+v ok=%v", isolated, gotFault, ok)
	}
	if retry, err := NewIsolatedDirectorySettlement(admission, metadataFault); err != nil || retry != isolated {
		t.Fatalf("retry=%+v error=%v", retry, err)
	}
}

func TestDirectorySettlementRejectsNonTerminalOrWiderFaults(t *testing.T) {
	root := admissionTestDirectoryID(t, 0x27)
	intent := admissionTestIntent(t, root, 0x87)
	scope := admissionTestScope(t, intent)
	admission, err := NewDirectoryAdmissionWithSecret(
		admissionTestSequence(0xb0, directoryAdmissionSecretBytes),
		scope,
		OutputDirectory{DirectoryID: root, Generation: admissionTestGeneration(t, 0x57)},
	)
	if err != nil {
		t.Fatal(err)
	}
	wrongCode, _ := fault.NewOutput(fault.ScopeDirectoryLocal, fault.OutputDirectoryBinding)
	widerFault, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputDirectoryMetadata)
	foreignDomain, _ := fault.NewCatalog(fault.ScopeDirectoryLocal, fault.CatalogInvalidGeneration)
	for name, input := range map[string]struct {
		admission DirectoryAdmission
		failure   fault.Fault
	}{
		"zero admission": {failure: wrongCode},
		"zero fault":     {admission: admission},
		"binding fault":  {admission: admission, failure: wrongCode},
		"pause fault":    {admission: admission, failure: widerFault},
		"foreign domain": {admission: admission, failure: foreignDomain},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewIsolatedDirectorySettlement(input.admission, input.failure); err == nil {
				t.Fatal("invalid isolated settlement was accepted")
			}
		})
	}
	if _, err := NewFinalizedDirectorySettlement(DirectoryAdmission{}); err == nil {
		t.Fatal("zero admission finalized")
	}
}
