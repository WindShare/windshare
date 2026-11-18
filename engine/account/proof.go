package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"math/bits"
	"strconv"
	"time"
)

const proofPollHashes = 4096
const proofProgressInterval = 250 * time.Millisecond

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
	if challenge.ID == "" || len(challenge.ID) > 64 || challenge.Bits == 0 || challenge.Bits > 32 {
		return "", errors.New("invalid proof of work")
	}
	started, progress := time.Now(), time.Now()
	prefix := []byte(challenge.ID)
	buffer := make([]byte, len(prefix), len(prefix)+20)
	copy(buffer, prefix)
	for nonce := uint64(0); ; nonce++ {
		input := strconv.AppendUint(buffer[:len(prefix)], nonce, 10)
		if proofMatches(input, challenge.Bits) {
			return strconv.FormatUint(nonce, 10), nil
		}
		if nonce%proofPollHashes == 0 {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			if uint64(service.config.Now().Unix()) >= challenge.ExpiresAt {
				return "", errors.New("proof of work expired")
			}
			if time.Since(progress) >= proofProgressInterval {
				service.mu.Lock()
				service.hashes, service.proofElapsed = nonce+1, time.Since(started)
				service.notifyLocked()
				service.mu.Unlock()
				progress = time.Now()
			}
		}
	}
}
