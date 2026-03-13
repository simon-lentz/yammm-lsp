package lsp

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/simon-lentz/yammm-lsp/internal/docstate"
	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"
	"github.com/simon-lentz/yammm-lsp/internal/workspace"
)

func TestNewServer(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	cfg := Config{ModuleRoot: "/test/root"}

	server := NewServer(logger, cfg)

	if server == nil {
		t.Fatal("NewServer() returned nil")
	}
	if server.logger == nil {
		t.Error("server.logger is nil")
	}
	if server.workspace == nil {
		t.Error("server.workspace is nil")
	}
	if server.mux == nil {
		t.Error("server.mux is nil")
	}
	if server.config.ModuleRoot != "/test/root" {
		t.Errorf("config.ModuleRoot = %q; want /test/root", server.config.ModuleRoot)
	}
}

func TestConfig_ModuleRoot(t *testing.T) {
	t.Parallel()

	cfg := Config{ModuleRoot: "/custom/path"}

	if cfg.ModuleRoot != "/custom/path" {
		t.Errorf("ModuleRoot = %q; want /custom/path", cfg.ModuleRoot)
	}
}

func TestConfig_Empty(t *testing.T) {
	t.Parallel()

	cfg := Config{}

	if cfg.ModuleRoot != "" {
		t.Errorf("ModuleRoot = %q; want empty", cfg.ModuleRoot)
	}
}

func TestServer_Shutdown(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	server := NewServer(logger, Config{})

	// Shutdown should not panic
	server.Shutdown()
}

func TestServer_Close(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	server := NewServer(logger, Config{})

	// Close before RunStdio should be safe (GetStdio returns nil)
	err1 := server.Close()
	if err1 != nil {
		t.Errorf("first Close() error: %v", err1)
	}

	// Close is idempotent: subsequent calls return the same result
	err2 := server.Close()
	if err2 != nil {
		t.Errorf("second Close() error: %v", err2)
	}

	// Third close for good measure
	err3 := server.Close()
	if err3 != nil {
		t.Errorf("third Close() error: %v", err3)
	}
}

func TestServer_WorkspaceCreated(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	server := NewServer(logger, Config{ModuleRoot: "/test"})

	// Verify workspace was created with correct config
	if server.workspace == nil {
		t.Error("server.workspace should not be nil")
	}

	// The workspace should inherit the config's module root
	root := server.workspace.FindModuleRoot("/any/path/file.yammm")
	if root != "/test" {
		t.Errorf("workspace.findModuleRoot() = %q; want /test", root)
	}
}

func TestServerName_Constant(t *testing.T) {
	t.Parallel()

	if serverName != "yammm-lsp" {
		t.Errorf("serverName = %q; want yammm-lsp", serverName)
	}
}

func TestChangeDocument_IncrementalMultipleChanges(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := workspace.NewWorkspace(logger, workspace.Config{})

	uri := "file:///test/multi-change.yammm"

	// Open document with initial content: "line1\nline2\nline3"
	ws.DocumentOpened(uri, 1, "line1\nline2\nline3")

	// Apply multiple incremental changes via ChangeDocument.
	// This tests that line offsets are correctly recomputed after each change.
	//
	// Change 1: Insert "X" at line 0, char 5 → "line1X\nline2\nline3"
	// Change 2: Insert "Y" at line 1, char 5 → "line1X\nline2Y\nline3"
	// Change 3: Insert "Z" at line 2, char 5 → "line1X\nline2Y\nline3Z"
	changes := []any{
		protocol.TextDocumentContentChangeEvent{
			Range: &protocol.Range{
				Start: protocol.Position{Line: 0, Character: 5},
				End:   protocol.Position{Line: 0, Character: 5},
			},
			Text: "X",
		},
		protocol.TextDocumentContentChangeEvent{
			Range: &protocol.Range{
				Start: protocol.Position{Line: 1, Character: 5},
				End:   protocol.Position{Line: 1, Character: 5},
			},
			Text: "Y",
		},
		protocol.TextDocumentContentChangeEvent{
			Range: &protocol.Range{
				Start: protocol.Position{Line: 2, Character: 5},
				End:   protocol.Position{Line: 2, Character: 5},
			},
			Text: "Z",
		},
	}

	ws.ChangeDocument(nil, uri, 2, changes)

	doc := ws.GetDocumentSnapshot(uri)
	if doc == nil {
		t.Fatal("document not found after changes")
	}

	expected := "line1X\nline2Y\nline3Z"
	if doc.Text != expected {
		t.Errorf("after multi-change:\ngot:  %q\nwant: %q", doc.Text, expected)
	}
}

func TestChangeDocument_IncrementalMultibyteUTF16(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := workspace.NewWorkspace(logger, workspace.Config{})

	uri := "file:///test/multibyte.yammm"

	// Content with multibyte characters: emoji takes 2 UTF-16 code units
	// "hello 🎉 world" - the emoji is at byte offset 6, but UTF-16 offset 6
	ws.DocumentOpened(uri, 1, "hello 🎉 world")

	// Insert "X" after the emoji. In UTF-16, the emoji is 2 code units,
	// so position after emoji is character 8 (h=0,e=1,l=2,l=3,o=4, =5,🎉=6-7, =8)
	changes := []any{
		protocol.TextDocumentContentChangeEvent{
			Range: &protocol.Range{
				Start: protocol.Position{Line: 0, Character: 8},
				End:   protocol.Position{Line: 0, Character: 8},
			},
			Text: "X",
		},
	}

	ws.ChangeDocument(nil, uri, 2, changes)

	doc := ws.GetDocumentSnapshot(uri)
	if doc == nil {
		t.Fatal("document not found after changes")
	}

	expected := "hello 🎉X world"
	if doc.Text != expected {
		t.Errorf("after multibyte change:\ngot:  %q\nwant: %q", doc.Text, expected)
	}
}

func TestDidChange_MultipleFullSyncChanges(t *testing.T) {
	// Tests that when multiple TextDocumentContentChangeEventWhole events
	// are sent in a single didChange notification, only the LAST one is applied.
	// This is the correct behavior per LSP spec for full sync mode.
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	server := NewServer(logger, Config{})

	uri := "file:///test/multi-full-sync.yammm"

	// Open document with initial content
	server.workspace.DocumentOpened(uri, 1, "initial content")

	// Send multiple full-sync changes in one notification.
	// Only the LAST one should be applied.
	params := &protocol.DidChangeTextDocumentParams{
		TextDocument: protocol.VersionedTextDocumentIdentifier{
			TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri},
			Version:                2,
		},
		ContentChanges: []any{
			protocol.TextDocumentContentChangeEventWhole{
				Text: "first full sync - should be ignored",
			},
			protocol.TextDocumentContentChangeEventWhole{
				Text: "second full sync - should be ignored",
			},
			protocol.TextDocumentContentChangeEventWhole{
				Text: "third full sync - this should be the final content",
			},
		},
	}

	err := server.textDocumentDidChange(context.TODO(), params)
	if err != nil {
		t.Fatalf("textDocumentDidChange failed: %v", err)
	}

	doc := server.workspace.GetDocumentSnapshot(uri)
	if doc == nil {
		t.Fatal("document not found after changes")
	}

	expected := "third full sync - this should be the final content"
	if doc.Text != expected {
		t.Errorf("after multiple full-sync changes:\ngot:  %q\nwant: %q", doc.Text, expected)
	}
}

func TestChangeDocument_IncrementalCRLF(t *testing.T) {
	// Tests that CRLF line endings are handled correctly in incremental changes.
	// Windows clients may send documents with CRLF (\r\n) line endings.
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := workspace.NewWorkspace(logger, workspace.Config{})

	uri := "file:///test/crlf.yammm"

	// Open document with CRLF line endings
	ws.DocumentOpened(uri, 1, "line1\r\nline2\r\nline3")

	// Insert "X" at line 1, char 5 (after "line2")
	// Without proper CRLF handling, the byte offset would be wrong because
	// CRLF is 2 bytes but the code assumed 1 byte per newline.
	changes := []any{
		protocol.TextDocumentContentChangeEvent{
			Range: &protocol.Range{
				Start: protocol.Position{Line: 1, Character: 5},
				End:   protocol.Position{Line: 1, Character: 5},
			},
			Text: "X",
		},
	}

	ws.ChangeDocument(nil, uri, 2, changes)

	doc := ws.GetDocumentSnapshot(uri)
	if doc == nil {
		t.Fatal("document not found after changes")
	}

	// The text is normalized to LF and the change is applied correctly.
	// The stored text will have LF line endings (normalized from CRLF).
	expected := "line1\nline2X\nline3"
	if doc.Text != expected {
		t.Errorf("after CRLF change:\ngot:  %q\nwant: %q", doc.Text, expected)
	}
}

func TestChangeDocument_IncrementalMixedLineEndings(t *testing.T) {
	// Tests handling of mixed line endings (some LF, some CRLF).
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := workspace.NewWorkspace(logger, workspace.Config{})

	uri := "file:///test/mixed.yammm"

	// Open document with mixed line endings: CRLF then LF then CRLF
	ws.DocumentOpened(uri, 1, "line1\r\nline2\nline3\r\nline4")

	// Insert at line 2, char 5 (after "line3")
	changes := []any{
		protocol.TextDocumentContentChangeEvent{
			Range: &protocol.Range{
				Start: protocol.Position{Line: 2, Character: 5},
				End:   protocol.Position{Line: 2, Character: 5},
			},
			Text: "Y",
		},
	}

	ws.ChangeDocument(nil, uri, 2, changes)

	doc := ws.GetDocumentSnapshot(uri)
	if doc == nil {
		t.Fatal("document not found after changes")
	}

	// The text is normalized to LF and the change is applied correctly.
	expected := "line1\nline2\nline3Y\nline4"
	if doc.Text != expected {
		t.Errorf("after mixed line ending change:\ngot:  %q\nwant: %q", doc.Text, expected)
	}
}

func TestNormalizeLineEndings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"LF only", "line1\nline2\nline3", "line1\nline2\nline3"},
		{"CRLF only", "line1\r\nline2\r\nline3", "line1\nline2\nline3"},
		{"CR only", "line1\rline2\rline3", "line1\nline2\nline3"},
		{"mixed CRLF and LF", "line1\r\nline2\nline3\r\n", "line1\nline2\nline3\n"},
		{"mixed all types", "line1\r\nline2\rline3\nline4", "line1\nline2\nline3\nline4"},
		{"empty", "", ""},
		{"no newlines", "single line", "single line"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := docstate.NormalizeLineEndings(tt.input)
			if got != tt.want {
				t.Errorf("docstate.NormalizeLineEndings(%q) = %q; want %q", tt.input, got, tt.want)
			}
		})
	}
}
