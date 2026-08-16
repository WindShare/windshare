package clievent

// DisplayName and DisplayPath are intentionally human-only values. Their
// accessors exist for the human renderer; the trace visitor must not read them.
// Terminal escaping remains the renderer/canvas boundary's responsibility.
type DisplayName struct{ text string }
type DisplayPath struct{ text string }

func NewDisplayName(text string) DisplayName { return DisplayName{text: text} }
func NewDisplayPath(text string) DisplayPath { return DisplayPath{text: text} }

func (value DisplayName) Text() string { return value.text }
func (value DisplayPath) Text() string { return value.text }
func (value DisplayName) Empty() bool  { return value.text == "" }
func (value DisplayPath) Empty() bool  { return value.text == "" }
