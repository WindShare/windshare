package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"math"
	"math/bits"
	"strconv"
	"time"
)

const (
	maximumWorkBits         = 32
	maximumChallengeIDBytes = 64
	proofPollHashes         = 4096
	proofProgressInterval   = 250 * time.Millisecond
	registrationWorkStart   = 50
	registrationWorkEnd     = 950
	registrationSubmitting  = 960
)

var errInvalidRegistrationChallenge = errors.New("invalid account challenge")

func (challenge Challenge) validWork() bool {
	return challenge.Mode == workChallenge && challenge.ID != "" && len(challenge.ID) <= maximumChallengeIDBytes &&
		challenge.Bits > 0 && challenge.Bits <= maximumWorkBits
}

// Work has no fixed length. The chance of a solution within this many attempts
// gives a bounded estimate while reserving the last few percent for issuance.
func estimatedRegistrationPermille(attempts uint64, difficulty uint8) uint16 {
	probability := -math.Expm1(-math.Ldexp(float64(attempts), -int(difficulty)))
	return registrationWorkStart + uint16(probability*(registrationWorkEnd-registrationWorkStart))
}

func (service *Service) setRegistrationProgress(permille uint16) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.state == Registering && permille > service.registrationPermille {
		service.registrationPermille = permille
		service.notifyLocked()
	}
}

func proofMatches(input []byte, difficulty uint8) bool {
	digest := sha256.Sum256(input)
	zeroes := 0
	for _, value := range digest {
		zeroes += bits.LeadingZeros8(value)
		if value != 0 {
			break
		}
	}
	return zeroes >= int(difficulty)
}

func (service *Service) solve(ctx context.Context, challenge Challenge) (string, error) {
	if !challenge.validWork() {
		return "", errInvalidRegistrationChallenge
	}
	progress := time.Now()
	prefix := []byte(challenge.ID)
	buffer := make([]byte, len(prefix), len(prefix)+20)
	copy(buffer, prefix)
	for nonce := uint64(0); ; nonce++ {
		if nonce%proofPollHashes == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if uint64(service.config.Now().Unix()) >= challenge.ExpiresAt {
				return "", errors.New("account setup expired, please try again")
			}
			if time.Since(progress) >= proofProgressInterval {
				service.setRegistrationProgress(estimatedRegistrationPermille(nonce, challenge.Bits))
				progress = time.Now()
			}
		}
		input := strconv.AppendUint(buffer[:len(prefix)], nonce, 10)
		if proofMatches(input, challenge.Bits) {
			return strconv.FormatUint(nonce, 10), nil
		}
	}
}
