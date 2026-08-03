//go:build linux

package linuxsubreaper

import (
	"errors"
	"time"

	"golang.org/x/sys/unix"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

const execResultTerminalDrainLimit = 500 * time.Millisecond

type terminalResult struct {
	evidence ownerprotocol.TargetEvidence
	err      error
}

type ownedTargetLifecycleAuthority interface {
	refreshOwnedTree() error
	requestTermination(unix.Signal) (terminationSignalWitness, error)
	naturalTreeComplete() (bool, error)
}

func monitorOwnedTarget(
	state supervisionState,
	authority ownedTargetLifecycleAuthority,
	wait <-chan terminalResult,
	control <-chan string,
	deadline <-chan time.Time,
	ticks <-chan time.Time,
) supervisionState {
	terminationRequested := !state.launched() || state.terminationReason != ownerprotocol.TerminationNatural
	for !terminationRequested && state.authorityFailure == nil {
		state = probeRootTerminal(state, wait)
		if state.rootTerminal != nil {
			complete, err := authority.naturalTreeComplete()
			if err != nil {
				state.authorityFailure = err
				break
			}
			if complete {
				break
			}
		}
		select {
		case result := <-wait:
			state.rootTerminal = &result
		case outcome := <-control:
			state, terminationRequested = linearizeTerminationRequest(state, authority, wait, outcome)
		case <-deadline:
			state, terminationRequested = linearizeTerminationRequest(
				state,
				authority,
				wait,
				ownerprotocol.TerminationDeadline,
			)
		case <-ticks:
			state.authorityFailure = authority.refreshOwnedTree()
		}
	}
	if state.authorityFailure != nil {
		state.terminationReason = ownerprotocol.TerminationOwnerFailure
	}
	return state
}

func linearizeTerminationRequest(
	state supervisionState,
	authority ownedTargetLifecycleAuthority,
	wait <-chan terminalResult,
	reason string,
) (supervisionState, bool) {
	for {
		state = probeRootTerminal(state, wait)
		if state.rootTerminal != nil {
			complete, err := authority.naturalTreeComplete()
			if err != nil {
				state.authorityFailure = err
				return state, false
			}
			if complete {
				return state, false
			}
		}

		witness, err := authority.requestTermination(unix.SIGTERM)
		if err != nil {
			state.authorityFailure = err
			return state, false
		}
		if witness.applied() {
			state.terminationReason = reason
			if reason == ownerprotocol.TerminationOwnerFailure {
				state.authorityFailure = errors.New("control authority lost its framing or identity")
			}
			return state, true
		}

		// A successful pidfd signal is the only authority that lets a request
		// claim causality. ESRCH for every retained identity proves that the
		// kernel had already made them terminal; join the exact Wait publication
		// before reaping adopted children so Wait4 cannot steal root evidence.
		if state.rootTerminal == nil {
			state = awaitRootTerminalPublication(state, wait)
			if state.authorityFailure != nil {
				return state, false
			}
		}
		complete, err := authority.naturalTreeComplete()
		if err != nil {
			state.authorityFailure = err
			return state, false
		}
		if complete {
			return state, false
		}
		// naturalTreeComplete refreshes and retains any descendant that became
		// visible during root exit. Retry immediately so that a live exact pidfd,
		// rather than a stale inventory count, decides causality.
	}
}

func awaitRootTerminalPublication(
	state supervisionState,
	wait <-chan terminalResult,
) supervisionState {
	timer := time.NewTimer(execResultTerminalDrainLimit)
	defer timer.Stop()
	select {
	case result := <-wait:
		state.rootTerminal = &result
	case <-timer.C:
		state.authorityFailure = errors.New("kernel-terminal root did not publish exact Wait evidence within its bounded join")
	}
	return state
}

func probeRootTerminal(state supervisionState, wait <-chan terminalResult) supervisionState {
	if state.rootTerminal != nil {
		return state
	}
	select {
	case result := <-wait:
		state.rootTerminal = &result
	default:
	}
	return state
}

func (authority *inventoryAuthority) refreshOwnedTree() error {
	_, err := authority.refresh()
	return err
}

func (authority *inventoryAuthority) requestTermination(
	signal unix.Signal,
) (terminationSignalWitness, error) {
	if _, err := authority.refresh(); err != nil {
		return terminationSignalWitness{}, err
	}
	return authority.signalTrackedWithWitness(signal)
}

func (authority *inventoryAuthority) naturalTreeComplete() (bool, error) {
	noChildren, reapErr := reapAdoptedChildren()
	inventory, inventoryErr := authority.refresh()
	if err := errors.Join(reapErr, inventoryErr); err != nil {
		return false, err
	}
	return noChildren && len(inventory) == 0, nil
}
