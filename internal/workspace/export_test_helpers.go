package workspace

import (
	"log/slog"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/markdown"
)

// SetMarkdownBlocksForTest injects blocks and snapshots into an open markdown
// document's internal state. This exists solely for test code that needs to
// set up specific block configurations (e.g., nil snapshots) that cannot be
// produced through the normal analysis pipeline.
//
// The URI must already be open via MarkdownDocumentOpened.
func (w *Workspace) SetMarkdownBlocksForTest(uri string, blocks []markdown.CodeBlock, snapshots []*analysis.Snapshot) {
	w.mu.Lock()
	defer w.mu.Unlock()
	md := w.markdownDocs[uri]
	if md == nil {
		return
	}
	md.Blocks = blocks
	md.Snapshots = snapshots
}

// MarkdownDocumentChangedForTest exposes markdownDocumentChanged for tests
// in external packages that need to exercise version-gating behavior.
func (w *Workspace) MarkdownDocumentChangedForTest(uri string, version int, text string) {
	w.markdownDocumentChanged(uri, version, text)
}

// MarkdownDocumentClosedForTest exposes markdownDocumentClosed for tests
// in external packages that need to exercise cleanup behavior.
func (w *Workspace) MarkdownDocumentClosedForTest(notify NotifyFunc, uri string) {
	w.markdownDocumentClosed(notify, uri)
}

// SetMarkdownVersionForTest sets the version of a markdown document directly.
// This exists for version-gate tests that need to simulate stale analysis.
func (w *Workspace) SetMarkdownVersionForTest(uri string, version int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	md := w.markdownDocs[uri]
	if md == nil {
		return
	}
	md.Version = version
}

// MergeIncrementalChangesForTest exposes mergeIncrementalChanges for external tests.
func MergeIncrementalChangesForTest(currentText string, enc lsputil.PositionEncoding, changes []any, logger *slog.Logger) string {
	return mergeIncrementalChanges(currentText, enc, changes, logger)
}
