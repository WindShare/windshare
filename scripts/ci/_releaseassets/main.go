// releaseassets packages binaries built from an already verified source bundle.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	source := flag.String("source", "", "extracted verified source root")
	archive := flag.String("source-zip", "", "verified source bundle")
	out := flag.String("out", "", "new assets directory")
	version := flag.String("version", "", "release version")
	commit := flag.String("commit", "", "release commit")
	flag.Parse()
	if err := buildAssets(*source, *archive, *out, *version, *commit); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func buildAssets(source, archive, out, version, commit string) error {
	if source == "" || archive == "" || out == "" || !safeIdentity(version) || !safeIdentity(commit) {
		return fmt.Errorf("source, source-zip, out, version and commit are required")
	}
	if err := os.Mkdir(out, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "windshare-binaries-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	extension := ""
	if runtime.GOOS == "windows" {
		extension = ".exe"
	}
	for _, item := range []struct{ name, path string }{{"wind", "./cmd/wind"}, {"wsrelay", "./relay/cmd/wsrelay"}} {
		command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-o", filepath.Join(staging, item.name+extension), item.path)
		command.Dir = source
		command.Env = buildEnvironment()
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %s: %w", item.name, output, err)
		}
	}
	for _, path := range []string{"LICENSE", "NOTICE", "docs/installation.md", "docs/stun.md", "scripts/deploy/wsrelay/README.md", "third_party/pion/README.md", "third_party/pion/manifest.json", "third_party/pion/ice/LICENSE", "third_party/pion/webrtc/LICENSE"} {
		if err := copyFile(filepath.Join(source, filepath.FromSlash(path)), filepath.Join(staging, filepath.FromSlash(path))); err != nil {
			return err
		}
	}
	installFiles := []string{"scripts/install/install.sh"}
	if runtime.GOOS == "windows" {
		installFiles = []string{"scripts/install/windows/install.ps1", "scripts/install/windows/firewall.psm1"}
	}
	for _, path := range installFiles {
		if err := copyFile(filepath.Join(source, filepath.FromSlash(path)), filepath.Join(staging, filepath.FromSlash(path))); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(staging, "VERSION"), []byte(version+"\n"+commit+"\n"), 0o644); err != nil {
		return err
	}
	binaryName := fmt.Sprintf("windshare-%s-%s-%s.zip", version, runtime.GOOS, runtime.GOARCH)
	if err := archiveDirectory(filepath.Join(out, binaryName), staging, "windshare-"+version+"/", extension); err != nil {
		return err
	}
	sourceName := "windshare-" + version + "-source.zip"
	if err := copyFile(archive, filepath.Join(out, sourceName)); err != nil {
		return err
	}
	var sums strings.Builder
	for _, name := range []string{binaryName, sourceName} {
		data, err := os.ReadFile(filepath.Join(out, name))
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(digest[:]), name)
	}
	return os.WriteFile(filepath.Join(out, "SHA256SUMS-"+runtime.GOOS+"-"+runtime.GOARCH), []byte(sums.String()), 0o644)
}

func safeIdentity(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '.' || character == '-') {
			return false
		}
	}
	return true
}

func buildEnvironment() []string {
	var environment []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		switch strings.ToUpper(key) {
		case "GOWORK", "CGO_ENABLED", "GOOS", "GOARCH", "GOFLAGS":
			continue
		}
		environment = append(environment, item)
	}
	return append(environment, "GOWORK=off", "CGO_ENABLED=0", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH, "GOFLAGS=")
}
