//go:build windows

package mutationdomain

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	isolationCapabilityNamePrefix        = "WindShare.Performance.Isolation."
	isolationCapabilitySIDPrefix         = "S-1-15-3-1024-"
	isolationCapabilitySIDComponentCount = 13
)

var deriveCapabilitySIDsFromName = windows.NewLazySystemDLL("kernelbase.dll").NewProc("DeriveCapabilitySidsFromName")

type appContainerIdentity struct {
	traditionalUserSID     *windows.SID
	packageSID             *windows.SID
	isolationCapabilitySID *windows.SID
}

func newIsolationCapabilitySID() (*windows.SID, error) {
	entropy, err := randomBytes(16)
	if err != nil {
		return nil, err
	}
	name := isolationCapabilityNamePrefix + hex.EncodeToString(entropy)
	return deriveIsolationCapabilitySID(name)
}

func deriveIsolationCapabilitySID(name string) (_ *windows.SID, resultErr error) {
	if len(name) != len(isolationCapabilityNamePrefix)+appContainerProfileEntropyHexBytes ||
		name[:len(isolationCapabilityNamePrefix)] != isolationCapabilityNamePrefix {
		return nil, fmt.Errorf("private capability name %q is outside the reserved namespace", name)
	}
	encodedName, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var groupSIDs **windows.SID
	var groupCount uint32
	var capabilitySIDs **windows.SID
	var capabilityCount uint32
	result, _, callErr := deriveCapabilitySIDsFromName.Call(
		uintptr(unsafe.Pointer(encodedName)),
		uintptr(unsafe.Pointer(&groupSIDs)),
		uintptr(unsafe.Pointer(&groupCount)),
		uintptr(unsafe.Pointer(&capabilitySIDs)),
		uintptr(unsafe.Pointer(&capabilityCount)),
	)
	defer func() {
		resultErr = errors.Join(
			resultErr,
			freeDerivedSIDs(groupSIDs, groupCount),
			freeDerivedSIDs(capabilitySIDs, capabilityCount),
		)
	}()
	if result == 0 {
		return nil, fmt.Errorf("derive private capability SID: %w", callErr)
	}
	if groupCount != 1 || groupSIDs == nil || capabilityCount != 1 || capabilitySIDs == nil {
		return nil, fmt.Errorf(
			"derive private capability returned groups=%d capabilities=%d, want one each",
			groupCount,
			capabilityCount,
		)
	}
	capability := unsafe.Slice(capabilitySIDs, capabilityCount)[0]
	if capability == nil {
		return nil, errors.New("derived private capability SID is unavailable")
	}
	copy, err := capability.Copy()
	if err != nil {
		return nil, err
	}
	if err := verifyIsolationCapabilitySID(copy); err != nil {
		return nil, err
	}
	return copy, nil
}

func freeDerivedSIDs(sids **windows.SID, count uint32) error {
	if sids == nil {
		if count != 0 {
			return errors.New("derived capability SID array is nil with a nonzero count")
		}
		return nil
	}
	var errs []error
	for _, sid := range unsafe.Slice(sids, count) {
		if sid != nil {
			_, err := windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(sid))))
			errs = append(errs, err)
		}
	}
	_, err := windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(sids))))
	errs = append(errs, err)
	return errors.Join(errs...)
}

func verifyIsolationCapabilitySID(capability *windows.SID) error {
	if capability == nil || !capability.IsValid() {
		return errors.New("private isolation capability SID is invalid")
	}
	// Hash-derived custom capabilities live below S-1-15-3-1024 and carry eight
	// hash components. Legacy ambient capabilities such as internetClient are
	// short well-known SIDs and can therefore never alias this per-session authority.
	// The x/sys sub-authority accessors round-trip an interior pointer through
	// uintptr. Parsing Windows' canonical SID string preserves checkptr provenance
	// when the SID has intentionally been copied into Go-owned memory.
	value := capability.String()
	if !strings.HasPrefix(value, isolationCapabilitySIDPrefix) ||
		len(strings.Split(value, "-")) != isolationCapabilitySIDComponentCount {
		return fmt.Errorf("derived SID %s is not a hash-scoped private capability", value)
	}
	return nil
}

func capabilitySIDsForToken(token windows.Token) ([]*windows.SID, error) {
	const tokenCapabilities = 30
	buffer, err := tokenInformationBuffer(token, tokenCapabilities)
	if err != nil {
		return nil, err
	}
	groups := (*windows.Tokengroups)(unsafe.Pointer(&buffer[0])).AllGroups()
	result := make([]*windows.SID, 0, len(groups))
	for _, group := range groups {
		if group.Sid == nil || group.Attributes&windows.SE_GROUP_ENABLED == 0 {
			return nil, errors.New("AppContainer capability is invalid or disabled")
		}
		copy, err := group.Sid.Copy()
		if err != nil {
			return nil, err
		}
		result = append(result, copy)
	}
	return result, nil
}

func appContainerIdentityForToken(token windows.Token) (appContainerIdentity, error) {
	traditionalUserSID, err := tokenUserSID(token)
	if err != nil {
		return appContainerIdentity{}, err
	}
	packageSID, err := appContainerSIDForToken(token)
	if err != nil {
		return appContainerIdentity{}, err
	}
	capabilities, err := capabilitySIDsForToken(token)
	if err != nil {
		return appContainerIdentity{}, err
	}
	if len(capabilities) != 1 {
		return appContainerIdentity{}, fmt.Errorf("AppContainer capability count is %d, want one private authority", len(capabilities))
	}
	if err := verifyIsolationCapabilitySID(capabilities[0]); err != nil {
		return appContainerIdentity{}, err
	}
	return appContainerIdentity{
		traditionalUserSID:     traditionalUserSID,
		packageSID:             packageSID,
		isolationCapabilitySID: capabilities[0],
	}, nil
}

func tokenUserSID(token windows.Token) (*windows.SID, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return nil, errors.New("token user SID is invalid")
	}
	return user.User.Sid.Copy()
}

func currentAppContainerIdentity() (appContainerIdentity, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return appContainerIdentity{}, err
	}
	defer token.Close()
	identity, err := appContainerIdentityForToken(token)
	if err != nil {
		return appContainerIdentity{}, err
	}
	if err := verifyPrivateAppContainerToken(token, identity); err != nil {
		return appContainerIdentity{}, err
	}
	return identity, nil
}
