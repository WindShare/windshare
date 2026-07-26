package resumestate

import (
	"fmt"

	"github.com/windshare/windshare/core/transfer"
)

const (
	MaxCertificationIDBytes                                = 128
	CertificationLinuxExt4ProcessRestart   CertificationID = "linux/ext4/process-restart/v1"
	CertificationWindowsNTFSProcessRestart CertificationID = "windows/ntfs/process-restart/v1"
)

type CertificationID string

func NewCertificationID(value string) (CertificationID, error) {
	certification := CertificationID(value)
	switch certification {
	case CertificationLinuxExt4ProcessRestart, CertificationWindowsNTFSProcessRestart:
		return certification, nil
	default:
		return "", fmt.Errorf("%w: filesystem certification ID", ErrInvalidState)
	}
}

type ControlSpec struct {
	Backend       transfer.OutputBackendID
	OutputRoot    OutputRootBinding
	Certification CertificationID
	Durability    transfer.DurabilityLevel
	Generation    uint64
}

type Control struct {
	backend       transfer.OutputBackendID
	outputRoot    OutputRootBinding
	certification CertificationID
	durability    transfer.DurabilityLevel
	generation    uint64
}

func NewControl(spec ControlSpec) (Control, error) {
	backend, backendErr := transfer.NewOutputBackendID(string(spec.Backend))
	certification, certificationErr := NewCertificationID(string(spec.Certification))
	// The initial certification claim is intentionally narrow. A later power-loss
	// claim needs a new, fault-tested certification rather than a flag flip.
	if backendErr != nil || certificationErr != nil || !spec.OutputRoot.valid() ||
		spec.OutputRoot.Certification() != certification ||
		spec.Durability != transfer.DurabilityProcessRestart || spec.Generation == 0 {
		return Control{}, fmt.Errorf("%w: global control record", ErrInvalidState)
	}
	return Control{
		backend: backend, outputRoot: spec.OutputRoot, certification: certification,
		durability: spec.Durability, generation: spec.Generation,
	}, nil
}

func (control Control) Backend() transfer.OutputBackendID    { return control.backend }
func (control Control) OutputRoot() OutputRootBinding        { return control.outputRoot }
func (control Control) Certification() CertificationID       { return control.certification }
func (control Control) Durability() transfer.DurabilityLevel { return control.durability }
func (control Control) Generation() uint64                   { return control.generation }

func (control Control) valid() bool {
	rebuilt, err := NewControl(ControlSpec{
		Backend: control.backend, OutputRoot: control.outputRoot,
		Certification: control.certification, Durability: control.durability, Generation: control.generation,
	})
	return err == nil && rebuilt == control
}
