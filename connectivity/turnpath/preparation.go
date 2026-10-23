package turnpath

// Preparing a successor is independent of serving on the current path. Keeping
// these dimensions separate preserves make-before-break without allowing a
// credential request and a delegated candidate to own the same preparation slot.
type preparationPhase string

const (
	preparationIdle       preparationPhase = "idle"
	preparationIssuing    preparationPhase = "issuing"
	preparationConnecting preparationPhase = "connecting"
)

type preparationState interface {
	preparationPhase() preparationPhase
}

func (*credentialRequest) preparationPhase() preparationPhase { return preparationIssuing }
func (*delegation) preparationPhase() preparationPhase        { return preparationConnecting }

type pathPreparation struct {
	state preparationState
}

func (p *pathPreparation) phase() preparationPhase {
	if p.state == nil {
		return preparationIdle
	}
	return p.state.preparationPhase()
}

func (p *pathPreparation) issuance() *credentialRequest {
	request, _ := p.state.(*credentialRequest)
	return request
}

func (p *pathPreparation) pending() *delegation {
	path, _ := p.state.(*delegation)
	return path
}

func (p *pathPreparation) begin(request *credentialRequest) bool {
	if request == nil || p.state != nil {
		return false
	}
	p.state = request
	return true
}

// Only the completion of the owning request can release the slot. A late
// result must not clear the next demand's preparation.
func (p *pathPreparation) complete(request *credentialRequest) bool {
	if request == nil || p.issuance() != request {
		return false
	}
	p.state = nil
	return true
}

func (p *pathPreparation) connect(path *delegation) bool {
	if path == nil || p.state != nil {
		return false
	}
	p.state = path
	return true
}

// Detaching transfers the candidate out of preparation; the caller decides
// whether it becomes the serving path or its resources are retired.
func (p *pathPreparation) detach(path *delegation) bool {
	if path == nil || p.pending() != path {
		return false
	}
	p.state = nil
	return true
}
