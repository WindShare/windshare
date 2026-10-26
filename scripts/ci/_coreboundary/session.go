package main

import "fmt"

// Runtime roles may share wire ownership, but must not inherit each other's
// application services. Test graphs intentionally compose both ends.
func sessionDependencyViolations(scope graphScope, importer string, imports []string) []string {
	if scope != productionScope {
		return nil
	}
	const session = coreImportPath + "/session"
	sender, receiver := session+"/sendersession", session+"/receiversession"
	kernel := session + "/internal/sessioncore"
	var forbidden []string
	switch {
	case inPathFamily(importer, kernel):
		// Signing keys belong to the role owner; the kernel accepts preparation capability.
		forbidden = []string{"crypto/ed25519", sender, receiver, session + "/receivercontinuation", coreImportPath + "/transfer", coreImportPath + "/liveshare"}
	case inPathFamily(importer, sender):
		forbidden = []string{receiver, session + "/receivercontinuation", coreImportPath + "/liveshare"}
	case inPathFamily(importer, receiver):
		forbidden = []string{sender, coreImportPath + "/liveshare"}
	case inPathFamily(importer, session+"/observation") || importer == session:
		forbidden = []string{sender, receiver, kernel, session + "/receivercontinuation", coreImportPath + "/transfer", coreImportPath + "/liveshare"}
	}
	var violations []string
	for _, dependency := range imports {
		for _, boundary := range forbidden {
			if inPathFamily(dependency, boundary) {
				violations = append(violations, fmt.Sprintf("session ownership boundary imports %s", dependency))
			}
		}
	}
	return violations
}
