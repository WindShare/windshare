package platformsetup

import (
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

//go:embed windows/inspect.ps1
var inspectScript string

//go:embed windows/rules.ps1
var rulesScript string

//go:embed windows/elevate.ps1
var elevateScript string

func quotePowerShell(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func encodePowerShell(script string) string {
	units := utf16.Encode([]rune(script))
	buffer := make([]byte, 2*len(units))
	for index, unit := range units {
		binary.LittleEndian.PutUint16(buffer[index*2:], unit)
	}
	return base64.StdEncoding.EncodeToString(buffer)
}

func ruleScript(executable string, action RuleAction) string {
	return "$Executable = " + quotePowerShell(executable) + "\n$Remove = " +
		map[bool]string{true: "$true", false: "$false"}[action == RemoveRules] + "\n" + rulesScript
}
