package legacyresume

import "strings"

const (
	ControlDirectory    = ".windshare-output"
	SessionsDirectory   = "sessions"
	CheckpointDirectory = "checkpoints-v1"
	ControlRecord       = "control.state"
	CoordinatorLock     = "coordinator.lock"
	SessionLock         = "session.lock"
	HeaderRecord        = "header.state"
	FilesDirectory      = "files"
	AnchorsDirectory    = "anchors"
	StagesDirectory     = "stages"

	BootstrapCandidatePrefix = ".windshare-output.bootstrap-"
	SessionCandidatePrefix   = ".candidate-"
	legacyUpdateSeparator    = ".tmp-"
	legacyRecordSuffix       = ".state"
	legacyAnchorSuffix       = ".anchor"
	legacyStageSuffix        = ".stage"
	encodedDigestCharacters  = 64
	encodedSessionCharacters = 32
)

func IsIntentDirectory(name string) bool {
	return validNonZeroLowerHex(name, encodedDigestCharacters)
}

func IsSessionDirectory(name string) bool {
	return validNonZeroLowerHex(name, encodedSessionCharacters)
}

func IsSessionCandidate(name string) bool {
	return strings.HasPrefix(name, SessionCandidatePrefix) &&
		IsSessionDirectory(strings.TrimPrefix(name, SessionCandidatePrefix))
}

func IsBootstrapCandidate(name string) bool {
	return strings.HasPrefix(name, BootstrapCandidatePrefix) &&
		validNonZeroLowerHex(strings.TrimPrefix(name, BootstrapCandidatePrefix), encodedDigestCharacters)
}

func IsControlTemporary(name string) bool {
	return isRecordTemporary(ControlRecord, name)
}

func IsHeaderTemporary(name string) bool {
	return isRecordTemporary(HeaderRecord, name)
}

func IsShard(name string) bool {
	return validLowerHex(name, 2)
}

func IsFileRecord(shard, name string) bool {
	return isShardedName(shard, name, legacyRecordSuffix)
}

func IsFileRecordTemporary(shard, name string) bool {
	if len(name) <= encodedDigestCharacters+len(legacyRecordSuffix) {
		return false
	}
	target := name[:encodedDigestCharacters] + legacyRecordSuffix
	return isShardedName(shard, target, legacyRecordSuffix) && isRecordTemporary(target, name)
}

func IsAnchor(shard, name string) bool {
	return isShardedName(shard, name, legacyAnchorSuffix)
}

func IsStage(shard, name string) bool {
	return isShardedName(shard, name, legacyStageSuffix)
}

func isRecordTemporary(target, name string) bool {
	prefix := target + legacyUpdateSeparator
	return strings.HasPrefix(name, prefix) &&
		validNonZeroLowerHex(strings.TrimPrefix(name, prefix), encodedDigestCharacters)
}

func isShardedName(shard, name, suffix string) bool {
	if !IsShard(shard) || !strings.HasSuffix(name, suffix) {
		return false
	}
	base := strings.TrimSuffix(name, suffix)
	return validNonZeroLowerHex(base, encodedDigestCharacters) && strings.HasPrefix(base, shard)
}

func validNonZeroLowerHex(value string, length int) bool {
	return validLowerHex(value, length) && strings.ContainsAny(value, "123456789abcdef")
}

func validLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}
