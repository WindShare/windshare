//go:build windows

package mutationdomain

import (
	"github.com/windshare/windshare/internal/perfevidence/mutationdomain/windowsbroker"
	"golang.org/x/sys/windows"
)

const (
	isolationCapabilityNamePrefix        = windowsbroker.IsolationCapabilityNamePrefix
	isolationCapabilitySIDPrefix         = windowsbroker.IsolationCapabilitySIDPrefix
	isolationCapabilitySIDComponentCount = windowsbroker.IsolationCapabilitySIDComponentCount
)

type appContainerIdentity struct {
	traditionalUserSID     *windows.SID
	packageSID             *windows.SID
	isolationCapabilitySID *windows.SID
}

type appContainerProcessClaim = windowsbroker.ProcessClaim

func windowsBrokerIdentity(identity appContainerIdentity) windowsbroker.Identity {
	return windowsbroker.Identity{
		TraditionalUserSID:     identity.traditionalUserSID,
		PackageSID:             identity.packageSID,
		IsolationCapabilitySID: identity.isolationCapabilitySID,
	}
}

func localAppContainerIdentity(identity windowsbroker.Identity) appContainerIdentity {
	return appContainerIdentity{
		traditionalUserSID:     identity.TraditionalUserSID,
		packageSID:             identity.PackageSID,
		isolationCapabilitySID: identity.IsolationCapabilitySID,
	}
}

func newIsolationCapabilitySID() (*windows.SID, error) {
	return windowsbroker.NewIsolationCapabilitySID()
}

func deriveIsolationCapabilitySID(name string) (*windows.SID, error) {
	return windowsbroker.DeriveIsolationCapabilitySID(name)
}

func verifyIsolationCapabilitySID(capability *windows.SID) error {
	return windowsbroker.VerifyIsolationCapabilitySID(capability)
}

func capabilitySIDsForToken(token windows.Token) ([]*windows.SID, error) {
	return windowsbroker.CapabilitySIDsForToken(token)
}

func appContainerIdentityForToken(token windows.Token) (appContainerIdentity, error) {
	identity, err := windowsbroker.IdentityForToken(token)
	return localAppContainerIdentity(identity), err
}

func tokenUserSID(token windows.Token) (*windows.SID, error) {
	return windowsbroker.TokenUserSID(token)
}

func currentAppContainerIdentity() (appContainerIdentity, error) {
	identity, err := windowsbroker.CurrentIdentity()
	return localAppContainerIdentity(identity), err
}

func verifyPrivateAppContainerProcess(process windows.Handle, identity appContainerIdentity) error {
	return windowsbroker.VerifyPrivateProcess(process, windowsBrokerIdentity(identity))
}

func verifyPrivateAppContainerToken(token windows.Token, identity appContainerIdentity) error {
	return windowsbroker.VerifyPrivateToken(token, windowsBrokerIdentity(identity))
}

func verifyLowIntegrityToken(token windows.Token) error {
	return windowsbroker.VerifyLowIntegrityToken(token)
}

func sidLastSubAuthority(sid *windows.SID) (uint32, error) {
	return windowsbroker.SIDLastSubAuthority(sid)
}

func verifyTokenHasNoEnabledPrivileges(token windows.Token) error {
	return windowsbroker.VerifyTokenHasNoEnabledPrivileges(token)
}

func appContainerSIDForToken(token windows.Token) (*windows.SID, error) {
	return windowsbroker.AppContainerSIDForToken(token)
}

func tokenUint32(token windows.Token, informationClass uint32) (uint32, error) {
	return windowsbroker.TokenUint32(token, informationClass)
}

func tokenGroupCount(token windows.Token, informationClass uint32) (uint32, error) {
	return windowsbroker.TokenGroupCount(token, informationClass)
}

func tokenInformationBuffer(token windows.Token, informationClass uint32) ([]byte, error) {
	return windowsbroker.TokenInformationBuffer(token, informationClass)
}

func tokenSecurityAttributeNames(token windows.Token) (map[string]bool, error) {
	return windowsbroker.TokenSecurityAttributeNames(token)
}

func appContainerProcessClaimForToken(token windows.Token) (appContainerProcessClaim, error) {
	return windowsbroker.ProcessClaimForToken(token)
}

func appContainerFolderPath(packageSID *windows.SID) (string, error) {
	return windowsbroker.AppContainerFolderPath(packageSID)
}

func randomBytes(count int) ([]byte, error) {
	return windowsbroker.RandomBytes(count)
}
