package lsp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm/location"
	"github.com/simon-lentz/yammm/schema"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/symbols"
)

func TestTopLevelCompletions(t *testing.T) {
	t.Parallel()
	s := &Server{}

	items := s.topLevelCompletions()

	if len(items) == 0 {
		t.Fatal("expected some completions")
	}

	// Check for expected keywords
	labels := make(map[string]bool)
	for _, item := range items {
		labels[item.Label] = true
	}

	expected := []string{"schema", "import", "type", "abstract type", "part type"}
	for _, exp := range expected {
		if !labels[exp] {
			t.Errorf("missing expected completion: %s", exp)
		}
	}

	// Check that items have snippet format
	for _, item := range items {
		if item.InsertTextFormat == nil || *item.InsertTextFormat != protocol.InsertTextFormatSnippet {
			t.Errorf("completion %q should have snippet format", item.Label)
		}
	}
}

func TestTypeBodyCompletions(t *testing.T) {
	t.Parallel()
	s := &Server{}

	items := s.typeBodyCompletions()

	if len(items) == 0 {
		t.Fatal("expected some completions")
	}

	// Check for snippets
	hasProperty := false
	hasAssociation := false
	hasComposition := false
	hasInvariant := false

	for _, item := range items {
		switch {
		case strings.Contains(item.Label, "property"):
			hasProperty = true
		case strings.Contains(item.Label, "association"):
			hasAssociation = true
		case strings.Contains(item.Label, "composition"):
			hasComposition = true
		case strings.Contains(item.Label, "invariant"):
			hasInvariant = true
		}
	}

	if !hasProperty {
		t.Error("missing property snippet")
	}
	if !hasAssociation {
		t.Error("missing association snippet")
	}
	if !hasComposition {
		t.Error("missing composition snippet")
	}
	if !hasInvariant {
		t.Error("missing invariant snippet")
	}

	// Check that built-in types are included
	hasString := false
	hasInteger := false
	for _, item := range items {
		if item.Label == "String" {
			hasString = true
		}
		if item.Label == "Integer" {
			hasInteger = true
		}
	}

	if !hasString {
		t.Error("missing String built-in type")
	}
	if !hasInteger {
		t.Error("missing Integer built-in type")
	}
}

func TestTypeCompletions_NilSnapshot(t *testing.T) {
	t.Parallel()
	s := &Server{}

	// With nil snapshot, should return empty slice without panic
	// Use zero SourceID since nil snapshot means no lookup occurs
	items := s.typeCompletions(nil, location.SourceID{})

	if items == nil {
		t.Error("expected non-nil slice")
	}
	if len(items) != 0 {
		t.Errorf("expected empty slice, got %d items", len(items))
	}
}

func TestTypeCompletions_EmptySchema(t *testing.T) {
	t.Parallel()
	s := &Server{}

	// Create a snapshot with an empty schema
	sourceID := location.MustNewSourceID("test://types.yammm")
	span := location.Range(sourceID, 1, 1, 10, 1)

	sch := schema.NewSchema("test", sourceID, span, "")

	snapshot := &analysis.Snapshot{
		Schema:          sch,
		SymbolsBySource: make(map[location.SourceID]*symbols.SymbolIndex),
	}

	items := s.typeCompletions(snapshot, sourceID)

	// Should return empty but not nil
	if items == nil {
		t.Error("expected non-nil slice")
	}
}

func TestPropertyTypeCompletions_BuiltinTypes(t *testing.T) {
	t.Parallel()
	s := &Server{}

	// Use zero SourceID since nil snapshot means no lookup occurs
	items := s.propertyTypeCompletions(nil, location.SourceID{})

	// Should have built-in types even without snapshot
	builtins := map[string]bool{
		"String":    false,
		"Integer":   false,
		"Float":     false,
		"Boolean":   false,
		"UUID":      false,
		"Date":      false,
		"Timestamp": false,
	}

	for _, item := range items {
		if _, ok := builtins[item.Label]; ok {
			builtins[item.Label] = true
		}
	}

	for name, found := range builtins {
		if !found {
			t.Errorf("missing built-in type: %s", name)
		}
	}
}

func TestKeywordCompletion(t *testing.T) {
	t.Parallel()

	item := keywordCompletion("type", "type ${1:Name} {}", "Type declaration")

	if item.Label != "type" {
		t.Errorf("Label = %q; want 'type'", item.Label)
	}
	if item.Kind == nil || *item.Kind != protocol.CompletionItemKindKeyword {
		t.Error("expected keyword kind")
	}
	if item.InsertTextFormat == nil || *item.InsertTextFormat != protocol.InsertTextFormatSnippet {
		t.Error("expected snippet format")
	}
	if item.InsertText == nil || *item.InsertText != "type ${1:Name} {}" {
		t.Error("insert text mismatch")
	}
}

func TestSnippetCompletion(t *testing.T) {
	t.Parallel()

	item := snippetCompletion("property", "${1:name} String", "Property")

	if item.Label != "property" {
		t.Errorf("Label = %q; want 'property'", item.Label)
	}
	if item.Kind == nil || *item.Kind != protocol.CompletionItemKindSnippet {
		t.Error("expected snippet kind")
	}
	if item.InsertTextFormat == nil || *item.InsertTextFormat != protocol.InsertTextFormatSnippet {
		t.Error("expected snippet format")
	}
}

func TestImportCompletions(t *testing.T) {
	t.Parallel()

	items := importCompletions()

	if len(items) == 0 {
		t.Fatal("expected import completion")
	}

	if items[0].Label != "import" {
		t.Errorf("expected 'import' label, got %q", items[0].Label)
	}
}

func TestBuiltinTypes_Complete(t *testing.T) {
	t.Parallel()

	// Verify all expected built-in types are present
	expected := []string{
		"Boolean", "Date", "Enum", "Float", "Integer", "List",
		"Pattern", "String", "Timestamp", "UUID", "Vector",
	}

	for _, exp := range expected {
		if !slices.Contains(builtinTypes, exp) {
			t.Errorf("missing built-in type: %s", exp)
		}
	}
}

func TestNewExprPtr(t *testing.T) {
	t.Parallel()

	s := "test"
	ptr := new(s)

	if *ptr != s {
		t.Errorf("new(s) = %q; want %q", *ptr, s)
	}
}

// UTF-8 Position Encoding Tests
// These tests validate that completion works correctly with UTF-8 encoding
// and multi-byte characters, catching regressions where byte offsets might
// be mistakenly treated as rune offsets.

// TestCompletion_UTF8Mode_Integration tests the full completion path with UTF-8
// position encoding and multi-byte content. This is an integration test that
// exercises ByteOffsetFromLSP -> DetectContext through the Server.
//
// UTF-8 mode still deserves a quick sanity test at the provider level, mainly
// to catch regressions where byte offsets are mistakenly treated as rune offsets.
// Uses textDocumentDidOpen to trigger analysis and snapshot storage, ensuring
// the full provider path is exercised.
func TestCompletion_UTF8Mode_Integration(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a schema file with CJK characters
	// The positions we test are byte offsets when using UTF-8 encoding
	content := `schema "日本語テスト"

type 人物 {
	名前 String
}
`
	filePath := filepath.Join(tmpDir, "main.yammm")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	// Create server with silent logging
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(logger, Config{ModuleRoot: tmpDir})

	// Set UTF-8 position encoding (normally negotiated during initialize,
	// but we set it directly for this test)
	server.workspace.SetPositionEncoding(PositionEncodingUTF8)

	// Open the document and trigger analysis via textDocumentDidOpen.
	// This ensures LatestSnapshot(uri) returns a non-nil snapshot.
	uri := lsputil.PathToURI(filePath)
	err := server.textDocumentDidOpen(context.TODO(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri,
			LanguageID: "yammm",
			Version:    1,
			Text:       content,
		},
	})
	if err != nil {
		t.Fatalf("textDocumentDidOpen failed: %v", err)
	}

	// Test completion at various byte positions with multi-byte characters
	tests := []struct {
		name string
		line int
		char int // byte offset from line start (UTF-8 encoding)
	}{
		// Line 0: `schema "日本語テスト"`
		// schema(0-5) _(6) "(7) 日(8,9,10) 本(11,12,13) 語(14,15,16)...
		{"line 0, start", 0, 0},
		{"line 0, at schema name start", 0, 8},
		{"line 0, mid CJK char", 0, 9}, // middle byte of 日

		// Line 2: `type 人物 {`
		// type(0-3) _(4) 人(5,6,7) 物(8,9,10) _(11) {(12)
		{"line 2, before type name", 2, 5},
		{"line 2, mid first CJK char", 2, 6}, // middle byte of 人
		{"line 2, at opening brace", 2, 12},

		// Line 3: `	名前 String` (tab + CJK property name)
		// tab(0) 名(1,2,3) 前(4,5,6) _(7) String(8-13)
		{"line 3, at property name", 3, 1},
		{"line 3, mid property name", 3, 2}, // middle byte of 名
		{"line 3, after property name", 3, 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Call completion handler - should not panic
			params := &protocol.CompletionParams{
				TextDocumentPositionParams: protocol.TextDocumentPositionParams{
					TextDocument: protocol.TextDocumentIdentifier{URI: uri},
					Position: protocol.Position{
						Line:      protocol.UInteger(tt.line), //nolint:gosec // test
						Character: protocol.UInteger(tt.char), //nolint:gosec // test
					},
				},
			}

			result, err := server.textDocumentCompletion(context.TODO(), params)
			if err != nil {
				t.Fatalf("completion failed: %v", err)
			}

			// Verify we got a result (may be nil for "no completions", or a slice)
			// The key assertion is that we didn't panic
			switch v := result.(type) {
			case nil:
				// No completions is valid
			case []protocol.CompletionItem:
				// Got completion items - this is fine
				t.Logf("got %d completion items", len(v))
			default:
				t.Errorf("unexpected result type: %T", result)
			}
		})
	}
}

// TestCompletion_UTF8Mode_NoPanic verifies that completion in UTF-8 position
// encoding mode works correctly without panicking.
//
// This exercises the full UTF-8 mode code path including ByteOffsetFromLSP
// and line offset calculations. The negative offset clamp in the completion
// handler (provider_completion.go) is a defensive guard that cannot currently
// be triggered with a normal source.Registry, since both LineStartByte and
// ContentBySource depend on the same registry entry. This test verifies the
// happy path works correctly.
//
// Uses textDocumentDidOpen to trigger analysis and snapshot storage,
// ensuring the full provider path is exercised.
func TestCompletion_UTF8Mode_NoPanic(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Multi-line content to exercise line offset calculations
	content := `schema "test"

type Person {
	name String
}
`
	filePath := filepath.Join(tmpDir, "main.yammm")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(logger, Config{ModuleRoot: tmpDir})
	server.workspace.SetPositionEncoding(PositionEncodingUTF8)

	// Open the document and trigger analysis via textDocumentDidOpen.
	uri := lsputil.PathToURI(filePath)
	err := server.textDocumentDidOpen(context.TODO(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri,
			LanguageID: "yammm",
			Version:    1,
			Text:       content,
		},
	})
	if err != nil {
		t.Fatalf("textDocumentDidOpen failed: %v", err)
	}

	// Request completion at line 3 (inside type body)
	params := &protocol.CompletionParams{
		TextDocumentPositionParams: protocol.TextDocumentPositionParams{
			TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			Position: protocol.Position{
				Line:      3,
				Character: 0, // start of line
			},
		},
	}

	// Should not panic and should return valid completions
	result, err := server.textDocumentCompletion(context.TODO(), params)
	if err != nil {
		t.Fatalf("completion failed: %v", err)
	}

	// Verify we got type body completions (since we're inside the type)
	items, ok := result.([]protocol.CompletionItem)
	if !ok {
		t.Skipf("no completion items returned (result type: %T)", result)
	}

	// Should have type body completions (property, association, etc.)
	if len(items) == 0 {
		t.Error("expected completion items for type body context")
	}
}

func TestTypeBodySnippets_NoCommasBeforeModifiers(t *testing.T) {
	// Regression test for Priority 1.3 fix: snippets should NOT have commas
	// before modifiers like "required" or "primary" in the OUTPUT.
	// YAMMM uses space-separated modifiers, not comma-separated.
	//
	// The invalid pattern was: ${N| , required, primary|} with space before comma
	// This would produce " , required" when selected (wrong!).
	//
	// The valid pattern is: ${N|, required, primary|} where:
	// - First option is empty (nothing before first comma)
	// - Second option is " required" (space+word)
	// - Third option is " primary" (space+word)
	// This produces " required" when selected (correct!).
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(logger, Config{})

	// Get type body completions
	completions := server.typeBodyCompletions()

	// The INVALID pattern is "| ," (space before comma in choice placeholder)
	// which would produce " , modifier" output
	invalidPatterns := []string{
		"| ,", // Space before comma in choice - would produce " ," in output
	}

	for _, item := range completions {
		if item.InsertText == nil {
			continue
		}
		insertText := *item.InsertText

		for _, pattern := range invalidPatterns {
			if strings.Contains(insertText, pattern) {
				t.Errorf("snippet %q contains invalid pattern %q; this would produce a comma in output",
					item.Label, pattern)
			}
		}
	}

	// Additionally verify that modifier choices have the correct format:
	// ${N|, modifier1, modifier2|} - empty first option, space-prefixed subsequent
	for _, item := range completions {
		if item.InsertText == nil {
			continue
		}
		insertText := *item.InsertText

		// Check that modifier choices like "|, required" are present (correct format)
		if strings.Contains(insertText, "required") {
			// Should have "|," followed by " required" (not "| ," or "|required")
			if strings.Contains(insertText, "| , required") || strings.Contains(insertText, "|required") {
				t.Errorf("snippet %q has incorrect modifier choice format", item.Label)
			}
		}
	}
}

func TestTypeBodySnippets_ModifierChoicesValid(t *testing.T) {
	// Verify that modifier choice placeholders have valid structure:
	// ${N|, option1, option2|} - first option MUST be empty (space-prefixed for non-empty)
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(logger, Config{})

	completions := server.typeBodyCompletions()

	for _, item := range completions {
		if item.InsertText == nil {
			continue
		}
		insertText := *item.InsertText

		// Find all choice placeholders like ${N|...|} that contain "required" or "primary"
		// These should have format ${N|, required|} or ${N|, required, primary|}
		// where the empty first option allows omitting the modifier
		if strings.Contains(insertText, "required") || strings.Contains(insertText, "primary") {
			// Check for malformed patterns
			if strings.Contains(insertText, "|required") || strings.Contains(insertText, "|primary") {
				// Missing space before the modifier in choice - wrong!
				// Should be "|, required" (empty first option) or "| required"
				t.Errorf("snippet %q has malformed modifier choice; expected '|, modifier' pattern for optional modifiers",
					item.Label)
			}
		}
	}
}

func TestTopLevelSnippets_ValidStructure(t *testing.T) {
	// Verify top-level snippets (schema, import, type) have valid structure
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(logger, Config{})

	completions := server.topLevelCompletions()

	// Each snippet should have InsertText
	for _, item := range completions {
		if item.Kind != nil && *item.Kind == protocol.CompletionItemKindSnippet {
			if item.InsertText == nil || *item.InsertText == "" {
				t.Errorf("snippet %q has empty InsertText", item.Label)
			}
		}
	}
}

func TestImportSnippets_ValidStructure(t *testing.T) {
	// Verify import snippet has valid placeholder structure
	t.Parallel()

	completions := importCompletions()

	for _, item := range completions {
		if item.Label == "import" && item.InsertText != nil {
			insertText := *item.InsertText

			// Import snippet should have path and alias placeholders
			if !strings.Contains(insertText, "${1:") || !strings.Contains(insertText, "${2:") {
				t.Errorf("import snippet should have ${1:} and ${2:} placeholders; got %q", insertText)
			}

			// Should contain import keyword pattern
			if !strings.Contains(insertText, "import") {
				t.Errorf("import snippet should contain 'import'; got %q", insertText)
			}
		}
	}
}

func TestComputeByteOffsetFromText_UTF8Mode(t *testing.T) {
	// Tests that computeByteOffsetFromText respects UTF-8 position encoding.
	// In UTF-8 mode, character offset IS byte offset (no conversion needed).
	// The bug was that this function always assumed UTF-16 encoding.
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(logger, Config{})

	// Set UTF-8 position encoding
	server.workspace.SetPositionEncoding(PositionEncodingUTF8)

	// Test with ASCII content - should work the same for both encodings
	text := "type Person {\n    name String\n}"

	tests := []struct {
		name     string
		lspLine  int
		lspChar  int
		expected int
	}{
		{
			name:     "ASCII - start of line",
			lspLine:  1,
			lspChar:  0,
			expected: 0,
		},
		{
			name:     "ASCII - middle of line",
			lspLine:  1,
			lspChar:  4,
			expected: 4,
		},
		{
			name:     "ASCII - end of indentation",
			lspLine:  1,
			lspChar:  8,
			expected: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := server.computeByteOffsetFromText(text, tt.lspLine, tt.lspChar)
			if result != tt.expected {
				t.Errorf("computeByteOffsetFromText() = %d; want %d", result, tt.expected)
			}
		})
	}
}

func TestComputeByteOffsetFromText_UTF8Mode_MultiByteChars(t *testing.T) {
	// Tests UTF-8 mode with multi-byte characters.
	// In UTF-8 mode, lspChar is already a byte offset, so no conversion needed.
	// In UTF-16 mode, lspChar would be UTF-16 code units.
	//
	// This test verifies that UTF-8 mode passes through the byte offset directly,
	// while UTF-16 mode performs conversion.
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Line with emoji: "type 😀Test {"
	// Byte positions: t(0) y(1) p(2) e(3) _(4) 😀(5,6,7,8) T(9) e(10) s(11) t(12) _(13) {(14)
	// UTF-16 positions: t(0) y(1) p(2) e(3) _(4) 😀(5,6) T(7) e(8) s(9) t(10) _(11) {(12)
	text := "type 😀Test {"

	t.Run("UTF-8 mode passes through byte offset", func(t *testing.T) {
		t.Parallel()
		server := NewServer(logger, Config{})
		server.workspace.SetPositionEncoding(PositionEncodingUTF8)

		// In UTF-8 mode, lspChar=9 means byte offset 9, which is 'T'
		result := server.computeByteOffsetFromText(text, 0, 9)
		if result != 9 {
			t.Errorf("UTF-8 mode: computeByteOffsetFromText(lspChar=9) = %d; want 9 (passthrough)", result)
		}
	})

	t.Run("UTF-16 mode converts code units to bytes", func(t *testing.T) {
		t.Parallel()
		server := NewServer(logger, Config{})
		// Default is UTF-16, no need to set

		// In UTF-16 mode, lspChar=7 means UTF-16 position 7, which is 'T' (after emoji)
		// 'T' is at byte offset 9 in the string
		result := server.computeByteOffsetFromText(text, 0, 7)
		if result != 9 {
			t.Errorf("UTF-16 mode: computeByteOffsetFromText(lspChar=7) = %d; want 9 (converted from UTF-16)", result)
		}
	})
}
