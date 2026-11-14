package configjson

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

const (
	maximumNameRunes = 64
	maximumPathBytes = 256
)

type repeatedMemberError struct {
	name, first string
	object      []string // innermost first, populated while the scan unwinds
}

func (failure *repeatedMemberError) Error() string {
	message := fmt.Sprintf("member %s appears more than once in %s", quoteName(failure.name), failure.objectPath())
	if failure.first != failure.name {
		message += fmt.Sprintf(" (also written as %s; member names match case-insensitively)", quoteName(failure.first))
	}
	return message
}

func (failure *repeatedMemberError) Unwrap() error { return ErrRepeatedMember }

func (failure *repeatedMemberError) objectPath() string {
	if len(failure.object) == 0 {
		return "the top-level object"
	}
	var path strings.Builder
	for _, segment := range slices.Backward(failure.object) {
		if path.Len()+len(segment) > maximumPathBytes {
			path.WriteString("...")
			break
		}
		if path.Len() > 0 && !strings.HasPrefix(segment, "[") {
			path.WriteByte('.')
		}
		path.WriteString(segment)
	}
	return path.String()
}

// Quote arbitrary names instead of deriving a second schema just to format
// diagnostics. Limits apply to display only; comparison always uses full names.
func quoteName(name string) string {
	count := 0
	for index := range name {
		if count == maximumNameRunes {
			name = name[:index] + "..."
			break
		}
		count++
	}
	return strconv.QuoteToASCII(name)
}

func memberPath(folded string) string {
	name := strings.ToLower(folded)
	for _, r := range name {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "[" + quoteName(name) + "]"
		}
	}
	if name == "" || len(name) > maximumNameRunes {
		return "[" + quoteName(name) + "]"
	}
	return name
}
