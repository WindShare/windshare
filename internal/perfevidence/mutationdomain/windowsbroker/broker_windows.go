//go:build windows

package windowsbroker

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	mutationwire "github.com/windshare/windshare/internal/perfevidence/mutationdomain/wire"
	"golang.org/x/sys/windows"
)

const (
	helperArgument       = "--perfevidence-mutation-helper"
	maximumProtocolLine  = mutationwire.MaximumProtocolLine
	privateRootDirectory = "private-mutation-domain"
)

type initialization = mutationwire.Initialization
type response = mutationwire.Response

type ObjectCreator func(root windows.Handle, name string, directory bool) (windows.Handle, error)

// StageInputs preserves source traversal as a caller-owned policy while the
// broker retains exclusive ownership of Windows object creation authorities.
type StageInputs func(
	privateRootPath string,
	privateRoot windows.Handle,
	roots []mutationwire.RootSpec,
	creator ObjectCreator,
) (string, error)

const (
	BrokerProcessDescriptor = brokerProcessDescriptor
	BrokerThreadDescriptor  = brokerThreadDescriptor
)

type ProcessSpec struct {
	Executable        string
	Arguments         []string
	Environment       []string
	Directory         string
	ProcessDescriptor string
	ThreadDescriptor  string
	PackageSID        *windows.SID
	CapabilitySID     *windows.SID
	AppContainer      bool
	Token             windows.Token
	Inherited         []windows.Handle
	OwnJob            bool
	Suspended         bool
}

type StartedProcess = windowsStartedProcess

func StartProcess(spec ProcessSpec) (*StartedProcess, error) {
	return createSealedWindowsProcess(windowsProcessSpec{
		executable: spec.Executable, arguments: spec.Arguments, environment: spec.Environment,
		directory: spec.Directory, processDescriptor: spec.ProcessDescriptor,
		threadDescriptor: spec.ThreadDescriptor, packageSID: spec.PackageSID,
		capabilitySID: spec.CapabilitySID, appContainer: spec.AppContainer, token: spec.Token,
		inherited: spec.Inherited, ownJob: spec.OwnJob, suspended: spec.Suspended,
	})
}

func (process *windowsStartedProcess) Stdin() *os.File {
	return process.stdin
}

func (process *windowsStartedProcess) Stdout() *os.File {
	return process.stdout
}

func (process *windowsStartedProcess) Stderr() *mutationwire.BoundedCapture {
	return process.stderr
}

func (process *windowsStartedProcess) Handle() windows.Handle {
	return process.control.handleValue()
}

func (process *windowsStartedProcess) ProcessID() uint32 {
	return process.control.pid
}

func (process *windowsStartedProcess) ExitCode() (uint32, error) {
	return process.control.exitCode()
}

func (process *windowsStartedProcess) Resume() error {
	return process.resume()
}

func (process *windowsStartedProcess) Kill() error {
	return process.kill()
}

func (process *windowsStartedProcess) Wait() error {
	return process.wait()
}

func (process *windowsStartedProcess) ClosePipes() error {
	return process.closePipes()
}

func NewKillOnCloseJob() (windows.Handle, error) {
	return newKillOnCloseJob()
}

func VerifyLauncherReopenDenied(processID uint32) error {
	return verifyWindowsLauncherReopenDenied(processID)
}

func SettleJob(job windows.Handle) error {
	return settleWindowsJob(job)
}

const (
	ProfileNamePrefix                    = appContainerProfilePrefix
	ProfileEntropyBytes                  = appContainerProfileEntropyBytes
	ProfileEntropyHexBytes               = appContainerProfileEntropyHexBytes
	IsolationCapabilityNamePrefix        = isolationCapabilityNamePrefix
	IsolationCapabilitySIDPrefix         = isolationCapabilitySIDPrefix
	IsolationCapabilitySIDComponentCount = isolationCapabilitySIDComponentCount
)

type Identity struct {
	TraditionalUserSID     *windows.SID
	PackageSID             *windows.SID
	IsolationCapabilitySID *windows.SID
}

type ProcessClaim = appContainerProcessClaim

func privateIdentity(identity Identity) appContainerIdentity {
	return appContainerIdentity{
		traditionalUserSID:     identity.TraditionalUserSID,
		packageSID:             identity.PackageSID,
		isolationCapabilitySID: identity.IsolationCapabilitySID,
	}
}

func publicIdentity(identity appContainerIdentity) Identity {
	return Identity{
		TraditionalUserSID:     identity.traditionalUserSID,
		PackageSID:             identity.packageSID,
		IsolationCapabilitySID: identity.isolationCapabilitySID,
	}
}

func NewIsolationCapabilitySID() (*windows.SID, error) {
	return newIsolationCapabilitySID()
}

func DeriveIsolationCapabilitySID(name string) (*windows.SID, error) {
	return deriveIsolationCapabilitySID(name)
}

func VerifyIsolationCapabilitySID(capability *windows.SID) error {
	return verifyIsolationCapabilitySID(capability)
}

func CapabilitySIDsForToken(token windows.Token) ([]*windows.SID, error) {
	return capabilitySIDsForToken(token)
}

func IdentityForToken(token windows.Token) (Identity, error) {
	identity, err := appContainerIdentityForToken(token)
	return publicIdentity(identity), err
}

func CurrentIdentity() (Identity, error) {
	identity, err := currentAppContainerIdentity()
	return publicIdentity(identity), err
}

func TokenUserSID(token windows.Token) (*windows.SID, error) {
	return tokenUserSID(token)
}

func VerifyPrivateProcess(process windows.Handle, identity Identity) error {
	return verifyPrivateAppContainerProcess(process, privateIdentity(identity))
}

func VerifyPrivateToken(token windows.Token, identity Identity) error {
	return verifyPrivateAppContainerToken(token, privateIdentity(identity))
}

func VerifyLowIntegrityToken(token windows.Token) error {
	return verifyLowIntegrityToken(token)
}

func SIDLastSubAuthority(sid *windows.SID) (uint32, error) {
	return sidLastSubAuthority(sid)
}

func VerifyTokenHasNoEnabledPrivileges(token windows.Token) error {
	return verifyTokenHasNoEnabledPrivileges(token)
}

func AppContainerSIDForToken(token windows.Token) (*windows.SID, error) {
	return appContainerSIDForToken(token)
}

func TokenUint32(token windows.Token, informationClass uint32) (uint32, error) {
	return tokenUint32(token, informationClass)
}

func TokenGroupCount(token windows.Token, informationClass uint32) (uint32, error) {
	return tokenGroupCount(token, informationClass)
}

func TokenInformationBuffer(token windows.Token, informationClass uint32) ([]byte, error) {
	return tokenInformationBuffer(token, informationClass)
}

func TokenSecurityAttributeNames(token windows.Token) (map[string]bool, error) {
	return tokenSecurityAttributeNames(token)
}

func ProcessClaimForToken(token windows.Token) (ProcessClaim, error) {
	return appContainerProcessClaimForToken(token)
}

func CreateRecoveryMarker(runtimeRoot, profileName string) (string, error) {
	return createAppContainerRecoveryMarker(runtimeRoot, profileName)
}

func CreateProfile(profileName string) (*windows.SID, error) {
	return createEphemeralAppContainerProfile(profileName)
}

func ReleaseProfileSID(packageSID *windows.SID) error {
	return releaseNativeAppContainerSID(packageSID)
}

func DeleteProfile(profileName string) error {
	return deleteEphemeralAppContainerProfile(profileName)
}

func ValidProfileName(profileName string) bool {
	return validEphemeralAppContainerProfileName(profileName)
}

func AppContainerFolderPath(packageSID *windows.SID) (string, error) {
	return appContainerFolderPath(packageSID)
}

func readJSONLine(reader *bufio.Reader, destination any) error {
	return mutationwire.ReadJSONLine(reader, destination)
}

func writeJSONLine(writer io.Writer, value any) error {
	return mutationwire.WriteJSONLine(writer, value)
}

func Run(parentInput io.Reader, parentOutput io.Writer, retainedImage *os.File, stageInputs StageInputs) (resultErr error) {
	reader := bufio.NewReaderSize(parentInput, maximumProtocolLine)
	var configuration initialization
	if err := readJSONLine(reader, &configuration); err != nil {
		return fmt.Errorf("read broker initialization: %w", err)
	}
	authority, err := createPrivateAppContainer(configuration, retainedImage, stageInputs)
	if err != nil {
		return errors.Join(writeJSONLine(parentOutput, response{Error: err.Error(), ExitCode: -1}), err)
	}
	defer func() { resultErr = errors.Join(resultErr, authority.close()) }()
	configuration.PrivateRoot = authority.rootPath
	configuration.BootstrapManifest = authority.manifestPath
	started, err := createSealedWindowsProcess(windowsProcessSpec{
		executable:        authority.helperPath,
		arguments:         []string{helperArgument},
		directory:         os.Getenv("SystemRoot"),
		processDescriptor: appContainerProcessDescriptor(authority.traditionalSID, authority.capabilitySID),
		threadDescriptor:  appContainerThreadDescriptor(authority.traditionalSID, authority.capabilitySID),
		packageSID:        authority.packageSID,
		capabilitySID:     authority.capabilitySID,
		appContainer:      true,
		ownJob:            true,
		suspended:         true,
	})
	if err != nil {
		startErr := fmt.Errorf("start no-network AppContainer helper: %w", err)
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: startErr.Error(), ExitCode: -1}),
			startErr,
		)
	}
	if err := verifyWindowsProcessImage(
		started.control.handleValue(), authority.helper, authority.helperPath, false,
	); err != nil {
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: err.Error(), ExitCode: -1}),
			err, started.kill(), started.closePipes(), started.wait(),
		)
	}
	if err := verifyPrivateAppContainerProcess(started.control.handleValue(), appContainerIdentity{
		traditionalUserSID:     authority.traditionalSID,
		packageSID:             authority.packageSID,
		isolationCapabilitySID: authority.capabilitySID,
	}); err != nil {
		attestationErr := fmt.Errorf("attest suspended AppContainer helper: %w", err)
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: attestationErr.Error(), ExitCode: -1}),
			attestationErr, started.kill(), started.closePipes(), started.wait(),
		)
	}
	if err := started.resume(); err != nil {
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: err.Error(), ExitCode: -1}),
			err, started.kill(), started.closePipes(), started.wait(),
		)
	}
	if err := writeJSONLine(started.stdin, configuration); err != nil {
		return errors.Join(err, started.kill(), started.closePipes(), started.wait())
	}
	helperReader := bufio.NewReaderSize(started.stdout, maximumProtocolLine)
	var ready response
	if err := readJSONLine(helperReader, &ready); err != nil {
		exitCode, exitCodeErr := started.control.exitCode()
		settleErr := errors.Join(started.kill(), started.closePipes(), started.wait())
		stderr := strings.TrimSpace(string(started.stderr.Snapshot()))
		startErr := errors.Join(
			fmt.Errorf("initialize no-network AppContainer helper: %w", err),
			fmt.Errorf("AppContainer helper exit code at protocol EOF: %d: %w", exitCode, exitCodeErr),
		)
		if stderr != "" {
			startErr = errors.Join(startErr, fmt.Errorf("AppContainer helper stderr: %s", stderr))
		}
		startErr = errors.Join(startErr, settleErr)
		return errors.Join(
			writeJSONLine(parentOutput, response{Error: startErr.Error(), ExitCode: -1}),
			startErr,
		)
	}
	if ready.Error != "" {
		readyErr := errors.New(ready.Error)
		return errors.Join(
			writeJSONLine(parentOutput, ready), readyErr,
			started.kill(), started.closePipes(), started.wait(),
		)
	}
	if err := writeJSONLine(parentOutput, ready); err != nil {
		return errors.Join(err, started.kill(), started.closePipes(), started.wait())
	}
	inputDone := make(chan error, 1)
	outputDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(started.stdin, reader)
		inputDone <- errors.Join(copyErr, started.stdin.Close())
	}()
	go func() {
		_, copyErr := io.Copy(parentOutput, helperReader)
		outputDone <- errors.Join(copyErr, started.stdout.Close())
	}()
	var proxyErr error
	select {
	case inputErr := <-inputDone:
		proxyErr = inputErr
		if inputErr != nil {
			proxyErr = errors.Join(proxyErr, started.kill())
		}
		proxyErr = errors.Join(proxyErr, <-outputDone)
	case outputErr := <-outputDone:
		proxyErr = outputErr
		proxyErr = errors.Join(proxyErr, started.stdin.Close())
	}
	return errors.Join(proxyErr, started.kill(), started.wait())
}
