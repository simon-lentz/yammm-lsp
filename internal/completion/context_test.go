package completion

import (
	"strings"
	"testing"

	"github.com/simon-lentz/yammm-lsp/internal/docstate"
)

// textToDoc creates a minimal docstate.Snapshot for testing DetectContext.
// LineState is nil so tests exercise the fallback path (isInsideTypeBodyDirect).
func textToDoc(text string) *docstate.Snapshot {
	return &docstate.Snapshot{Text: text}
}

func TestDetectContext_TopLevel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		line     int
		char     int
		expected Context
	}{
		{
			name:     "empty file",
			text:     "",
			line:     0,
			char:     0,
			expected: TopLevel,
		},
		{
			name:     "after schema declaration",
			text:     "schema \"test\"\n\n",
			line:     2,
			char:     0,
			expected: TopLevel,
		},
		{
			name:     "beginning of line after import",
			text:     "schema \"test\"\nimport \"./foo\" as foo\n\n",
			line:     3,
			char:     0,
			expected: TopLevel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := DetectContext(textToDoc(tt.text), tt.line, tt.char)
			if result != tt.expected {
				t.Errorf("DetectContext() = %v; want %v", result, tt.expected)
			}
		})
	}
}

func TestDetectContext_TypeBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		line     int
		char     int
		expected Context
	}{
		{
			name:     "inside type braces",
			text:     "type Person {\n    \n}",
			line:     1,
			char:     4,
			expected: TypeBody,
		},
		{
			name:     "after opening brace",
			text:     "type Person {\n",
			line:     1,
			char:     0,
			expected: TypeBody,
		},
		{
			name:     "nested in type with properties",
			text:     "type Person {\n    name String\n    \n}",
			line:     2,
			char:     4,
			expected: TypeBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := DetectContext(textToDoc(tt.text), tt.line, tt.char)
			if result != tt.expected {
				t.Errorf("DetectContext() = %v; want %v", result, tt.expected)
			}
		})
	}
}

func TestDetectContext_Extends(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		line     int
		char     int
		expected Context
	}{
		{
			name:     "after extends keyword",
			text:     "type Car extends ",
			line:     0,
			char:     17,
			expected: Extends,
		},
		{
			name:     "after extends with partial",
			text:     "type Car extends Ve",
			line:     0,
			char:     19,
			expected: Extends,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := DetectContext(textToDoc(tt.text), tt.line, tt.char)
			if result != tt.expected {
				t.Errorf("DetectContext() = %v; want %v", result, tt.expected)
			}
		})
	}
}

func TestDetectContext_PropertyType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		line     int
		char     int
		expected Context
	}{
		{
			name:     "after property name with space",
			text:     "type Person {\n    name ",
			line:     1,
			char:     9,
			expected: PropertyType,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := DetectContext(textToDoc(tt.text), tt.line, tt.char)
			if result != tt.expected {
				t.Errorf("DetectContext() = %v; want %v", result, tt.expected)
			}
		})
	}
}

func TestDetectContext_RelationTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		text     string
		line     int
		char     int
		expected Context
	}{
		{
			name:     "after association arrow and multiplicity",
			text:     "type Person {\n    --> ADDRESSES (many) ",
			line:     1,
			char:     28,
			expected: RelationTarget,
		},
		{
			name:     "after composition arrow and multiplicity",
			text:     "type Car {\n    *-> WHEELS (many) ",
			line:     1,
			char:     22,
			expected: RelationTarget,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := DetectContext(textToDoc(tt.text), tt.line, tt.char)
			if result != tt.expected {
				t.Errorf("DetectContext() = %v; want %v", result, tt.expected)
			}
		})
	}
}

func TestDetectContext_Import(t *testing.T) {
	t.Parallel()

	text := "import "
	result := DetectContext(textToDoc(text), 0, 7)
	if result != ImportPath {
		t.Errorf("DetectContext() = %v; want %v", result, ImportPath)
	}
}

func TestIsIdentifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input    string
		expected bool
	}{
		{"name", true},
		{"_private", true},
		{"Name123", true},
		{"123name", false},
		{"", false},
		{"name-with-dash", false},
		{"name.dot", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			t.Parallel()
			result := isIdentifier(tt.input)
			if result != tt.expected {
				t.Errorf("isIdentifier(%q) = %v; want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsInsideTypeBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		text      string
		line      int
		cursorCol int // byte offset within line (-1 means end of line)
		expected  bool
	}{
		{
			name:      "outside type",
			text:      "schema \"test\"\n\ntype Person {\n}\n",
			line:      1,
			cursorCol: -1,
			expected:  false,
		},
		{
			name:      "inside type",
			text:      "type Person {\n    name String\n}",
			line:      1,
			cursorCol: -1,
			expected:  true,
		},
		{
			name:      "after closing brace",
			text:      "type Person {\n    name String\n}\n",
			line:      3,
			cursorCol: -1,
			expected:  false,
		},
		{
			name:      "nested braces",
			text:      "type A {\n}\ntype B {\n    x Int\n}",
			line:      3,
			cursorCol: -1,
			expected:  true,
		},
		{
			name:      "brace in comment should not count",
			text:      "type Person {\n    // } this is a comment\n    name String\n}",
			line:      2,
			cursorCol: -1,
			expected:  true,
		},
		{
			name:      "cursor before closing brace on same line",
			text:      "type Person { }",
			line:      0,
			cursorCol: 14, // cursor before the closing }
			expected:  true,
		},
		{
			name:      "cursor after closing brace on same line",
			text:      "type Person { }",
			line:      0,
			cursorCol: 15, // cursor after the closing }
			expected:  false,
		},
		{
			name:      "cursor before first brace",
			text:      "type Person { }",
			line:      0,
			cursorCol: 12, // cursor before opening {
			expected:  false,
		},
		{
			name:      "cursor just after opening brace",
			text:      "type Person { }",
			line:      0,
			cursorCol: 14, // cursor after opening { but before closing }
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lines := strings.Split(tt.text, "\n")
			cursorCol := tt.cursorCol
			if cursorCol < 0 {
				// Use end of line
				cursorCol = len(lines[tt.line])
			}
			// Test the direct brace-counting logic (without cache)
			result := isInsideTypeBodyDirect(lines, tt.line, cursorCol)
			if result != tt.expected {
				t.Errorf("isInsideTypeBodyDirect(line=%d, col=%d) = %v; want %v\ntext: %q",
					tt.line, cursorCol, result, tt.expected, tt.text)
			}
		})
	}
}

func TestIsInsideTypeBodyDirect_CommentSequencesInStrings(t *testing.T) {
	// Tests that // and /* */ inside string literals don't affect brace counting.
	// The string parser should skip over these sequences.
	//
	// This is a regression test for the bug where comment detection ran
	// BEFORE string handling, causing strings like "http://" to be treated
	// as containing a line comment.
	t.Parallel()

	tests := []struct {
		name      string
		text      string
		line      int
		cursorCol int
		expected  bool
	}{
		{
			name:      "URL in string - not a comment",
			text:      "type Foo {\n    url \"http://example.com\"\n}",
			line:      1,
			cursorCol: 30, // Inside braces, after the string
			expected:  true,
		},
		{
			name:      "block comment sequence in string",
			text:      "type Foo {\n    note \"/* not a comment */\"\n}",
			line:      1,
			cursorCol: 35, // Inside braces
			expected:  true,
		},
		{
			name:      "line comment sequence in string",
			text:      "type Foo {\n    path \"C://Windows//path\"\n}",
			line:      1,
			cursorCol: 30,
			expected:  true,
		},
		{
			name:      "brace inside string should not count",
			text:      "type Foo {\n    json \"{nested}\"\n}",
			line:      1,
			cursorCol: 20,
			expected:  true, // Still depth 1, not 2
		},
		{
			name:      "closing brace in string should not close type",
			text:      "type Foo {\n    val \"}\"\n    \n}",
			line:      2,
			cursorCol: 4, // Empty line after string with }
			expected:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lines := strings.Split(tt.text, "\n")
			result := isInsideTypeBodyDirect(lines, tt.line, tt.cursorCol)
			if result != tt.expected {
				t.Errorf("isInsideTypeBodyDirect(line=%d, col=%d) = %v; want %v\ntext: %q",
					tt.line, tt.cursorCol, result, tt.expected, tt.text)
			}
		})
	}
}

func TestIsInsideTypeBody_CachedLineState(t *testing.T) {
	// Tests IsInsideTypeBody with a pre-computed lineState cache.
	// This exercises the O(1) cached path rather than the direct computation.
	t.Parallel()

	text := "type A {\n    name String\n}"
	lines := strings.Split(text, "\n")

	// Pre-compute lineState using the same logic as Workspace
	braceDepths, inComment := docstate.ComputeBraceDepths(text)

	doc := &docstate.Snapshot{
		Version: 1,
		Text:    text,
		LineState: &docstate.LineState{
			Version:        1, // Matches doc version
			BraceDepth:     braceDepths,
			InBlockComment: inComment,
		},
	}

	tests := []struct {
		line      int
		cursorCol int // -1 means end of line
		expected  bool
	}{
		{0, -1, true},  // "type A {" - end of line after opening brace, depth=1
		{1, -1, true},  // "    name String" - inside body
		{2, -1, false}, // "}" - end of line after closing brace, depth=0
		{2, 0, true},   // "}" - cursor before closing brace (start of line 2)
	}

	for _, tt := range tests {
		cursorCol := tt.cursorCol
		if cursorCol < 0 {
			cursorCol = len(lines[tt.line])
		}
		result := IsInsideTypeBody(doc, lines, tt.line, cursorCol)
		if result != tt.expected {
			t.Errorf("IsInsideTypeBody(line %d, col %d) = %v; want %v (braceDepths=%v)",
				tt.line, cursorCol, result, tt.expected, braceDepths)
		}
	}
}

func TestIsInsideTypeBody_StaleCacheUsesDirectComputation(t *testing.T) {
	// Tests that IsInsideTypeBody falls back to direct computation when
	// lineState version doesn't match document version.
	t.Parallel()

	text := "type A {\n    name String\n}"
	lines := strings.Split(text, "\n")

	doc := &docstate.Snapshot{
		Version: 2, // document version
		Text:    text,
		LineState: &docstate.LineState{
			Version:    1, // Stale cache version
			BraceDepth: []int{1, 1, 0},
		},
	}

	// Should fall back to direct computation, which gives same result
	// Use end of line position
	cursorCol := len(lines[1])
	result := IsInsideTypeBody(doc, lines, 1, cursorCol)
	if !result {
		t.Error("IsInsideTypeBody with stale cache should fall back to direct computation")
	}
}

func TestIsInsideTypeBody_CachedLineState_MultiLineBlockComment(t *testing.T) {
	// Tests IsInsideTypeBody with cached lineState when a multi-line block comment
	// contains braces. This exercises the O(1) cached path with InBlockComment state.
	// Guards against false positives from braces inside block comments.
	t.Parallel()

	// Multi-line block comment with braces inside
	text := "type A {\n/* {\n} */\n    name String\n}"
	lines := strings.Split(text, "\n")

	// Pre-compute lineState
	braceDepths, inComment := docstate.ComputeBraceDepths(text)

	// Verify the computed state is correct
	// Line 0: "type A {" -> depth 1, not in block comment
	// Line 1: "/* {" -> depth 1 (brace in comment), ends IN block comment
	// Line 2: "} */" -> depth 1 (} is in comment before */), ends NOT in block comment
	// Line 3: "    name String" -> depth 1
	// Line 4: "}" -> depth 0
	expectedDepths := []int{1, 1, 1, 1, 0}
	expectedInComment := []bool{false, true, false, false, false}

	for i, d := range braceDepths {
		if d != expectedDepths[i] {
			t.Fatalf("pre-check: braceDepths[%d] = %d; want %d", i, d, expectedDepths[i])
		}
	}
	for i, c := range inComment {
		if c != expectedInComment[i] {
			t.Fatalf("pre-check: inComment[%d] = %v; want %v", i, c, expectedInComment[i])
		}
	}

	doc := &docstate.Snapshot{
		Version: 1,
		Text:    text,
		LineState: &docstate.LineState{
			Version:        1,
			BraceDepth:     braceDepths,
			InBlockComment: inComment,
		},
	}

	tests := []struct {
		name      string
		line      int
		cursorCol int
		expected  bool
	}{
		{"line 0 end (after opening brace)", 0, len(lines[0]), true},
		{"line 1 start (inside block comment)", 1, 0, true}, // In comment, but still inside braces
		{"line 1 end (brace in comment)", 1, len(lines[1]), true},
		{"line 2 start (} in comment before */)", 2, 0, true},
		{"line 2 end (after comment closes)", 2, len(lines[2]), true},
		{"line 3 (inside type body)", 3, len(lines[3]), true},
		{"line 4 end (after closing brace)", 4, len(lines[4]), false},
		{"line 4 start (before closing brace)", 4, 0, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := IsInsideTypeBody(doc, lines, tt.line, tt.cursorCol)
			if result != tt.expected {
				t.Errorf("IsInsideTypeBody(line %d, col %d) = %v; want %v\nbraceDepths=%v inComment=%v",
					tt.line, tt.cursorCol, result, tt.expected, braceDepths, inComment)
			}
		})
	}
}

func TestIsImportContext_CursorAfterPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		beforeCursor string
		want         bool
	}{
		{
			name:         "cursor after closing quote",
			beforeCursor: `import "some/path"`,
			want:         false,
		},
		{
			name:         "cursor after path with space",
			beforeCursor: `import "some/path" `,
			want:         false,
		},
		{
			name:         "cursor inside path (one quote)",
			beforeCursor: `import "some/pa`,
			want:         true,
		},
		{
			name:         "cursor at opening quote",
			beforeCursor: `import "`,
			want:         true,
		},
		{
			name:         "cursor before any quote",
			beforeCursor: `import `,
			want:         true,
		},
		// Single-quote support
		{
			name:         "single-quoted path complete",
			beforeCursor: `import 'some/path'`,
			want:         false,
		},
		{
			name:         "single-quoted path complete with space",
			beforeCursor: `import 'some/path' `,
			want:         false,
		},
		{
			name:         "single-quoted path incomplete",
			beforeCursor: `import 'some/pa`,
			want:         true,
		},
		{
			name:         "single-quoted path at opening",
			beforeCursor: `import '`,
			want:         true,
		},
		// Escaped quotes
		{
			name:         "escaped double quote inside path",
			beforeCursor: `import "path\"with`,
			want:         true,
		},
		{
			name:         "escaped double quote complete",
			beforeCursor: `import "path\"with"`,
			want:         false,
		},
		{
			name:         "escaped single quote inside path",
			beforeCursor: `import 'it\'s`,
			want:         true,
		},
		{
			name:         "escaped single quote complete",
			beforeCursor: `import 'it\'s'`,
			want:         false,
		},
		// Paths starting with "as" - should NOT be detected as the `as` keyword
		{
			name:         "double-quoted path starting with as",
			beforeCursor: `import "as`,
			want:         true,
		},
		{
			name:         "double-quoted path assets incomplete",
			beforeCursor: `import "assets/foo`,
			want:         true,
		},
		{
			name:         "single-quoted path starting with as",
			beforeCursor: `import 'as`,
			want:         true,
		},
		{
			name:         "single-quoted path assets incomplete",
			beforeCursor: `import 'assets/foo`,
			want:         true,
		},
		{
			name:         "double-quoted path starting with as complete",
			beforeCursor: `import "assets"`,
			want:         false,
		},
		{
			name:         "double-quoted path starting with as then alias",
			beforeCursor: `import "assets" as `,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isImportContext(tt.beforeCursor)
			if got != tt.want {
				t.Errorf("isImportContext(%q) = %v; want %v", tt.beforeCursor, got, tt.want)
			}
		})
	}
}

func TestIsQuotedStringComplete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"empty string", "", false},
		{"no quotes", "hello", false},
		{"double quote open only", `"hello`, false},
		{"double quote complete", `"hello"`, true},
		{"single quote open only", `'hello`, false},
		{"single quote complete", `'hello'`, true},
		{"escaped double quote incomplete", `"hel\"lo`, false},
		{"escaped double quote complete", `"hel\"lo"`, true},
		{"escaped single quote incomplete", `'it\'s`, false},
		{"escaped single quote complete", `'it\'s'`, true},
		{"mismatched quotes", `"hello'`, false},
		{"mismatched quotes reverse", `'hello"`, false},
		{"trailing backslash", `"hello\`, false},
		{"multiple escapes complete", `"a\"b\"c"`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isQuotedStringComplete(tt.input)
			if got != tt.want {
				t.Errorf("isQuotedStringComplete(%q) = %v; want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsImportContext_CursorInAlias(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		beforeCursor string
		want         bool
	}{
		{
			name:         "cursor after 'as' keyword",
			beforeCursor: `import "some/path" as `,
			want:         false,
		},
		{
			name:         "cursor typing alias",
			beforeCursor: `import "some/path" as ali`,
			want:         false,
		},
		{
			name:         "cursor at 'as' keyword",
			beforeCursor: `import "some/path" as`,
			want:         false,
		},
		{
			name:         "cursor with space before as",
			beforeCursor: `import "some/path"  as foo`,
			want:         false,
		},
		{
			name:         "cursor with tab before as",
			beforeCursor: "import \"some/path\"\tas foo",
			want:         false,
		},
		{
			name:         "cursor with tab after as",
			beforeCursor: "import \"some/path\" as\tfoo",
			want:         false,
		},
		{
			name:         "cursor with tabs around as",
			beforeCursor: "import \"some/path\"\tas\tfoo",
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := isImportContext(tt.beforeCursor)
			if got != tt.want {
				t.Errorf("isImportContext(%q) = %v; want %v", tt.beforeCursor, got, tt.want)
			}
		})
	}
}

func TestDetectContext_UTF8_MultiByteChars(t *testing.T) {
	// Validates that DetectContext does not panic when character
	// positions are byte offsets (UTF-8 encoding) and content contains
	// multi-byte UTF-8 characters.
	t.Parallel()

	// Content with CJK characters:
	// Line 0: "type 日本語 {"
	//         t(0) y(1) p(2) e(3) _(4) 日(5,6,7) 本(8,9,10) 語(11,12,13) _(14) {(15)
	// Line 1: "    name String"
	//         _(0) _(1) _(2) _(3) n(4) a(5) m(6) e(7) _(8) S(9)...
	// Line 2: "}"
	text := "type 日本語 {\n    name String\n}"

	tests := []struct {
		name     string
		line     int
		charByte int // byte offset from line start (UTF-8 encoding semantics)
	}{
		// Line 0: "type 日本語 {"
		{"start of file", 0, 0},
		{"after 'type' keyword", 0, 4},
		{"at first byte of 日", 0, 5},
		{"middle byte of 日 (byte 2)", 0, 6},
		{"last byte of 日 (byte 3)", 0, 7},
		{"at first byte of 本", 0, 8},
		{"middle byte of 本", 0, 9},
		{"at first byte of 語", 0, 11},
		{"middle byte of 語", 0, 12},
		{"after CJK, at space", 0, 14},
		{"at opening brace", 0, 15},

		// Line 1: "    name String" (inside type body)
		{"start of line inside body", 1, 0},
		{"after indentation", 1, 4},
		{"after 'name '", 1, 9},

		// Line 2: "}"
		{"at closing brace", 2, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Primary assertion: does not panic when charByte points to middle of multi-byte char
			result := DetectContext(textToDoc(text), tt.line, tt.charByte)
			// Secondary: returns a valid context
			if result < Unknown || result > ImportPath {
				t.Errorf("DetectContext returned invalid context: %d", result)
			}
		})
	}
}

func TestDetectContext_UTF8_Emoji(t *testing.T) {
	// Test with emoji (4-byte UTF-8 sequences) to ensure no panics
	// when byte offsets land in the middle of multi-byte sequences.
	t.Parallel()

	// "type 😀Test {" where 😀 is 4 bytes (U+1F600)
	// t(0) y(1) p(2) e(3) _(4) 😀(5,6,7,8) T(9) e(10) s(11) t(12) _(13) {(14)
	text := "type 😀Test {\n}"

	tests := []struct {
		name     string
		charByte int
	}{
		{"before emoji", 4},
		{"first byte of emoji", 5},
		{"second byte of emoji", 6},
		{"third byte of emoji", 7},
		{"fourth byte of emoji", 8},
		{"after emoji", 9},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			// Primary assertion: does not panic
			result := DetectContext(textToDoc(text), 0, tt.charByte)
			// Secondary: returns a valid context (not strictly required, but good sanity check)
			if result < Unknown || result > ImportPath {
				t.Errorf("DetectContext returned invalid context: %d", result)
			}
		})
	}
}
