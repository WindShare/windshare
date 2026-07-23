package v2

import (
	"bytes"
	"crypto/sha256"
	"io"
	"sync"
	"time"
)

const (
	ChallengeTTL          = 30 * time.Second
	challengeIssueRetries = 4
)

type ChallengeBinding [sha256.Size]byte

func RegistrationChallengeBinding(init RegisterInit, relayIdentity RelayIdentity) (ChallengeBinding, error) {
	encoded, err := init.MarshalBinary()
	if err != nil || !nonzero(relayIdentity[:]) {
		return ChallengeBinding{}, ErrIdentity
	}
	return sha256.Sum256(append(encoded, relayIdentity[:]...)), nil
}

func StopChallengeBinding(init StopInit) (ChallengeBinding, error) {
	encoded, err := init.MarshalBinary()
	if err != nil {
		return ChallengeBinding{}, err
	}
	return sha256.Sum256(encoded), nil
}

type ChallengeLedgerConfig struct {
	Capacity int
	Random   io.Reader
	Now      func() time.Time
}

type challengeState struct {
	challenge Challenge
	binding   ChallengeBinding
}

// ChallengeLedger is a bounded, one-use owner. Take removes a challenge before
// proof verification, so an invalid signature cannot turn it into a retry oracle.
type ChallengeLedger struct {
	mu       sync.Mutex
	capacity int
	random   io.Reader
	now      func() time.Time
	entries  map[ChallengeID]challengeState
}

func NewChallengeLedger(config ChallengeLedgerConfig) (*ChallengeLedger, error) {
	if config.Capacity <= 0 || config.Random == nil {
		return nil, ErrChallengeBudget
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &ChallengeLedger{
		capacity: config.Capacity,
		random:   config.Random,
		now:      config.Now,
		entries:  make(map[ChallengeID]challengeState),
	}, nil
}

func (l *ChallengeLedger) Issue(purpose ChallengePurpose, binding ChallengeBinding) (Challenge, error) {
	if l == nil || !purpose.valid() || !nonzero(binding[:]) {
		return Challenge{}, ErrPurpose
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.cleanup(now)
	if len(l.entries) >= l.capacity {
		return Challenge{}, ErrChallengeBudget
	}
	for range challengeIssueRetries {
		var challenge Challenge
		challenge.Purpose = purpose
		if _, err := io.ReadFull(l.random, challenge.ID[:]); err != nil {
			return Challenge{}, err
		}
		if _, err := io.ReadFull(l.random, challenge.Nonce[:]); err != nil {
			return Challenge{}, err
		}
		if !nonzero(challenge.ID[:]) || !nonzero(challenge.Nonce[:]) {
			continue
		}
		if _, exists := l.entries[challenge.ID]; exists {
			continue
		}
		if now.Unix() < 0 {
			return Challenge{}, ErrChallengeExpired
		}
		challenge.ExpiresAtUnixSeconds = uint64(now.Unix()) + uint64(ChallengeTTL/time.Second)
		l.entries[challenge.ID] = challengeState{challenge: challenge, binding: binding}
		return challenge, nil
	}
	return Challenge{}, ErrChallengeBudget
}

func (l *ChallengeLedger) Take(id ChallengeID, purpose ChallengePurpose, binding ChallengeBinding) (Challenge, error) {
	if l == nil {
		return Challenge{}, ErrChallengeConsumed
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	state, exists := l.entries[id]
	if !exists {
		return Challenge{}, ErrChallengeConsumed
	}
	delete(l.entries, id)
	now := l.now()
	if !challengeAlive(state.challenge, now) {
		return Challenge{}, ErrChallengeExpired
	}
	if state.challenge.Purpose != purpose || !bytes.Equal(state.binding[:], binding[:]) {
		return Challenge{}, ErrProof
	}
	return state.challenge, nil
}

func (l *ChallengeLedger) AuthenticateRegistration(id ChallengeID, init RegisterInit, relayIdentity RelayIdentity, proof RegisterProof) (SenderAuthority, error) {
	binding, err := RegistrationChallengeBinding(init, relayIdentity)
	if err != nil {
		return SenderAuthority{}, err
	}
	purpose, err := purposeForMode(init.Mode)
	if err != nil {
		return SenderAuthority{}, err
	}
	challenge, err := l.Take(id, purpose, binding)
	if err != nil {
		return SenderAuthority{}, err
	}
	return authenticateRegisterProof(init, challenge, relayIdentity, proof, l.now())
}

func (l *ChallengeLedger) AuthenticateStop(id ChallengeID, init StopInit, proof StopProof) (StopAuthority, error) {
	binding, err := StopChallengeBinding(init)
	if err != nil {
		return StopAuthority{}, err
	}
	challenge, err := l.Take(id, ChallengeStop, binding)
	if err != nil {
		return StopAuthority{}, err
	}
	return authenticateStopProof(init, challenge, proof, l.now())
}

func (l *ChallengeLedger) cleanup(now time.Time) {
	for id, state := range l.entries {
		if !challengeAlive(state.challenge, now) {
			delete(l.entries, id)
		}
	}
}
