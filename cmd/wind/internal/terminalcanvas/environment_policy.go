package terminalcanvas

import "strings"

type environmentLookup func(string) (string, bool)

func nativeColorEnabled(ansi bool, lookup environmentLookup) bool {
	if !ansi {
		return false
	}
	_, disabled := lookup("NO_COLOR")
	return !disabled
}

func localeSupportsUnicode(lookup environmentLookup) bool {
	for _, name := range [...]string{"LC_ALL", "LC_CTYPE", "LANG"} {
		value, present := lookup(name)
		if !present || value == "" {
			continue
		}
		normalized := strings.ToLower(value)
		return strings.Contains(normalized, "utf-8") || strings.Contains(normalized, "utf8")
	}
	return false
}
