//go:build darwin

package outputdarwin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"golang.org/x/sys/unix"
)

const (
	temporaryStageNonceBytes = 16
	filePermissionBits       = 0o777
)

type publicFileProfile struct {
	mode     uint32
	group    uint32
	security []byte
}

func (dir *directory) CreateProcessStage(stageDirectory outputcap.Directory, name string, size int64) (outputcap.MutableFile, error) {
	stage, ok := stageDirectory.(*directory)
	if !ok || !stage.private || size < 0 {
		return nil, outputcap.ErrUnsafeNamespace
	}
	if err := errors.Join(dir.ValidateCreateAuthority(), stage.ValidateCreateAuthority(), validComponent(name)); err != nil {
		return nil, err
	}
	if dir.object.device != stage.object.device {
		return nil, outputcap.ErrUnsafeNamespace
	}
	profile, err := dir.publicFileProfile()
	if err != nil {
		return nil, err
	}
	return stage.createProfiledStage(name, size, profile)
}

func (dir *directory) publicFileProfile() (publicFileProfile, error) {
	var nonce [temporaryStageNonceBytes]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return publicFileProfile{}, err
	}
	temporary := ".windshare-stage-" + hex.EncodeToString(nonce[:])
	template, err := dir.CreateFile(temporary, false, 0)
	if err != nil {
		return publicFileProfile{}, err
	}
	state, _ := stateFrom(template)
	profile, readErr := readPublicFileProfile(state)
	// Darwin has no O_TMPFILE. Sample native umask/group/ACL inheritance with an
	// empty inode, then retire it. A peer that opened this public name must never
	// retain a descriptor to the inode that will contain downloaded plaintext.
	err = errors.Join(readErr, dir.RemoveFile(temporary, template), template.Close())
	return profile, err
}

func readPublicFileProfile(state *fileState) (publicFileProfile, error) {
	if err := state.verify(); err != nil {
		return publicFileProfile{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(state.file.Fd()), &stat); err != nil {
		return publicFileProfile{}, nativeError(err)
	}
	security, err := readSecurity(int(state.file.Fd()))
	return publicFileProfile{
		mode:     uint32(stat.Mode) & filePermissionBits,
		group:    stat.Gid,
		security: security,
	}, err
}

func (dir *directory) createProfiledStage(name string, size int64, profile publicFileProfile) (outputcap.MutableFile, error) {
	if !dir.private {
		return nil, outputcap.ErrUnsafeNamespace
	}
	file, err := dir.CreateFile(name, false, 0)
	if err != nil {
		return nil, err
	}
	state, _ := stateFrom(file)
	if err := applyPublicFileProfile(state, profile); err != nil {
		return nil, errors.Join(err, dir.RemoveFile(name, file), file.Close())
	}
	if err := state.file.Truncate(size); err != nil {
		return nil, errors.Join(err, dir.RemoveFile(name, file), file.Close())
	}
	return file, nil
}

func applyPublicFileProfile(state *fileState, profile publicFileProfile) error {
	if err := state.verifyOrigin(); err != nil {
		return err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(state.file.Fd()), &stat); err != nil {
		return nativeError(err)
	}
	// Avoid asking for a group change when both parents already inherit the
	// same group; creation may be authorized by ACL without group membership.
	var group *uint32
	if stat.Gid != profile.group {
		group = &profile.group
	}
	currentSecurity, err := readSecurity(int(state.file.Fd()))
	if err != nil {
		return err
	}
	// Preserve ordinary ACL-free output without imposing an ACL-setting
	// capability. The common profile already matches and needs no mutation.
	if !hasSecurityACL(profile.security) && !hasSecurityACL(currentSecurity) {
		if group != nil {
			if err := unix.Fchown(int(state.file.Fd()), -1, int(*group)); err != nil {
				return nativeError(err)
			}
		}
		if uint32(stat.Mode)&filePermissionBits != profile.mode {
			return nativeError(unix.Fchmod(int(state.file.Fd()), profile.mode))
		}
		return nil
	}
	return setSecurityAttributes(int(state.file.Fd()), profile.mode, group, profile.security)
}
