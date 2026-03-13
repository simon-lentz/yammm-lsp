package workspace

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm/location"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/docstate"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
)

// Config holds workspace configuration.
type Config struct {
	// ModuleRoot overrides the computed module root for import resolution.
	ModuleRoot string
}

// NotifyFunc is a function that sends LSP notifications.
// This type decouples notification sending from transport details.
type NotifyFunc func(method string, params any)

// Workspace manages the state of open documents and analysis results.
//
// Lock ordering: debounceMu must never be acquired while holding mu.
// Methods that need both must release mu before acquiring debounceMu.
type Workspace struct {
	// mu protects: docs, snapshots, posEncoding, deps, mapper, roots.
	mu sync.RWMutex

	logger *slog.Logger
	config Config

	// Workspace roots (from workspaceFolders)
	roots []string

	// docs stores open .yammm document state (overlays, snapshots, line state).
	docs docstate.Overlay

	// markdownDocs stores open markdown document state keyed by URI.
	markdownDocs map[string]*markdownDocument

	// Latest analysis snapshots keyed by entry URI
	snapshots map[string]*analysis.Snapshot

	// Position encoding negotiated with client
	posEncoding lsputil.PositionEncoding

	// deps tracks forward and reverse import dependencies between entry files.
	deps depGraph

	// sched manages debounced analysis scheduling and background context.
	// Has its own debounceMu lock (must never be acquired while holding mu).
	sched analysisScheduler

	// mapper manages canonical path mapping and per-entry publication tracking.
	mapper uriMapper

	// diagHash holds hash-based diagnostic deduplication state.
	// Separate from mu because publishDiagnostics is called outside the
	// workspace lock (to avoid deadlock on I/O).
	diagHash diagHashState
}

// NewWorkspace creates a new workspace.
// If logger is nil, slog.Default() is used.
func NewWorkspace(logger *slog.Logger, cfg Config) *Workspace {
	if logger == nil {
		logger = slog.Default()
	}

	// Canonicalize ModuleRoot if set, to ensure consistent path comparisons
	// with workspace roots (which are also canonicalized in AddRoot).
	if cfg.ModuleRoot != "" {
		if resolved, err := filepath.EvalSymlinks(cfg.ModuleRoot); err == nil {
			cfg.ModuleRoot = filepath.Clean(resolved)
		} else {
			cfg.ModuleRoot = filepath.Clean(cfg.ModuleRoot)
		}
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())

	return &Workspace{
		logger:      logger.With(slog.String("component", "workspace")),
		config:      cfg,
		roots:       make([]string, 0),
		snapshots:   make(map[string]*analysis.Snapshot),
		posEncoding: lsputil.PositionEncodingUTF16,
		diagHash: diagHashState{
			hashes: make(map[string]uint64),
		},
		docs: docstate.Overlay{
			Open: make(map[string]*docstate.Document),
		},
		markdownDocs: make(map[string]*markdownDocument),
		deps: depGraph{
			importsByEntry: make(map[string]map[string]struct{}),
			reverseDeps:    make(map[string]map[string]struct{}),
		},
		sched: analysisScheduler{
			debounces: make(map[string]*debounceEntry),
			bgCtx:     bgCtx,
			bgCancel:  bgCancel,
			analyzer:  analysis.NewAnalyzer(logger), // Pass base logger; analyzer adds its own component
		},
		mapper: uriMapper{
			PublishedByEntry: make(map[string]map[string]struct{}),
		},
	}
}

// OpenDocument stores document state and triggers immediate analysis.
// Routes to yammm or markdown handling based on URI extension.
// Unsupported file types are silently ignored (debug logged).
func (w *Workspace) OpenDocument(notify NotifyFunc, uri string, version int, text string) {
	switch {
	case lsputil.IsYammmURI(uri):
		w.DocumentOpened(uri, version, text)
		w.analyzeAndPublish(notify, w.sched.backgroundContext(), uri)
	case lsputil.IsMarkdownURI(uri):
		w.MarkdownDocumentOpened(uri, version, text)
		w.AnalyzeMarkdownAndPublish(notify, w.sched.backgroundContext(), uri)
	default:
		w.logger.Debug("ignoring open for unsupported file type", slog.String("uri", uri))
	}
}

// ChangeDocument updates document text and schedules debounced analysis.
// Handles both full-sync and incremental-change fallback internally.
func (w *Workspace) ChangeDocument(notify NotifyFunc, uri string, version int, changes []any) {
	text, ok := extractFullSyncText(changes)
	if !ok {
		text, ok = w.mergeIncrementalFallback(uri, changes)
		if !ok {
			return
		}
	}

	switch {
	case lsputil.IsYammmURI(uri):
		w.documentChanged(uri, version, text)
		w.scheduleAnalysis(notify, uri)
	case lsputil.IsMarkdownURI(uri):
		w.markdownDocumentChanged(uri, version, text)
		w.scheduleMarkdownAnalysis(notify, uri)
	default:
		w.logger.Debug("ignoring change for unsupported file type", slog.String("uri", uri))
	}
}

// CloseDocument cleans up document state and clears diagnostics.
func (w *Workspace) CloseDocument(notify NotifyFunc, uri string) {
	switch {
	case lsputil.IsYammmURI(uri):
		w.documentClosed(notify, uri)
	case lsputil.IsMarkdownURI(uri):
		w.markdownDocumentClosed(notify, uri)
	default:
		w.logger.Debug("ignoring close for unsupported file type", slog.String("uri", uri))
	}
}

// extractFullSyncText returns the text of the last full-sync change, if any.
func extractFullSyncText(changes []any) (string, bool) {
	if len(changes) == 0 {
		return "", false
	}
	var lastFullChange *protocol.TextDocumentContentChangeEventWhole
	for _, rawChange := range changes {
		if change, ok := rawChange.(protocol.TextDocumentContentChangeEventWhole); ok {
			lastFullChange = &change
		}
	}
	if lastFullChange != nil {
		return lastFullChange.Text, true
	}
	return "", false
}

// mergeIncrementalFallback handles incremental changes by merging them into the
// current document text. Returns the merged text and true if successful.
func (w *Workspace) mergeIncrementalFallback(uri string, changes []any) (string, bool) {
	if len(changes) == 0 {
		return "", false
	}
	if _, ok := changes[0].(protocol.TextDocumentContentChangeEvent); !ok {
		return "", false
	}

	w.logger.Warn("received incremental change but server advertises full sync",
		slog.String("uri", uri))

	var currentText string
	var found bool
	switch {
	case lsputil.IsYammmURI(uri):
		doc := w.GetDocumentSnapshot(uri)
		if doc == nil {
			w.logger.Warn("incremental change for unknown document", slog.String("uri", uri))
			return "", false
		}
		currentText = doc.Text
		found = true
	case lsputil.IsMarkdownURI(uri):
		currentText, found = w.GetMarkdownCurrentText(uri)
	}
	if !found {
		return "", false
	}

	merged := mergeIncrementalChanges(currentText, w.PositionEncoding(), changes, w.logger)
	return merged, true
}

// mergeIncrementalChanges applies incremental content changes to currentText
// and returns the merged result. It is a pure function with no side effects.
func mergeIncrementalChanges(currentText string, enc lsputil.PositionEncoding, changes []any, logger *slog.Logger) string {
	text := docstate.NormalizeLineEndings(currentText)

	for _, rawChange := range changes {
		change, ok := rawChange.(protocol.TextDocumentContentChangeEvent)
		if !ok {
			continue
		}
		if change.Range == nil {
			text = docstate.NormalizeLineEndings(change.Text)
			continue
		}

		lines := strings.Split(text, "\n")
		startOffset := rangeToByteOffset(lines, int(change.Range.Start.Line), int(change.Range.Start.Character), enc)
		endOffset := rangeToByteOffset(lines, int(change.Range.End.Line), int(change.Range.End.Character), enc)

		if startOffset <= len(text) && endOffset <= len(text) && startOffset <= endOffset {
			text = text[:startOffset] + docstate.NormalizeLineEndings(change.Text) + text[endOffset:]
		} else {
			if logger != nil {
				logger.Warn("incremental change has invalid range, using full-text fallback",
					slog.Int("start_offset", startOffset),
					slog.Int("end_offset", endOffset),
					slog.Int("text_len", len(text)),
				)
			}
			text = docstate.NormalizeLineEndings(change.Text)
		}
	}
	return text
}

// rangeToByteOffset converts an LSP position to a byte offset in the document.
func rangeToByteOffset(lines []string, line, char int, enc lsputil.PositionEncoding) int {
	offset := 0

	for i := 0; i < line && i < len(lines); i++ {
		offset += len(lines[i]) + 1
	}

	if line < len(lines) {
		lineContent := []byte(lines[line])
		var charOffset int
		switch enc {
		case lsputil.PositionEncodingUTF8:
			charOffset = min(char, len(lineContent))
		default:
			charOffset = lsputil.UTF16CharToByteOffset(lineContent, 0, char)
		}
		offset += charOffset
	}

	return offset
}

// AddRoot adds a workspace root.
func (w *Workspace) AddRoot(uri string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	path, err := lsputil.URIToPath(uri)
	if err != nil {
		w.logger.Warn("failed to parse workspace root URI",
			slog.String("uri", uri),
			slog.String("error", err.Error()),
		)
		return
	}

	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = filepath.Clean(resolved)
	}

	if slices.Contains(w.roots, canonicalPath) {
		w.logger.Debug("workspace root already exists", slog.String("path", canonicalPath))
		return
	}

	w.roots = append(w.roots, canonicalPath)
	w.logger.Debug("added workspace root", slog.String("path", canonicalPath))
}

// RemoveRoot removes a workspace root.
func (w *Workspace) RemoveRoot(uri string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	path, err := lsputil.URIToPath(uri)
	if err != nil {
		w.logger.Warn("failed to parse workspace root URI for removal",
			slog.String("uri", uri),
			slog.String("error", err.Error()),
		)
		return
	}

	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = filepath.Clean(resolved)
	}

	newRoots := w.roots[:0]
	removed := false
	for _, root := range w.roots {
		if root != canonicalPath {
			newRoots = append(newRoots, root)
		} else {
			removed = true
		}
	}
	w.roots = newRoots

	if removed {
		w.logger.Debug("removed workspace root", slog.String("path", canonicalPath))
	} else {
		w.logger.Debug("workspace root not found for removal", slog.String("path", canonicalPath))
	}
}

// SetPositionEncoding sets the position encoding to use.
func (w *Workspace) SetPositionEncoding(enc lsputil.PositionEncoding) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.posEncoding = enc
}

// PositionEncoding returns the negotiated position encoding.
func (w *Workspace) PositionEncoding() lsputil.PositionEncoding {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.posEncoding
}

// DocumentOpened handles a document being opened.
// Resolves symlinks to compute the canonical SourceID before storing.
func (w *Workspace) DocumentOpened(uri string, version int, text string) {
	path, err := lsputil.URIToPath(uri)
	if err != nil {
		w.logger.Warn("failed to parse document URI",
			slog.String("uri", uri),
			slog.String("error", err.Error()),
		)
		return
	}

	// Resolve symlinks to get canonical path matching the loader's behavior.
	// The loader uses makeCanonicalPath which resolves symlinks, so we need
	// to do the same here to ensure SourceID matches loader output.
	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = filepath.Clean(resolved)
	}

	sourceID, err := location.SourceIDFromAbsolutePath(canonicalPath)
	if err != nil {
		w.logger.Warn("failed to create source ID",
			slog.String("path", canonicalPath),
			slog.String("error", err.Error()),
		)
		return
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	w.docs.OpenDocument(uri, sourceID, version, text)

	// Invalidate canonical-to-URI cache (new document may map to a canonical path)
	w.mapper.invalidateCache()
}

// documentChanged handles a document content change.
func (w *Workspace) documentChanged(uri string, version int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.docs.ChangeDocument(uri, version, text, w.logger)
}

// documentClosed handles a document being closed.
func (w *Workspace) documentClosed(notify NotifyFunc, uri string) {
	w.mu.Lock()
	w.docs.RemoveDocument(uri)
	delete(w.snapshots, uri)

	w.mapper.invalidateCache()

	publishedFromEntry := w.mapper.clearEntryPublications(uri)

	urisToClear := make([]string, 0)
	for pubURI := range publishedFromEntry {
		if !w.mapper.uriStillPublishedByOthers(pubURI) {
			urisToClear = append(urisToClear, pubURI)
		}
	}

	w.deps.updateDependencies(uri, nil, w.logger)
	w.mu.Unlock()

	for _, pubURI := range urisToClear {
		w.publishDiagnostics(notify, pubURI, nil, nil)
		w.clearDiagHash(pubURI)
	}

	w.cancelPendingAnalysis(uri)
}

// ReanalyzeOpenDocuments triggers re-analysis of all open documents.
func (w *Workspace) ReanalyzeOpenDocuments(notify NotifyFunc) {
	w.mu.RLock()
	uris := w.docs.AllOpenURIs()
	w.mu.RUnlock()

	for _, uri := range uris {
		w.scheduleAnalysis(notify, uri)
	}
}

// scheduleAnalysis schedules a debounced analysis for the given document.
func (w *Workspace) scheduleAnalysis(notify NotifyFunc, uri string) {
	w.sched.schedule(notify, uri, w.analyzeAndPublish)
}

// analyzeAndPublish analyzes a document and publishes diagnostics.
func (w *Workspace) analyzeAndPublish(notify NotifyFunc, analyzeCtx context.Context, uri string) {
	w.mu.RLock()
	doc, ok := w.docs.Open[uri]
	if !ok {
		w.mu.RUnlock()
		return
	}

	overlays := w.docs.CollectOverlays()
	entryVersion := doc.Version
	w.mu.RUnlock()

	path, err := lsputil.URIToPath(uri)
	if err != nil {
		w.logger.Warn("failed to parse document URI for analysis",
			slog.String("uri", uri),
			slog.String("error", err.Error()),
		)
		return
	}

	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = filepath.Clean(resolved)
	}

	moduleRoot := w.FindModuleRoot(canonicalPath)

	snapshot, err := w.sched.analyzer.Analyze(analyzeCtx, canonicalPath, overlays, moduleRoot, w.PositionEncoding())

	if analyzeCtx.Err() != nil {
		w.logger.Debug("analysis cancelled", slog.String("uri", uri))
		return
	}

	w.mu.RLock()
	currentDoc := w.docs.Open[uri]
	isStale := currentDoc == nil || currentDoc.Version != entryVersion
	w.mu.RUnlock()

	if isStale {
		w.logger.Debug("skipping stale analysis results",
			slog.String("uri", uri),
			slog.Int("entry_version", entryVersion),
		)
		return
	}

	if err != nil {
		w.logger.Error("analysis failed",
			slog.String("uri", uri),
			slog.String("error", err.Error()),
		)
		if snapshot != nil {
			snapshot.EntryVersion = entryVersion
			w.mu.Lock()
			w.snapshots[uri] = snapshot
			w.mu.Unlock()
			w.UpdateDependencies(uri, snapshot.ImportedPaths)
			w.publishSnapshotDiagnostics(notify, uri, snapshot)
		}
		return
	}

	snapshot.EntryVersion = entryVersion

	w.mu.Lock()
	w.snapshots[uri] = snapshot
	w.mu.Unlock()

	w.UpdateDependencies(uri, snapshot.ImportedPaths)

	w.publishSnapshotDiagnostics(notify, uri, snapshot)
}

// FileChanged handles a watched file change notification.
func (w *Workspace) FileChanged(notify NotifyFunc, uri string, changeType protocol.UInteger) {
	canonicalURI := uri
	if path, err := lsputil.URIToPath(uri); err == nil {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = filepath.Clean(resolved)
		} else {
			path = filepath.Clean(path)
		}
		if sourceID, err := location.SourceIDFromAbsolutePath(path); err == nil {
			canonicalURI = lsputil.PathToURI(sourceID.String())
		}
	}

	w.mu.RLock()
	deps := w.deps.reverseDependents(canonicalURI)
	w.mu.RUnlock()

	for entryURI := range deps {
		w.scheduleAnalysis(notify, entryURI)
	}
}

// UpdateDependencies updates the dependency tracking for an entry file.
func (w *Workspace) UpdateDependencies(entryURI string, importedPaths []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deps.updateDependencies(entryURI, importedPaths, w.logger)
}

// cancelPendingAnalysis cancels any pending analysis for a URI.
func (w *Workspace) cancelPendingAnalysis(uri string) {
	w.sched.cancelPending(uri)
}

// Shutdown cancels all pending analysis operations.
func (w *Workspace) Shutdown() {
	w.sched.shutdown()
}

// FindModuleRoot finds the module root for a file path.
//
// This method acquires its own lock to safely read w.roots and w.config.
// Callers must NOT hold w.mu when calling this method to avoid deadlock.
func (w *Workspace) FindModuleRoot(path string) string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	if w.config.ModuleRoot != "" {
		return w.config.ModuleRoot
	}

	var nearest string
	for _, root := range w.roots {
		if (path == root || strings.HasPrefix(path, root+string(filepath.Separator))) && len(root) > len(nearest) {
			nearest = root
		}
	}
	if nearest != "" {
		return nearest
	}

	return filepath.Dir(path)
}

// LatestSnapshot returns the latest snapshot for a URI.
func (w *Workspace) LatestSnapshot(uri string) *analysis.Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.snapshots[uri]
}

// GetDocumentSnapshot returns an immutable snapshot of the document for a URI.
func (w *Workspace) GetDocumentSnapshot(uri string) *docstate.Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.docs.GetSnapshot(uri)
}

// RemapPathToURI maps a path to the client's document URI if the file is open.
func (w *Workspace) RemapPathToURI(input string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mapper.remapPathToURI(input, w.docs.Open)
}
