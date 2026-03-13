package lsp

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"
	"github.com/simon-lentz/yammm-lsp/testutil"
)

// debounceWait is the time to wait for debounce to fire and analysis to complete.
// Set to ~2.7x the debounceDelay constant (150ms) for reliable settling across
// CI environments where timers may jitter.
const debounceWait = 400 * time.Millisecond

// newTestHarnessWithServer creates a harness with an initialized LSP server,
// returning both the harness and server for tests that need direct workspace access.
func newTestHarnessWithServer(t *testing.T, root string) (*testutil.Harness, *Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := NewServer(logger, Config{ModuleRoot: root})
	h := testutil.NewHarness(t, server.Mux(), root)
	require.NoError(t, h.Initialize(), "harness initialization failed")
	return h, server
}

// TestTemporal_DebouncePipeline verifies the full debounce → analyze → publish
// pipeline works end-to-end through the jrpc2 transport.
//
// Flow: open document (immediate analysis) → verify snapshot → change document
// (debounced analysis) → wait for debounce → verify snapshot updated.
func TestTemporal_DebouncePipeline(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	h, _ := newTestHarnessWithServer(t, tmpDir)
	defer h.Close()

	// Open document with type "Alpha" — triggers immediate analysis.
	contentA := "schema \"test\"\n\ntype Alpha {\n\tname String\n}\n"
	require.NoError(t, h.OpenDocument("test.yammm", contentA))
	h.Sync()

	// Snapshot from immediate analysis should contain Alpha.
	symbols, err := h.DocumentSymbols("test.yammm")
	require.NoError(t, err)
	testutil.AssertDocumentSymbolExists(t, symbols, "Alpha")

	// Change to content with type "Beta" — debounced analysis.
	contentB := "schema \"test\"\n\ntype Beta {\n\tage Integer\n}\n"
	require.NoError(t, h.ChangeDocument("test.yammm", contentB, 2))

	// Wait for debounce (150ms) + analysis + notification propagation.
	time.Sleep(debounceWait)
	h.Sync()

	// Snapshot should now contain Beta, not Alpha.
	symbols, err = h.DocumentSymbols("test.yammm")
	require.NoError(t, err)
	testutil.AssertDocumentSymbolExists(t, symbols, "Beta")
}

// TestTemporal_RapidEditsSettle verifies that rapid successive edits are
// debounced correctly: only the final content's analysis results are published.
// This exercises the debounce cancel-and-reschedule behavior under rapid typing.
func TestTemporal_RapidEditsSettle(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	h, _ := newTestHarnessWithServer(t, tmpDir)
	defer h.Close()

	// Open document with initial content.
	require.NoError(t, h.OpenDocument("test.yammm", "schema \"test\"\n"))
	h.Sync()

	// Send 10 rapid changes. Each change resets the debounce timer.
	// Only the final content should be analyzed after settling.
	for i := range 9 {
		require.NoError(t, h.ChangeDocument("test.yammm",
			fmt.Sprintf("schema \"test\"\n\ntype Intermediate%d {\n\tf String\n}\n", i), i+2))
	}

	// Final change: type "Final"
	finalContent := "schema \"test\"\n\ntype Final {\n\tname String\n}\n"
	require.NoError(t, h.ChangeDocument("test.yammm", finalContent, 11))

	// Wait for debounce to settle on the final content.
	time.Sleep(debounceWait)
	h.Sync()

	// Snapshot should reflect the final content.
	symbols, err := h.DocumentSymbols("test.yammm")
	require.NoError(t, err)
	testutil.AssertDocumentSymbolExists(t, symbols, "Final")
}

// TestTemporal_CloseCancelsPendingAnalysis verifies that closing a document
// cancels any pending debounced analysis and clears diagnostics. Even after
// the debounce timer would have fired, no stale diagnostics are published.
func TestTemporal_CloseCancelsPendingAnalysis(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	h, server := newTestHarnessWithServer(t, tmpDir)
	defer h.Close()

	// Open document — triggers immediate analysis and stores snapshot.
	require.NoError(t, h.OpenDocument("test.yammm",
		"schema \"test\"\n\ntype Alpha {\n\tname String\n}\n"))
	h.Sync()

	uri := testutil.PathToURI(filepath.Join(tmpDir, "test.yammm"))
	snap := server.workspace.LatestSnapshot(uri)
	require.NotNil(t, snap, "snapshot should exist after open")

	// Change document — starts 150ms debounce timer.
	// Use content that would produce diagnostics if analyzed.
	require.NoError(t, h.ChangeDocument("test.yammm", "not valid!!!", 2))

	// Close document immediately, before debounce fires.
	// This should cancel the pending analysis and clear diagnostics.
	require.NoError(t, h.CloseDocument("test.yammm"))
	h.Sync()

	// Wait past the debounce timer to verify no stale analysis fires.
	time.Sleep(debounceWait)
	h.Sync()

	// Snapshot should be nil (cleaned up by close).
	snap = server.workspace.LatestSnapshot(uri)
	assert.Nil(t, snap, "snapshot should be nil after close")

	// Diagnostics should be empty (not stale error diagnostics).
	diags := h.Diagnostics(uri)
	assert.Empty(t, diags, "diagnostics should be cleared after close")
}

// TestTemporal_ConcurrentOpenChangeClose exercises the workspace under
// concurrent document lifecycle operations across multiple URIs. Run with
// -race to detect data races in the lock-protected document overlay,
// snapshot storage, and dependency tracking.
//
// This test uses the unified OpenDocument/ChangeDocument/CloseDocument methods
// which trigger real analysis goroutines, unlike TestWorkspace_ConcurrentDocumentAccess
// which only tests the lower-level overlay operations.
func TestTemporal_ConcurrentOpenChangeClose(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := NewWorkspace(logger, Config{})

	const numURIs = 10
	const iterations = 20
	var wg sync.WaitGroup
	collector := &notificationCollector{}

	for i := range numURIs {
		uri := fmt.Sprintf("file:///test/file%d.yammm", i)

		// Writer goroutine: open, then rapid changes
		wg.Go(func() {
			for j := range iterations {
				content := fmt.Sprintf("schema \"test%d\"\n\ntype T%d_%d {\n\tf String\n}\n", i, i, j)
				if j == 0 {
					ws.OpenDocument(collector.notify, uri, 1, content)
				} else {
					ws.ChangeDocument(collector.notify, uri, j+1, []any{
						protocol.TextDocumentContentChangeEventWhole{Text: content},
					})
				}
			}
		})

		// Reader goroutine: concurrent snapshot reads
		wg.Go(func() {
			for range iterations {
				_ = ws.GetDocumentSnapshot(uri)
				_ = ws.LatestSnapshot(uri)
			}
		})
	}

	wg.Wait()

	// Clean up: close all documents and cancel pending analysis.
	for i := range numURIs {
		uri := fmt.Sprintf("file:///test/file%d.yammm", i)
		ws.CloseDocument(collector.notify, uri)
	}
	ws.Shutdown()
}

// TestTemporal_ConcurrentOpenCloseRace verifies that concurrent open and close
// of the SAME document from different goroutines doesn't cause panics or data
// races. This exercises the lock consolidation in documentClosed and the
// version gate in analyzeAndPublish.
func TestTemporal_ConcurrentOpenCloseRace(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ws := NewWorkspace(logger, Config{})

	const iterations = 50
	uri := "file:///test/race.yammm"
	var wg sync.WaitGroup

	// Opener goroutine: repeatedly open the same document
	wg.Go(func() {
		for i := range iterations {
			content := fmt.Sprintf("schema \"test\"\n\ntype T%d {\n\tf String\n}\n", i)
			ws.OpenDocument(nil, uri, i+1, content)
		}
	})

	// Closer goroutine: repeatedly close the same document
	wg.Go(func() {
		for range iterations {
			ws.CloseDocument(nil, uri)
		}
	})

	wg.Wait()
	ws.Shutdown()
}
