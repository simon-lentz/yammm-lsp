package docstate

import "strings"

// NormalizeLineEndings converts CRLF and CR line endings to LF.
// This ensures consistent line ending handling across platforms.
// Windows clients may send CRLF (\r\n), which would cause incorrect
// byte offset calculations in position conversion.
func NormalizeLineEndings(text string) string {
	// First replace CRLF with LF, then replace any remaining CR with LF
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text
}
