// Command transportboundary keeps frame transports independent of connection orchestration.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
)

const (
	modulePath       = "github.com/windshare/windshare"
	transportPath    = modulePath + "/transport"
	connectivityPath = modulePath + "/connectivity"
)

type listedPackage struct {
	ImportPath string
	Deps       []string
}

func findings(reader io.Reader) ([]string, error) {
	var result []string
	decoder := json.NewDecoder(reader)
	for {
		var pkg listedPackage
		if err := decoder.Decode(&pkg); err != nil {
			if err == io.EOF {
				slices.Sort(result)
				return result, nil
			}
			return nil, err
		}
		if !inFamily(pkg.ImportPath, transportPath) {
			continue
		}
		// Deps includes indirect imports, so an intermediate wrapper cannot move
		// socket admission or path selection back into a frame transport.
		for _, dependency := range pkg.Deps {
			if inFamily(dependency, connectivityPath) {
				result = append(result, fmt.Sprintf("%s depends on %s", pkg.ImportPath, dependency))
			}
		}
	}
}

func inFamily(path, family string) bool {
	return path == family || strings.HasPrefix(path, family+"/")
}

func targetEnvironment(base []string, goos string) []string {
	result := make([]string, 0, len(base)+4)
	for _, variable := range base {
		key, _, _ := strings.Cut(variable, "=")
		switch strings.ToUpper(key) {
		case "GOOS", "GOARCH", "GOWORK", "GOTOOLCHAIN":
			continue
		}
		result = append(result, variable)
	}
	return append(result, "GOOS="+goos, "GOARCH=amd64", "GOWORK=off", "GOTOOLCHAIN=local")
}

func run() error {
	executable := os.Getenv("WINDSHARE_GO_EXECUTABLE")
	if executable == "" {
		executable = "go"
	}
	for _, goos := range []string{"linux", "windows", "darwin"} {
		command := exec.Command(executable, "list", "-mod=readonly", "-json", "./transport/...")
		command.Env = targetEnvironment(os.Environ(), goos)
		var output, diagnostics bytes.Buffer
		command.Stdout, command.Stderr = &output, &diagnostics
		if err := command.Run(); err != nil {
			return fmt.Errorf("%s package graph: %w: %s", goos, err, &diagnostics)
		}
		violations, err := findings(&output)
		if err != nil {
			return fmt.Errorf("%s package graph: %w", goos, err)
		}
		if len(violations) != 0 {
			return fmt.Errorf("%s: %s", goos, strings.Join(violations, "; "))
		}
		fmt.Printf("transport-boundary: target=%s/amd64 PASS\n", goos)
	}
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "transport-boundary: %v\n", err)
		os.Exit(1)
	}
}
