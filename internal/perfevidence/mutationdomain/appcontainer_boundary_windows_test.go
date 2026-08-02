//go:build windows

package mutationdomain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/windshare/windshare/internal/perfevidence"
	"golang.org/x/sys/windows"
)

const (
	nativeBoundaryProbeEnvironment       = "WINDSHARE_NATIVE_BOUNDARY_PROBE"
	nativeBoundaryChildEnvironment       = "WINDSHARE_NATIVE_BOUNDARY_CHILD"
	nativeBoundaryOutputEnvironment      = "WINDSHARE_NATIVE_BOUNDARY_OUTPUT"
	nativeBoundarySourceEnvironment      = "WINDSHARE_NATIVE_BOUNDARY_SOURCE"
	nativeBoundaryHostSecretEnvironment  = "WINDSHARE_NATIVE_BOUNDARY_HOST_SECRET"
	nativeBoundaryNetworkEnvironment     = "WINDSHARE_NATIVE_BOUNDARY_NETWORK"
	foreignProfileProbeEnvironment       = "WINDSHARE_FOREIGN_PROFILE_PROBE"
	foreignProfilePathEnvironment        = "WINDSHARE_FOREIGN_PROFILE_PATH"
	foreignProfileOutputEnvironment      = "WINDSHARE_FOREIGN_PROFILE_OUTPUT"
	profileIdentityProbeEnvironment      = "WINDSHARE_PROFILE_IDENTITY_PROBE"
	profileIdentityOutputEnvironment     = "WINDSHARE_PROFILE_IDENTITY_OUTPUT"
	nativeBoundaryExpectedSourceContents = "sealed-source-snapshot"
)

type nativeBoundaryProbeResult struct {
	PackageSID                  string
	CapabilitySID               string
	TraditionalUserSID          string
	OwnerSID                    string
	PrivateRoot                 string
	IsAppContainer              uint32
	CapabilityCount             uint32
	RestrictedSIDCount          uint32
	IntegrityRID                uint32
	Claims                      []string
	Claim                       appContainerProcessClaim
	DescendantClaim             appContainerProcessClaim
	DescendantReadAllowed       bool
	HostPathDenied              bool
	NetworkDenied               bool
	NoDangerousEnabledPrivilege bool
}

type nativeBoundaryChildResult struct {
	Claim   appContainerProcessClaim
	Content string
}

type foreignProfileProbeResult struct {
	PackageSID    string
	CapabilitySID string
	Denied        bool
}

type profileIdentityProbeResult struct {
	PackageSID    string
	CapabilitySID string
}

func TestPrivateDomainEnforcesNativeAppContainerBoundary(t *testing.T) {
	inputRoot, target, source := createWindowsBoundaryInputs(t)
	hostSecret := filepath.Join(t.TempDir(), "host-secret.txt")
	if err := os.WriteFile(hostSecret, []byte("must-not-cross-boundary"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	runtimeRoot := createWindowsRuntimeRoot(t)
	domain, err := NewFactory().Open(context.Background(), perfevidence.MutationDomainSpec{
		RuntimeRoot: runtimeRoot,
		Roots:       []perfevidence.MutationRoot{{Name: "test", HostPath: inputRoot}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := domain.Close(); err != nil {
			t.Error(err)
		}
	}()
	profileName := activeWindowsProfileName(t, runtimeRoot)
	hostOutput := filepath.Join(t.TempDir(), "boundary.json")
	sink := &memorySink{}
	result, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target,
		Arguments:  []string{"-test.run=^TestMutationDomainNativeBoundaryProbe$"},
		Directory:  inputRoot,
		Environment: mutationDomainTestEnvironment(
			nativeBoundaryProbeEnvironment+"=1",
			nativeBoundaryOutputEnvironment+"="+hostOutput,
			nativeBoundarySourceEnvironment+"="+source,
			nativeBoundaryHostSecretEnvironment+"="+hostSecret,
			nativeBoundaryNetworkEnvironment+"="+listener.Addr().String(),
		),
		Outputs: []perfevidence.MutationOutput{{HostPath: hostOutput, MaxBytes: 1 << 20}},
	}, map[string]perfevidence.MutationOutputSink{hostOutput: sink})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("native boundary probe = exit %d stderr=%q err=%v", result.ExitCode, result.Stderr, err)
	}
	var observed nativeBoundaryProbeResult
	if err := json.Unmarshal(sink.content, &observed); err != nil {
		t.Fatal(err)
	}
	assertNativeBoundaryIdentity(t, observed)
	t.Logf("native boundary identity: user=%s package=%s capability=%s owner=%s claims=%v", observed.TraditionalUserSID, observed.PackageSID, observed.CapabilitySID, observed.OwnerSID, observed.Claims)
	if !observed.DescendantReadAllowed || observed.Claim == observed.DescendantClaim {
		t.Fatalf("same-package descendant evidence = allowed=%v parent=%#v child=%#v", observed.DescendantReadAllowed, observed.Claim, observed.DescendantClaim)
	}
	if !observed.HostPathDenied || !observed.NetworkDenied {
		t.Fatalf("private-capability boundary = hostDenied=%v networkDenied=%v", observed.HostPathDenied, observed.NetworkDenied)
	}
	if err := openPrivateRootAsTrustedUser(observed.PrivateRoot); err != nil {
		t.Fatalf("trusted invoking user could not retain private AppContainer root: %v", err)
	}

	foreign := runForeignProfileProbe(t, inputRoot, target, observed.PrivateRoot)
	if foreign.PackageSID == observed.PackageSID {
		t.Fatalf("foreign profile unexpectedly reused package SID %s", observed.PackageSID)
	}
	if foreign.CapabilitySID == observed.CapabilitySID {
		t.Fatalf("foreign profile unexpectedly reused isolation capability SID %s", observed.CapabilitySID)
	}
	if !foreign.Denied {
		t.Fatal("another AppContainer profile opened the private root")
	}

	profileRoot := activeWindowsProfileRoot(t, observed.PackageSID)
	if err := domain.Close(); err != nil {
		t.Fatal(err)
	}
	assertWindowsProfileDeleted(t, runtimeRoot, profileName, observed.PackageSID, profileRoot)
}

func TestPrivateDomainUsesFreshProfilesAndKeepsCopiedTokenSIDsGoOwned(t *testing.T) {
	inputRoot, target, _ := createWindowsBoundaryInputs(t)
	firstName, firstIdentity := runWindowsProfileIdentitySession(t, inputRoot, target)
	secondName, secondIdentity := runWindowsProfileIdentitySession(t, inputRoot, target)
	if firstName == secondName || firstIdentity.PackageSID == secondIdentity.PackageSID ||
		firstIdentity.CapabilitySID == secondIdentity.CapabilitySID {
		t.Fatalf("ephemeral AppContainer identity was reused: first=(%s,%#v) second=(%s,%#v)", firstName, firstIdentity, secondName, secondIdentity)
	}
}

func TestMutationDomainNativeBoundaryProbe(t *testing.T) {
	if os.Getenv(nativeBoundaryProbeEnvironment) != "1" {
		t.Skip("target-only native AppContainer boundary probe")
	}
	if os.Getenv(nativeBoundaryChildEnvironment) == "1" {
		claim, err := currentNativeProcessClaim()
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(os.Getenv(nativeBoundarySourceEnvironment))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(nativeBoundaryChildResult{Claim: claim, Content: string(content)}); err != nil {
			t.Fatal(err)
		}
		return
	}
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	identity, err := appContainerIdentityForToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyPrivateAppContainerToken(token, identity); err != nil {
		t.Fatal(err)
	}
	ownerBuffer, err := tokenInformationBuffer(token, windows.TokenOwner)
	if err != nil {
		t.Fatal(err)
	}
	ownerSID := *(**windows.SID)(unsafe.Pointer(&ownerBuffer[0]))
	if ownerSID == nil {
		t.Fatal("AppContainer token owner SID is unavailable")
	}
	claim, err := appContainerProcessClaimForToken(token)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestMutationDomainNativeBoundaryProbe$")
	child.Env = append(os.Environ(), nativeBoundaryChildEnvironment+"=1")
	childOutput, err := child.Output()
	if err != nil {
		t.Fatal(err)
	}
	var descendant nativeBoundaryChildResult
	if err := json.NewDecoder(bytes.NewReader(childOutput)).Decode(&descendant); err != nil {
		t.Fatal(err)
	}
	claims, err := tokenSecurityAttributeNames(token)
	if err != nil {
		t.Fatal(err)
	}
	claimNames := make([]string, 0, len(claims))
	for name := range claims {
		claimNames = append(claimNames, name)
	}
	sort.Strings(claimNames)
	isAppContainer, err := tokenUint32(token, 29)
	if err != nil {
		t.Fatal(err)
	}
	capabilities, err := tokenGroupCount(token, 30)
	if err != nil {
		t.Fatal(err)
	}
	restricted, err := tokenGroupCount(token, windows.TokenRestrictedSids)
	if err != nil {
		t.Fatal(err)
	}
	integrityRID, err := tokenIntegrityRIDForTest(token)
	if err != nil {
		t.Fatal(err)
	}
	hostDenied := false
	if _, err := os.ReadFile(os.Getenv(nativeBoundaryHostSecretEnvironment)); err != nil {
		hostDenied = true
	}
	connection, networkErr := net.DialTimeout("tcp4", os.Getenv(nativeBoundaryNetworkEnvironment), 500*time.Millisecond)
	if connection != nil {
		_ = connection.Close()
	}
	output := os.Getenv(nativeBoundaryOutputEnvironment)
	observed := nativeBoundaryProbeResult{
		PackageSID:                  identity.packageSID.String(),
		CapabilitySID:               identity.isolationCapabilitySID.String(),
		TraditionalUserSID:          identity.traditionalUserSID.String(),
		OwnerSID:                    ownerSID.String(),
		PrivateRoot:                 filepath.Dir(filepath.Dir(output)),
		IsAppContainer:              isAppContainer,
		CapabilityCount:             capabilities,
		RestrictedSIDCount:          restricted,
		IntegrityRID:                integrityRID,
		Claims:                      claimNames,
		Claim:                       claim,
		DescendantClaim:             descendant.Claim,
		DescendantReadAllowed:       descendant.Content == nativeBoundaryExpectedSourceContents,
		HostPathDenied:              hostDenied,
		NetworkDenied:               networkErr != nil,
		NoDangerousEnabledPrivilege: verifyTokenHasNoEnabledPrivileges(token) == nil,
	}
	encoded, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMutationDomainForeignProfileProbe(t *testing.T) {
	if os.Getenv(foreignProfileProbeEnvironment) != "1" {
		t.Skip("target-only foreign AppContainer profile probe")
	}
	identity, err := currentAppContainerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	_, accessErr := os.ReadDir(os.Getenv(foreignProfilePathEnvironment))
	encoded, err := json.Marshal(foreignProfileProbeResult{
		PackageSID:    identity.packageSID.String(),
		CapabilitySID: identity.isolationCapabilitySID.String(),
		Denied:        accessErr != nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(foreignProfileOutputEnvironment), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMutationDomainProfileIdentityProbe(t *testing.T) {
	if os.Getenv(profileIdentityProbeEnvironment) != "1" {
		t.Skip("target-only AppContainer profile identity probe")
	}
	identity, err := currentAppContainerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(profileIdentityProbeResult{
		PackageSID:    identity.packageSID.String(),
		CapabilitySID: identity.isolationCapabilitySID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(profileIdentityOutputEnvironment), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func createWindowsBoundaryInputs(t *testing.T) (string, string, string) {
	t.Helper()
	inputRoot := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(inputRoot, "boundary-probe.exe")
	if err := copyRegularTestFile(executable, target); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(inputRoot, "source.txt")
	if err := os.WriteFile(source, []byte(nativeBoundaryExpectedSourceContents), 0o600); err != nil {
		t.Fatal(err)
	}
	return inputRoot, target, source
}

func createWindowsRuntimeRoot(t *testing.T) string {
	t.Helper()
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return runtimeRoot
}

func activeWindowsProfileName(t *testing.T, runtimeRoot string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(runtimeRoot, appContainerLedgerDirectory))
	if err != nil {
		t.Fatal(err)
	}
	var profileNames []string
	for _, entry := range entries {
		if profileName, valid := profileNameFromMarker(entry.Name()); valid {
			profileNames = append(profileNames, profileName)
		}
	}
	if len(profileNames) != 1 {
		t.Fatalf("active AppContainer recovery markers = %v, want exactly one", profileNames)
	}
	return profileNames[0]
}

func assertNativeBoundaryIdentity(t *testing.T, observed nativeBoundaryProbeResult) {
	t.Helper()
	const securityMandatoryLowRID = 0x00001000
	if observed.PackageSID == "" || observed.CapabilitySID == "" || observed.IsAppContainer != 1 || observed.CapabilityCount != 1 ||
		observed.RestrictedSIDCount != 0 || observed.IntegrityRID != securityMandatoryLowRID ||
		!observed.NoDangerousEnabledPrivilege {
		t.Fatalf("native AppContainer identity = %#v", observed)
	}
	capabilitySID, err := windows.StringToSid(observed.CapabilitySID)
	if err != nil {
		t.Fatalf("parse private capability SID: %v", err)
	}
	if err := verifyIsolationCapabilitySID(capabilitySID); err != nil {
		t.Fatal(err)
	}
	trustedUserSID, err := tokenUserSID(windows.GetCurrentProcessToken())
	if err != nil {
		t.Fatal(err)
	}
	if observed.TraditionalUserSID != trustedUserSID.String() || observed.OwnerSID != trustedUserSID.String() {
		t.Fatalf("traditional AppContainer authority = user %s owner %s, want trusted user %s", observed.TraditionalUserSID, observed.OwnerSID, trustedUserSID.String())
	}
	if !containsString(observed.Claims, "TSA://ProcUnique") || containsString(observed.Claims, "WIN://SYSAPPID") {
		t.Fatalf("native unpackaged claims = %v", observed.Claims)
	}
}

func runForeignProfileProbe(t *testing.T, inputRoot, target, privateRoot string) foreignProfileProbeResult {
	t.Helper()
	runtimeRoot := createWindowsRuntimeRoot(t)
	domain, err := NewFactory().Open(context.Background(), perfevidence.MutationDomainSpec{
		RuntimeRoot: runtimeRoot,
		Roots:       []perfevidence.MutationRoot{{Name: "test", HostPath: inputRoot}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := domain.Close(); err != nil {
			t.Error(err)
		}
	}()
	hostOutput := filepath.Join(t.TempDir(), "foreign.json")
	sink := &memorySink{}
	result, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target,
		Arguments:  []string{"-test.run=^TestMutationDomainForeignProfileProbe$"},
		Directory:  inputRoot,
		Environment: mutationDomainTestEnvironment(
			foreignProfileProbeEnvironment+"=1",
			foreignProfilePathEnvironment+"="+privateRoot,
			foreignProfileOutputEnvironment+"="+hostOutput,
		),
		Outputs: []perfevidence.MutationOutput{{HostPath: hostOutput, MaxBytes: 1 << 20}},
	}, map[string]perfevidence.MutationOutputSink{hostOutput: sink})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("foreign profile probe = exit %d stderr=%q err=%v", result.ExitCode, result.Stderr, err)
	}
	var observed foreignProfileProbeResult
	if err := json.Unmarshal(sink.content, &observed); err != nil {
		t.Fatal(err)
	}
	return observed
}

func runWindowsProfileIdentitySession(t *testing.T, inputRoot, target string) (string, profileIdentityProbeResult) {
	t.Helper()
	runtimeRoot := createWindowsRuntimeRoot(t)
	domain, err := NewFactory().Open(context.Background(), perfevidence.MutationDomainSpec{
		RuntimeRoot: runtimeRoot,
		Roots:       []perfevidence.MutationRoot{{Name: "test", HostPath: inputRoot}},
	})
	if err != nil {
		t.Fatal(err)
	}
	profileName := activeWindowsProfileName(t, runtimeRoot)
	hostOutput := filepath.Join(t.TempDir(), "identity.txt")
	sink := &memorySink{}
	result, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target,
		Arguments:  []string{"-test.run=^TestMutationDomainProfileIdentityProbe$"},
		Directory:  inputRoot,
		Environment: mutationDomainTestEnvironment(
			profileIdentityProbeEnvironment+"=1",
			profileIdentityOutputEnvironment+"="+hostOutput,
		),
		Outputs: []perfevidence.MutationOutput{{HostPath: hostOutput, MaxBytes: 1 << 20}},
	}, map[string]perfevidence.MutationOutputSink{hostOutput: sink})
	if err != nil || result.ExitCode != 0 {
		_ = domain.Close()
		t.Fatalf("profile identity probe = exit %d stderr=%q err=%v", result.ExitCode, result.Stderr, err)
	}
	var identity profileIdentityProbeResult
	if err := json.Unmarshal(sink.content, &identity); err != nil {
		_ = domain.Close()
		t.Fatal(err)
	}
	profileRoot := activeWindowsProfileRoot(t, identity.PackageSID)
	if err := domain.Close(); err != nil {
		t.Fatal(err)
	}
	assertWindowsProfileDeleted(t, runtimeRoot, profileName, identity.PackageSID, profileRoot)
	return profileName, identity
}

func activeWindowsProfileRoot(t *testing.T, observedSID string) string {
	t.Helper()
	packageSID, err := windows.StringToSid(observedSID)
	if err != nil {
		t.Fatal(err)
	}
	profileRoot, err := appContainerFolderPath(packageSID)
	if err != nil {
		t.Fatal(err)
	}
	return profileRoot
}

func assertWindowsProfileDeleted(t *testing.T, runtimeRoot, profileName, observedSID, profileRoot string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(runtimeRoot, appContainerLedgerDirectory, profileName+appContainerMarkerSuffix)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("profile recovery marker survived successful shutdown: %v", err)
	}
	if _, err := os.Stat(profileRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ephemeral AppContainer storage survived profile deletion: %s: %v", profileRoot, err)
	}
	recreatedSID, err := createEphemeralAppContainerProfile(profileName)
	if err != nil {
		t.Fatalf("profile name was not released by teardown: %v", err)
	}
	if recreatedSID.String() != observedSID {
		t.Fatalf("recreated profile SID = %s, observed %s", recreatedSID.String(), observedSID)
	}
	if err := releaseNativeAppContainerSID(recreatedSID); err != nil {
		t.Error(err)
	}
	if err := deleteEphemeralAppContainerProfile(profileName); err != nil {
		t.Error(err)
	}
}

func tokenIntegrityRIDForTest(token windows.Token) (uint32, error) {
	buffer, err := tokenInformationBuffer(token, windows.TokenIntegrityLevel)
	if err != nil {
		return 0, err
	}
	label := (*windows.SIDAndAttributes)(unsafe.Pointer(&buffer[0]))
	return sidLastSubAuthority(label.Sid)
}

func openPrivateRootAsTrustedUser(path string) error {
	encoded, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		encoded,
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return err
	}
	return windows.CloseHandle(handle)
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if strings.EqualFold(value, expected) {
			return true
		}
	}
	return false
}
