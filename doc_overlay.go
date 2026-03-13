package lsp

import (
	"log/slog"
	"path/filepath"

	"github.com/simon-lentz/yammm/location"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/markdown"
)

// docOverlay stores open document state for both .yammm files and markdown files.
//
// All fields are protected by Workspace.mu (external lock).
// Callers must hold Workspace.mu before calling any method.
type docOverlay struct {
	// Open .yammm documents keyed by URI
	open map[string]*document

	// Open markdown documents keyed by URI
	markdownDocs map[string]*markdownDocument

	// Counter for deterministic document ordering (symlink disambiguation)
	openCounter int
}

// openDocument creates or replaces a document in the overlay.
// It resolves symlinks to compute the canonical SourceID, normalizes line endings,
// and eagerly computes lineState for completion context detection.
//
// Must be called with Workspace.mu held.
func (d *docOverlay) openDocument(uri string, version int, text string, logger *slog.Logger) {
	path, err := lsputil.URIToPath(uri)
	if err != nil {
		logger.Warn("failed to parse document URI",
			slog.String("uri", uri),
			slog.String("error", err.Error()),
		)
		return
	}

	// Resolve symlinks to get canonical path matching the loader's behavior.
	// The loader uses makeCanonicalPath which resolves symlinks, so we need
	// to do the same here to ensure SourceID matches loader output.
	// Note: EvalSymlinks also cleans the path, but we call Clean explicitly
	// to make the invariant visible (canonical paths are always clean).
	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = filepath.Clean(resolved)
	}

	sourceID, err := location.SourceIDFromAbsolutePath(canonicalPath)
	if err != nil {
		logger.Warn("failed to create source ID",
			slog.String("path", canonicalPath),
			slog.String("error", err.Error()),
		)
		return
	}

	// Normalize line endings (CRLF/CR → LF) for consistent offset calculations.
	// Windows clients may send CRLF, which would cause mismatches with
	// line-based operations if not normalized here at the storage layer.
	text = normalizeLineEndings(text)

	// Eagerly compute lineState on the write path to avoid lock juggling in
	// GetDocumentSnapshot. For typical yammm files (<1000 lines) this is
	// sub-millisecond and dominated by the 150ms debounce delay.
	depths, inComment := computeBraceDepths(text)

	d.openCounter++
	d.open[uri] = &document{
		URI:       uri,
		SourceID:  sourceID,
		Version:   version,
		Text:      text,
		OpenOrder: d.openCounter,
		lineState: &lineState{
			Version:        version,
			BraceDepth:     depths,
			InBlockComment: inComment,
		},
	}
}

// changeDocument updates an existing document's text and version.
// Ignores stale updates where version <= current version (unless version is 0/unknown).
//
// Must be called with Workspace.mu held.
func (d *docOverlay) changeDocument(uri string, version int, text string, logger *slog.Logger) {
	doc, ok := d.open[uri]
	if !ok {
		return
	}

	// Ignore stale updates (version <= current) unless version is 0 (unknown).
	// This prevents out-of-order updates from overwriting newer content.
	if version != 0 && doc.Version != 0 && version <= doc.Version {
		logger.Debug("ignoring stale document change",
			slog.String("uri", uri),
			slog.Int("incoming_version", version),
			slog.Int("current_version", doc.Version),
		)
		return
	}
	doc.Version = version
	// Normalize line endings (CRLF/CR → LF) for consistent offset calculations.
	doc.Text = normalizeLineEndings(text)
	// Eagerly recompute lineState on every change. For typical yammm files
	// (<1000 lines) this is sub-millisecond and eliminates lock juggling
	// in GetDocumentSnapshot.
	depths, inComment := computeBraceDepths(doc.Text)
	doc.lineState = &lineState{
		Version:        doc.Version,
		BraceDepth:     depths,
		InBlockComment: inComment,
	}
}

// removeDocument removes a document from the overlay.
//
// Must be called with Workspace.mu held.
func (d *docOverlay) removeDocument(uri string) {
	delete(d.open, uri)
}

// getSnapshot returns an immutable snapshot of the document for a URI.
// The snapshot contains a copy of the document state at the time of the call,
// allowing safe access outside of locks without racing with changeDocument.
// Returns nil if the document is not open.
//
// Must be called with Workspace.mu held (at least RLock).
func (d *docOverlay) getSnapshot(uri string) *documentSnapshot {
	doc := d.open[uri]
	if doc == nil {
		return nil
	}

	return &documentSnapshot{
		URI:       doc.URI,
		SourceID:  doc.SourceID,
		Version:   doc.Version,
		Text:      doc.Text,
		lineState: doc.lineState,
	}
}

// collectOverlays builds an overlay map from all open documents.
// Uses canonical SourceID as key to ensure symlinks and path variations
// map to the same entry that the loader will use.
//
// Must be called with Workspace.mu held (at least RLock).
func (d *docOverlay) collectOverlays() map[string][]byte {
	overlays := make(map[string][]byte, len(d.open))
	for _, doc := range d.open {
		overlays[doc.SourceID.String()] = []byte(doc.Text)
	}
	return overlays
}

// allOpenURIs returns a list of all open .yammm document URIs.
//
// Must be called with Workspace.mu held (at least RLock).
func (d *docOverlay) allOpenURIs() []string {
	uris := make([]string, 0, len(d.open))
	for uri := range d.open {
		uris = append(uris, uri)
	}
	return uris
}

// openMarkdownDocument creates a markdownDocument with normalized text and version.
// Block extraction is deferred to analyzeMarkdownAndPublish.
//
// Must be called with Workspace.mu held.
func (d *docOverlay) openMarkdownDocument(uri string, version int, text string) {
	d.markdownDocs[uri] = &markdownDocument{
		URI:     uri,
		Version: version,
		Text:    normalizeLineEndings(text),
	}
}

// changeMarkdownDocument updates text and version for a markdown document.
// Ignores stale updates (version <= current unless either is 0).
// Does NOT re-extract blocks — that is done atomically by analyzeMarkdownAndPublish.
//
// Must be called with Workspace.mu held.
func (d *docOverlay) changeMarkdownDocument(uri string, version int, text string, logger *slog.Logger) {
	md := d.markdownDocs[uri]
	if md == nil {
		return
	}

	if version != 0 && md.Version != 0 && version <= md.Version {
		logger.Debug("ignoring stale markdown document change",
			slog.String("uri", uri),
			slog.Int("incoming_version", version),
			slog.Int("current_version", md.Version),
		)
		return
	}
	md.Version = version
	md.Text = normalizeLineEndings(text)
}

// removeMarkdownDocument removes a markdown document from the overlay.
//
// Must be called with Workspace.mu held.
func (d *docOverlay) removeMarkdownDocument(uri string) {
	delete(d.markdownDocs, uri)
}

// getMarkdownSnapshot returns an immutable snapshot of a markdown document.
// Returns nil if the document is not open.
//
// Must be called with Workspace.mu held (at least RLock).
func (d *docOverlay) getMarkdownSnapshot(uri string) *markdownDocumentSnapshot {
	md := d.markdownDocs[uri]
	if md == nil {
		return nil
	}

	blocks := make([]markdown.CodeBlock, len(md.Blocks))
	copy(blocks, md.Blocks)

	snapshots := make([]*analysis.Snapshot, len(md.Snapshots))
	copy(snapshots, md.Snapshots)

	return &markdownDocumentSnapshot{
		URI:       md.URI,
		Version:   md.Version,
		Blocks:    blocks,
		Snapshots: snapshots,
	}
}

// getMarkdownCurrentText returns the current text of a markdown document.
//
// Must be called with Workspace.mu held (at least RLock).
func (d *docOverlay) getMarkdownCurrentText(uri string) (string, bool) {
	md := d.markdownDocs[uri]
	if md == nil {
		return "", false
	}
	return md.Text, true
}
