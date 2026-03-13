package lsp

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/testutil"
	"github.com/simon-lentz/yammm/schema/load"
)

func TestFormatting_UsesTokenStreamFormatterForIntraLineSpacing(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	content := "schema \"test\"\n\ntype   A{\n\tname String\n}\n"
	filePath := filepath.Join(tmpDir, "test.yammm")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(logger, Config{ModuleRoot: tmpDir})
	uri := lsputil.PathToURI(filePath)

	if err := server.textDocumentDidOpen(context.TODO(), &protocol.DidOpenTextDocumentParams{
		TextDocument: protocol.TextDocumentItem{
			URI:        uri,
			LanguageID: "yammm",
			Version:    1,
			Text:       content,
		},
	}); err != nil {
		t.Fatalf("textDocumentDidOpen failed: %v", err)
	}

	edits, err := server.textDocumentFormatting(context.TODO(), &protocol.DocumentFormattingParams{
		TextDocument: protocol.TextDocumentIdentifier{URI: uri},
	})
	if err != nil {
		t.Fatalf("textDocumentFormatting failed: %v", err)
	}

	if len(edits) == 0 {
		t.Fatal("expected formatting edits for intra-line spacing normalization")
	}

	edit := edits[0]
	if edit.Range.Start.Line != 0 || edit.Range.Start.Character != 0 {
		t.Errorf("edit range should start at 0,0; got %d,%d", edit.Range.Start.Line, edit.Range.Start.Character)
	}
	if !strings.Contains(edit.NewText, "type A {") {
		t.Errorf("expected formatted text to normalize type spacing, got:\n%s", edit.NewText)
	}
}

func TestFormatting_UTF8PositionEncoding(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a schema file with CJK characters that needs formatting.
	// In UTF-8 mode, the edit range's Character field should be byte count.
	// In UTF-16 mode, it should be UTF-16 code units.
	// CJK characters are 3 bytes in UTF-8 but 1 UTF-16 code unit each.
	content := "schema \"日本語\"\n\ntype Person {    \n\tname String\n}\n"
	filePath := filepath.Join(tmpDir, "test.yammm")
	if err := os.WriteFile(filePath, []byte(content), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}

	// Test both encodings
	tests := []struct {
		name     string
		encoding PositionEncoding
	}{
		{"UTF-16", PositionEncodingUTF16},
		{"UTF-8", PositionEncodingUTF8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			server := NewServer(logger, Config{ModuleRoot: tmpDir})

			// Set position encoding
			server.workspace.SetPositionEncoding(tt.encoding)

			// Open the document
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

			// Request formatting
			edits, err := server.textDocumentFormatting(context.TODO(), &protocol.DocumentFormattingParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			})
			if err != nil {
				t.Fatalf("textDocumentFormatting failed: %v", err)
			}

			if len(edits) == 0 {
				// document doesn't need formatting (trailing spaces removed in formatDocument)
				// This is acceptable - the test just verifies no crash with encoding switch
				return
			}

			// Verify the edit range covers the document (starts at 0,0)
			edit := edits[0]
			if edit.Range.Start.Line != 0 || edit.Range.Start.Character != 0 {
				t.Errorf("edit range should start at 0,0; got %d,%d",
					edit.Range.Start.Line, edit.Range.Start.Character)
			}

			// For UTF-8, the character should be byte offset (larger for multi-byte chars)
			// For UTF-16, the character should be code units (smaller for BMP chars)
			// The test primarily verifies that the call completes without panic
			// and returns a valid edit when the encoding is switched
		})
	}
}

func TestIntegration_FormattingRoundTrip_Multibyte(t *testing.T) {
	t.Parallel()

	// CJK characters: 3 bytes UTF-8, 1 UTF-16 code unit each.
	// Emoji 😀: 4 bytes UTF-8, 2 UTF-16 code units (surrogate pair).
	// Emoji on the last line exercises the end-range computation at provider_format.go:73-88.
	unformatted := "schema \"日本語\"\n\ntype   Person{\n    name String  required // 名前\n    tag String\n}\n// 最後 😀\n"
	expected := "schema \"日本語\"\n\ntype Person {\n\tname String required // 名前\n\ttag  String\n}\n// 最後 😀\n"

	tests := []struct {
		name     string
		encoding PositionEncoding
	}{
		{"UTF-16", PositionEncodingUTF16},
		{"UTF-8", PositionEncodingUTF8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			filePath := filepath.Join(tmpDir, "test.yammm")
			if err := os.WriteFile(filePath, []byte(unformatted), 0o600); err != nil {
				t.Fatalf("failed to write file: %v", err)
			}

			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			server := NewServer(logger, Config{ModuleRoot: tmpDir})
			server.workspace.SetPositionEncoding(tt.encoding)

			uri := lsputil.PathToURI(filePath)
			err := server.textDocumentDidOpen(context.TODO(), &protocol.DidOpenTextDocumentParams{
				TextDocument: protocol.TextDocumentItem{
					URI:        uri,
					LanguageID: "yammm",
					Version:    1,
					Text:       unformatted,
				},
			})
			if err != nil {
				t.Fatalf("textDocumentDidOpen failed: %v", err)
			}

			edits, err := server.textDocumentFormatting(context.TODO(), &protocol.DocumentFormattingParams{
				TextDocument: protocol.TextDocumentIdentifier{URI: uri},
			})
			if err != nil {
				t.Fatalf("textDocumentFormatting failed: %v", err)
			}

			if len(edits) == 0 {
				t.Fatal("expected formatting edits, got none")
			}

			// Apply edits with the test encoding.
			result := testutil.ApplyEdits(unformatted, edits, string(tt.encoding))
			if result != expected {
				t.Errorf("round-trip result != expected\ngot:\n%q\nwant:\n%q", result, expected)
			}

			// Verify the result parses and has the expected schema name.
			ctx := t.Context()
			s, _, err := load.LoadString(ctx, result, "test.yammm")
			if err != nil {
				t.Fatalf("formatted output failed to parse: %v", err)
			}
			if s.Name() != "日本語" {
				t.Errorf("schema name = %q; want %q", s.Name(), "日本語")
			}
		})
	}
}
