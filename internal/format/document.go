package format

import "strings"

func finalizeFormattedText(text string) string {
	lines := strings.Split(text, "\n")
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		result = append(result, strings.TrimRight(line, " \t"))
	}

	for len(result) > 0 && result[len(result)-1] == "" {
		result = result[:len(result)-1]
	}

	formatted := strings.Join(result, "\n")
	if formatted != "" && !strings.HasSuffix(formatted, "\n") {
		formatted += "\n"
	}
	return formatted
}

// FormatDocument applies canonical formatting rules to a YAMMM document.
//
// Implementation Note: This formatter uses line-by-line string processing rather
// than AST walking. This approach correctly normalizes whitespace and preserves
// comments positionally, but cannot safely reorder declarations while maintaining
// comment associations. This is acceptable since the current formatting rules do
// not require semantic reordering. If declaration reordering is needed in the
// future, an AST-based formatter should be implemented.
//
// Rules:
// - Tabs for indentation (spaces converted to tabs: 4 spaces = 1 tab)
// - LF line endings
// - No trailing whitespace
// - Preserve blank lines (conservative: maintains visual structure between declarations)
// - Preserve comment text and line positions (indentation is normalized)
func FormatDocument(text string) string {
	// Normalize line endings to LF
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	lines := strings.Split(text, "\n")
	result := make([]string, 0, len(lines))

	for _, line := range lines {
		// Remove trailing whitespace but preserve leading whitespace (indentation)
		trimmedRight := strings.TrimRight(line, " \t")

		// Normalize indentation: convert spaces to tabs (canonical format)
		normalized := NormalizeIndentation(trimmedRight)

		// Check if line is blank (only whitespace)
		isBlank := strings.TrimSpace(line) == ""

		if isBlank {
			// Preserve all blank lines - maintains visual structure between declarations
			result = append(result, "")
		} else {
			result = append(result, normalized)
		}
	}

	// Remove trailing blank lines
	for len(result) > 0 && result[len(result)-1] == "" {
		result = result[:len(result)-1]
	}

	// Ensure file ends with newline
	formatted := strings.Join(result, "\n")
	if formatted != "" && !strings.HasSuffix(formatted, "\n") {
		formatted += "\n"
	}

	return formatted
}

// NormalizeIndentation converts spaces to tabs for indentation.
// Each 4 spaces at the start of a line becomes 1 tab.
func NormalizeIndentation(line string) string {
	if line == "" {
		return line
	}

	// Count leading whitespace
	leadingWS := 0
	for _, r := range line {
		if r == ' ' || r == '\t' {
			leadingWS++
		} else {
			break
		}
	}

	if leadingWS == 0 {
		return line
	}

	// Extract leading whitespace and content
	leading := line[:leadingWS]
	content := line[leadingWS:]

	// Convert to tabs: count equivalent spaces (tab = 4 spaces)
	spaceCount := 0
	for _, r := range leading {
		if r == '\t' {
			spaceCount += 4
		} else {
			spaceCount++
		}
	}

	// Convert to tabs
	tabs := spaceCount / 4
	remaining := spaceCount % 4

	return strings.Repeat("\t", tabs) + strings.Repeat(" ", remaining) + content
}
