//go:build linux || windows

package processrun

import "strings"

func ownerHelperEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	prefix := helperRoleEnvironment + "="
	for _, entry := range environment {
		if !strings.EqualFold(strings.SplitN(entry, "=", 2)[0], helperRoleEnvironment) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+helperRoleValue)
}
