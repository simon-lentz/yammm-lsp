package lsp

// braceScanner tracks brace nesting depth across lines, correctly skipping
// braces inside line comments (//), block comments (/* */), and string literals.
//
// This is the single implementation of the comment/string/brace scanning state
// machine used by computeBraceDepths, isInsideTypeBodyDirect, and depthAtColumn.
type braceScanner struct {
	depth          int
	inBlockComment bool
}

// scanLine processes a single line up to maxCol bytes and updates the scanner's
// depth and inBlockComment state. Returns the depth after scanning.
func (bs *braceScanner) scanLine(line string, maxCol int) int {
	if maxCol > len(line) {
		maxCol = len(line)
	}

	j := 0
	for j < maxCol {
		// Handle block comment continuation
		if bs.inBlockComment {
			if j+1 < len(line) && line[j] == '*' && line[j+1] == '/' {
				j += 2
				bs.inBlockComment = false
				continue
			}
			j++
			continue
		}

		ch := line[j]

		// Skip line comments (rest of line)
		if j+1 < len(line) && line[j] == '/' && line[j+1] == '/' {
			break
		}

		// Start block comment
		if j+1 < len(line) && line[j] == '/' && line[j+1] == '*' {
			bs.inBlockComment = true
			j += 2
			continue
		}

		// Skip string literals
		if ch == '"' || ch == '\'' {
			quote := ch
			j++
			for j < maxCol {
				if line[j] == '\\' && j+1 < len(line) {
					j += 2 // skip escape sequence
					continue
				}
				if line[j] == quote {
					j++
					break
				}
				j++
			}
			continue
		}

		// Count braces
		switch ch {
		case '{':
			bs.depth++
		case '}':
			bs.depth--
		}
		j++
	}

	return bs.depth
}
