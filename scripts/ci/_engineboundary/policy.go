package main

import "strings"

const module = "github.com/windshare/windshare"

func sourceImportViolation(source, dependency string) string {
	engine := strings.HasPrefix(source, "engine/")
	if strings.HasPrefix(dependency, module+"/internal/test") && (engine || productionHost(source)) {
		return "production code cannot import test-support packages"
	}

	switch {
	case strings.HasPrefix(source, "engine/observation/"):
		// Public facts cannot inherit the engine's permission to assemble networking.
		if implementationPackage(dependency) {
			return "observation contracts cannot depend on networking or storage implementations"
		}
	case engine:
		if storageImplementation(dependency) {
			return "shared engine code must receive filesystem adapters from its host"
		}
	default:
		if networkingImplementation(dependency) {
			return "commands and hosts must access networking through engine"
		}
	}
	return ""
}

func productionHost(path string) bool {
	return strings.HasPrefix(path, "host/") || strings.HasPrefix(path, "cmd/wind/") ||
		strings.HasPrefix(path, "cmd/windshare-host/")
}

func implementationPackage(path string) bool {
	return networkingImplementation(path) || storageImplementation(path)
}

func networkingImplementation(path string) bool {
	for _, family := range []string{"connectivity", "transport", "relay"} {
		if inRepositoryFamily(path, family) {
			return true
		}
	}
	return false
}

func storageImplementation(path string) bool {
	for _, family := range []string{"osfs", "core/osfs", "adapters"} {
		if inRepositoryFamily(path, family) {
			return true
		}
	}
	return false
}

func prohibitedAPI(path string) bool {
	if !strings.HasPrefix(path, module+"/") {
		return false
	}
	for _, contract := range []string{"core", "engine"} {
		if inRepositoryFamily(path, contract) {
			return false
		}
	}
	return true
}

func inRepositoryFamily(path, family string) bool {
	root := module + "/" + family
	return path == root || strings.HasPrefix(path, root+"/")
}
