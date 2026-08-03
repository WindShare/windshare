//go:build windows

package windowsbroker

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type tokenSecurityAttributeV1 struct {
	Name       windows.NTUnicodeString
	ValueType  uint16
	Reserved   uint16
	Flags      uint32
	ValueCount uint32
	Values     unsafe.Pointer
}

type tokenSecurityAttributesInformation struct {
	Version        uint16
	Reserved       uint16
	AttributeCount uint32
	Attributes     *tokenSecurityAttributeV1
}

type tokenAppContainerInformation struct {
	SID *windows.SID
}

type appContainerProcessClaim struct {
	Value0 uint64
	Value1 uint64
}

func verifyPrivateAppContainerProcess(process windows.Handle, identity appContainerIdentity) error {
	if process == 0 || process == windows.InvalidHandle {
		return errors.New("AppContainer process handle is unavailable")
	}
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("open AppContainer process token: %w", err)
	}
	return errors.Join(verifyPrivateAppContainerToken(token, identity), token.Close())
}

func verifyPrivateAppContainerToken(token windows.Token, identity appContainerIdentity) error {
	const (
		tokenIsAppContainer = 29
	)
	if identity.traditionalUserSID == nil || identity.packageSID == nil || identity.isolationCapabilitySID == nil {
		return errors.New("expected trusted user, AppContainer package, or isolation capability SID is unavailable")
	}
	isAppContainer, err := tokenUint32(token, tokenIsAppContainer)
	if err != nil || isAppContainer != 1 {
		return errors.Join(fmt.Errorf("token AppContainer marker is %d, want 1", isAppContainer), err)
	}
	observedPackageSID, err := appContainerSIDForToken(token)
	if err != nil {
		return fmt.Errorf("query token AppContainer SID: %w", err)
	}
	if !observedPackageSID.Equals(identity.packageSID) {
		return errors.New("token AppContainer SID does not match the ephemeral package")
	}
	observedUserSID, err := tokenUserSID(token)
	if err != nil {
		return fmt.Errorf("query AppContainer traditional user SID: %w", err)
	}
	if !observedUserSID.Equals(identity.traditionalUserSID) {
		return errors.New("AppContainer token does not retain the trusted invoking user")
	}
	capabilities, err := capabilitySIDsForToken(token)
	if err != nil {
		return fmt.Errorf("query private AppContainer capability: %w", err)
	}
	if len(capabilities) != 1 || !capabilities[0].Equals(identity.isolationCapabilitySID) {
		return fmt.Errorf("token capability set does not contain exactly the private isolation authority")
	}
	if err := verifyIsolationCapabilitySID(capabilities[0]); err != nil {
		return err
	}
	restrictions, err := tokenGroupCount(token, windows.TokenRestrictedSids)
	if err != nil || restrictions != 0 {
		return errors.Join(fmt.Errorf("token restriction SID count is %d, want 0", restrictions), err)
	}
	if err := verifyLowIntegrityToken(token); err != nil {
		return err
	}
	if err := verifyTokenHasNoEnabledPrivileges(token); err != nil {
		return err
	}
	attributes, err := tokenSecurityAttributeNames(token)
	if err != nil {
		return fmt.Errorf("query token security claims: %w", err)
	}
	// An ephemeral profile is an unpackaged AppContainer. Windows therefore
	// emits the process-unique claim but correctly does not forge the SYSAPPID
	// claim reserved for registered AppX/MSIX package identities.
	if !attributes["TSA://ProcUnique"] || attributes["WIN://SYSAPPID"] {
		names := make([]string, 0, len(attributes))
		for name := range attributes {
			names = append(names, name)
		}
		sort.Strings(names)
		return fmt.Errorf("unpackaged AppContainer claims are invalid (claims=%v)", names)
	}
	if _, err := appContainerProcessClaimForToken(token); err != nil {
		return err
	}
	return nil
}

func verifyLowIntegrityToken(token windows.Token) error {
	const securityMandatoryLowRID = 0x00001000
	buffer, err := tokenInformationBuffer(token, windows.TokenIntegrityLevel)
	if err != nil {
		return fmt.Errorf("query AppContainer integrity level: %w", err)
	}
	label := (*windows.SIDAndAttributes)(unsafe.Pointer(&buffer[0]))
	rid, err := sidLastSubAuthority(label.Sid)
	if err != nil {
		return fmt.Errorf("AppContainer integrity SID is invalid: %w", err)
	}
	if rid != securityMandatoryLowRID {
		return fmt.Errorf("AppContainer integrity RID is 0x%08x, want low integrity", rid)
	}
	return nil
}

func sidLastSubAuthority(sid *windows.SID) (uint32, error) {
	if sid == nil || !sid.IsValid() {
		return 0, errors.New("SID is invalid")
	}
	// x/sys obtains sub-authority pointers through uintptr-returning Windows APIs.
	// The canonical string keeps checkptr provenance intact for SIDs backed by a
	// Go-owned token-information buffer.
	value := sid.String()
	separator := strings.LastIndexByte(value, '-')
	if separator < 0 || separator == len(value)-1 {
		return 0, fmt.Errorf("SID %q has no terminal sub-authority", value)
	}
	parsed, err := strconv.ParseUint(value[separator+1:], 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse terminal sub-authority from SID %q: %w", value, err)
	}
	return uint32(parsed), nil
}

func verifyTokenHasNoEnabledPrivileges(token windows.Token) error {
	name, err := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
	if err != nil {
		return err
	}
	var traversalPrivilege windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &traversalPrivilege); err != nil {
		return fmt.Errorf("resolve standard traversal privilege: %w", err)
	}
	buffer, err := tokenInformationBuffer(token, windows.TokenPrivileges)
	if err != nil {
		return fmt.Errorf("query AppContainer privileges: %w", err)
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&buffer[0]))
	for _, privilege := range privileges.AllPrivileges() {
		if privilege.Attributes&windows.SE_PRIVILEGE_ENABLED != 0 && privilege.Luid != traversalPrivilege {
			return fmt.Errorf(
				"AppContainer token retains enabled privilege LUID %d:%d",
				privilege.Luid.HighPart,
				privilege.Luid.LowPart,
			)
		}
	}
	// Windows keeps SeChangeNotifyPrivilege enabled even in a native AppContainer.
	// It only bypasses directory traverse checks; it cannot bypass the final
	// object DACL or the AppContainer restricted-token intersection.
	return nil
}

func appContainerSIDForToken(token windows.Token) (*windows.SID, error) {
	const tokenAppContainerSID = 31
	var required uint32
	err := windows.GetTokenInformation(token, tokenAppContainerSID, nil, 0, &required)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || required < uint32(unsafe.Sizeof(tokenAppContainerInformation{})) {
		return nil, err
	}
	words := make([]uint64, (required+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)[:required]
	if err := windows.GetTokenInformation(token, tokenAppContainerSID, &buffer[0], required, &required); err != nil {
		return nil, err
	}
	information := (*tokenAppContainerInformation)(unsafe.Pointer(&buffer[0]))
	if information.SID == nil {
		return nil, errors.New("token has no AppContainer package SID")
	}
	return information.SID.Copy()
}

func tokenUint32(token windows.Token, informationClass uint32) (uint32, error) {
	var value uint32
	var returned uint32
	err := windows.GetTokenInformation(
		token,
		informationClass,
		(*byte)(unsafe.Pointer(&value)),
		uint32(unsafe.Sizeof(value)),
		&returned,
	)
	return value, err
}

func tokenGroupCount(token windows.Token, informationClass uint32) (uint32, error) {
	buffer, err := tokenInformationBuffer(token, informationClass)
	if err != nil {
		return 0, err
	}
	return *(*uint32)(unsafe.Pointer(&buffer[0])), nil
}

func tokenInformationBuffer(token windows.Token, informationClass uint32) ([]byte, error) {
	var required uint32
	err := windows.GetTokenInformation(token, informationClass, nil, 0, &required)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || required < uint32(unsafe.Sizeof(uint32(0))) {
		return nil, err
	}
	words := make([]uint64, (required+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)[:required]
	if err := windows.GetTokenInformation(token, informationClass, &buffer[0], required, &required); err != nil {
		return nil, err
	}
	return buffer, nil
}

func tokenSecurityAttributeNames(token windows.Token) (map[string]bool, error) {
	buffer, err := tokenSecurityAttributes(token)
	if err != nil {
		return nil, err
	}
	information := (*tokenSecurityAttributesInformation)(unsafe.Pointer(&buffer[0]))
	if information.Version != 1 || information.AttributeCount > 1024 ||
		(information.AttributeCount > 0 && information.Attributes == nil) {
		return nil, errors.New("private AppContainer token security attributes are invalid")
	}
	result := make(map[string]bool, information.AttributeCount)
	for _, attribute := range unsafe.Slice(information.Attributes, information.AttributeCount) {
		if attribute.Name.Buffer == nil || attribute.Name.Length%2 != 0 {
			return nil, errors.New("private AppContainer token contains an invalid security attribute name")
		}
		name := windows.UTF16ToString(unsafe.Slice(attribute.Name.Buffer, attribute.Name.Length/2))
		result[name] = true
	}
	return result, nil
}

func appContainerProcessClaimForToken(token windows.Token) (appContainerProcessClaim, error) {
	const (
		claimSecurityAttributeTypeUint64     = 0x0002
		claimSecurityAttributeNonInheritable = 0x0001
		claimSecurityAttributeUnique         = 0x0040
	)
	buffer, err := tokenSecurityAttributes(token)
	if err != nil {
		return appContainerProcessClaim{}, err
	}
	information := (*tokenSecurityAttributesInformation)(unsafe.Pointer(&buffer[0]))
	if information.AttributeCount > 1024 || (information.AttributeCount > 0 && information.Attributes == nil) {
		return appContainerProcessClaim{}, errors.New("private AppContainer token security attributes are invalid")
	}
	for _, attribute := range unsafe.Slice(information.Attributes, information.AttributeCount) {
		if attribute.Name.Buffer == nil || attribute.Name.Length%2 != 0 {
			continue
		}
		name := windows.UTF16ToString(unsafe.Slice(attribute.Name.Buffer, attribute.Name.Length/2))
		if name != "TSA://ProcUnique" {
			continue
		}
		if attribute.ValueType != claimSecurityAttributeTypeUint64 || attribute.ValueCount != 2 ||
			attribute.Values == nil || attribute.Flags&(claimSecurityAttributeNonInheritable|claimSecurityAttributeUnique) !=
			claimSecurityAttributeNonInheritable|claimSecurityAttributeUnique {
			return appContainerProcessClaim{}, fmt.Errorf(
				"AppContainer process claim shape is type=%d flags=0x%x values=%d",
				attribute.ValueType,
				attribute.Flags,
				attribute.ValueCount,
			)
		}
		values := unsafe.Slice((*uint64)(attribute.Values), attribute.ValueCount)
		if values[0] == 0 || values[1] == 0 {
			return appContainerProcessClaim{}, errors.New("AppContainer process claim contains a zero identity component")
		}
		return appContainerProcessClaim{Value0: values[0], Value1: values[1]}, nil
	}
	return appContainerProcessClaim{}, errors.New("AppContainer process claim is unavailable")
}

func tokenSecurityAttributes(token windows.Token) ([]byte, error) {
	const tokenSecurityAttributes = 39
	var required uint32
	err := windows.GetTokenInformation(token, tokenSecurityAttributes, nil, 0, &required)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || required == 0 {
		return nil, err
	}
	words := make([]uint64, (required+7)/8)
	buffer := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), len(words)*8)[:required]
	if err := windows.GetTokenInformation(
		token, tokenSecurityAttributes, &buffer[0], required, &required,
	); err != nil {
		return nil, err
	}
	return buffer, nil
}

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
