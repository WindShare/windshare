package humanoutput

// Symbols keeps Unicode capability independent from interactivity and ANSI.
// ASCII output remains meaningful instead of relying on font replacement glyphs.
type Symbols struct {
	Ready, Arrow, Sharing, Relay, Waiting, Path, Discovery string
	Success, Warning, Failure, Paused                      string
	Separator, Ellipsis                                    string
}

func SelectSymbols(unicode bool) Symbols {
	if !unicode {
		return Symbols{
			Ready: "*", Arrow: ">", Sharing: ">", Relay: ">", Waiting: ">", Path: ">", Discovery: "?",
			Success: "OK", Warning: "!", Failure: "X", Paused: "||",
			Separator: " | ", Ellipsis: "...",
		}
	}
	return Symbols{
		Ready: "✨", Arrow: "➜", Sharing: "➜", Relay: "➜", Waiting: "👥", Path: "⚡", Discovery: "🔍",
		Success: "✔", Warning: "⚠", Failure: "✖", Paused: "⏸",
		Separator: " · ", Ellipsis: "…",
	}
}
