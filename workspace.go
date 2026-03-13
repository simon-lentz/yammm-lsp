package lsp

import (
	"context"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm/location"
	"github.com/simon-lentz/yammm/schema/load"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/markdown"
)

// PositionEncoding is an alias for lsputil.PositionEncoding within the lsp package.
type PositionEncoding = lsputil.PositionEncoding

const (
	// PositionEncodingUTF16 counts positions in UTF-16 code units (default).
	PositionEncodingUTF16 = lsputil.PositionEncodingUTF16

	// PositionEncodingUTF8 counts positions in UTF-8 bytes.
	PositionEncodingUTF8 = lsputil.PositionEncodingUTF8
)

// debounceDelay is the delay before triggering analysis after a change.
const debounceDelay = 150 * time.Millisecond

// debounceEntry tracks a pending analysis for a single document.
// Using a struct with pointer identity allows callbacks to safely clean up
// only their own entries, avoiding the race where a stale callback deletes
// a newer entry that was scheduled while analysis was running.
type debounceEntry struct {
	timer  *time.Timer
	cancel context.CancelFunc
}

// notifyFunc is a function that sends LSP notifications.
// This type decouples notification sending from transport details.
// This reduces coupling in closures
// (e.g., debounce timers) and makes explicit what capability is being captured.
type notifyFunc func(method string, params any)

// document represents an open text document.
// lineState holds cached per-line analysis results for completion context detection.
// This enables O(1) lookup for isInsideTypeBody instead of O(n) scanning from line 0.
type lineState struct {
	Version        int    // document version this state was computed for
	BraceDepth     []int  // BraceDepth[i] = nesting depth at END of line i
	InBlockComment []bool // InBlockComment[i] = true if line i ends inside a block comment
}

// document represents an open document in the workspace.
type document struct {
	URI       string
	SourceID  location.SourceID
	Version   int
	Text      string
	OpenOrder int // Order in which document was opened (for deterministic URI selection)

	// lineState caches per-line brace depth for completion context.
	// Lazily computed and invalidated when Version changes.
	lineState *lineState
}

// documentSnapshot is a point-in-time view of a document's state.
// Treat as immutable after creation — fields are value types or pointers
// for efficiency, but callers must not modify the underlying data.
type documentSnapshot struct {
	URI       string
	SourceID  location.SourceID
	Version   int
	Text      string
	lineState *lineState // Cached brace depth per line (may be nil)
}

// computeBraceDepths computes the brace nesting depth and block comment state
// at the end of each line. This is used for completion context detection (isInsideTypeBody).
// The function properly skips braces inside comments and string literals.
//
// Returns two parallel slices:
//   - depths[i] = brace nesting depth at end of line i
//   - inComment[i] = true if line i ends inside a block comment
func computeBraceDepths(text string) (depths []int, inComment []bool) {
	// Normalize line endings to LF for consistent processing.
	// Windows clients may send CRLF (\r\n), which would leave trailing \r
	// in each line after splitting on \n.
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	lines := strings.Split(text, "\n")
	depths = make([]int, len(lines))
	inComment = make([]bool, len(lines))

	var bs braceScanner
	for i, line := range lines {
		bs.scanLine(line, len(line))
		depths[i] = bs.depth
		inComment[i] = bs.inBlockComment
	}

	return depths, inComment
}

// Workspace manages the state of open documents and analysis results.
//
// Lock ordering: debounceMu must never be acquired while holding mu.
// Methods that need both must release mu before acquiring debounceMu.
// Methods with a "Locked" suffix expect the caller to already hold mu.
type Workspace struct {
	// mu protects: docs, snapshots, posEncoding, deps, mapper, roots.
	mu sync.RWMutex

	logger *slog.Logger
	config Config

	// Workspace roots (from workspaceFolders)
	roots []string

	// docs stores open document state for both .yammm and markdown files.
	docs docOverlay

	// Latest analysis snapshots keyed by entry URI
	snapshots map[string]*analysis.Snapshot

	// Position encoding negotiated with client
	posEncoding PositionEncoding

	// deps tracks forward and reverse import dependencies between entry files.
	deps depGraph

	// sched manages debounced analysis scheduling and background context.
	// Has its own debounceMu lock (must never be acquired while holding mu).
	sched analysisScheduler

	// mapper manages canonical path mapping and per-entry publication tracking.
	mapper uriMapper
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
			// Fallback to cleaned path when symlink resolution fails
			cfg.ModuleRoot = filepath.Clean(cfg.ModuleRoot)
		}
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())

	return &Workspace{
		logger:      logger.With(slog.String("component", "workspace")),
		config:      cfg,
		roots:       make([]string, 0),
		snapshots:   make(map[string]*analysis.Snapshot),
		posEncoding: PositionEncodingUTF16,
		docs: docOverlay{
			open:         make(map[string]*document),
			markdownDocs: make(map[string]*markdownDocument),
		},
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
			publishedByEntry: make(map[string]map[string]struct{}),
		},
	}
}

// BackgroundContext returns the workspace's background context for analysis.
// This context is cancelled during Shutdown.
func (w *Workspace) BackgroundContext() context.Context {
	return w.sched.backgroundContext()
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

	// Resolve symlinks to get canonical path matching DocumentOpened behavior.
	// This ensures roots and open documents live in the same namespace,
	// making findModuleRoot reliable under symlinks (e.g., /var → /private/var on macOS).
	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = filepath.Clean(resolved)
	}

	// Check for duplicates before appending to prevent unbounded root growth
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

	// Resolve symlinks to match how roots are stored
	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = filepath.Clean(resolved)
	}

	// Remove all matching roots (in case of prior duplicates)
	newRoots := w.roots[:0] // reuse backing array
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
func (w *Workspace) SetPositionEncoding(enc PositionEncoding) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.posEncoding = enc
}

// PositionEncoding returns the negotiated position encoding.
func (w *Workspace) PositionEncoding() PositionEncoding {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.posEncoding
}

// DocumentOpened handles a document being opened.
func (w *Workspace) DocumentOpened(uri string, version int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.docs.openDocument(uri, version, text, w.logger)

	// Invalidate canonical-to-URI cache (new document may map to a canonical path)
	w.mapper.invalidateCache()
}

// DocumentChanged handles a document content change.
// Ignores stale updates where version <= current version (unless version is 0/unknown).
func (w *Workspace) DocumentChanged(uri string, version int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.docs.changeDocument(uri, version, text, w.logger)
}

// DocumentClosed handles a document being closed.
// notify is the notification function for clearing diagnostics; if nil,
// diagnostics are not cleared (useful in tests).
func (w *Workspace) DocumentClosed(notify notifyFunc, uri string) {
	w.mu.Lock()
	w.docs.removeDocument(uri)
	delete(w.snapshots, uri)

	// Invalidate canonical-to-URI cache (document removal changes mapping)
	w.mapper.invalidateCache()

	// Get URIs published from this entry's analysis and remove tracking
	publishedFromEntry := w.mapper.clearEntryPublications(uri)

	// Determine which URIs to clear: only those not published by any other entry
	urisToClear := make([]string, 0)
	for pubURI := range publishedFromEntry {
		if !w.mapper.uriStillPublishedByOthers(pubURI) {
			urisToClear = append(urisToClear, pubURI)
		}
	}

	// Clean up dependency tracking while lock is held
	w.deps.updateDependencies(uri, nil, w.logger)
	w.mu.Unlock()

	// Clear diagnostics for URIs only published by this entry (no lock — may block on I/O)
	for _, pubURI := range urisToClear {
		w.publishDiagnostics(notify, pubURI, nil, nil)
	}

	// Cancel any pending analysis (acquires debounceMu — safe, mu not held)
	w.cancelPendingAnalysis(uri)
}

// ReanalyzeOpenDocuments triggers re-analysis of all open documents.
// This is called when workspace folders change, as module root selection
// may have changed for existing documents.
func (w *Workspace) ReanalyzeOpenDocuments(notify notifyFunc) {
	w.mu.RLock()
	uris := w.docs.allOpenURIs()
	w.mu.RUnlock()

	for _, uri := range uris {
		w.ScheduleAnalysis(notify, uri)
	}
}

// ScheduleAnalysis schedules a debounced analysis for the given document.
func (w *Workspace) ScheduleAnalysis(notify notifyFunc, uri string) {
	w.sched.schedule(notify, uri, w.AnalyzeAndPublish)
}

// AnalyzeAndPublish analyzes a document and publishes diagnostics.
// analyzeCtx is a cancellable context; if cancelled, analysis aborts early.
// notify is the notification function for publishing diagnostics; if nil,
// diagnostics are computed but not published (useful in tests).
func (w *Workspace) AnalyzeAndPublish(notify notifyFunc, analyzeCtx context.Context, uri string) {
	w.mu.RLock()
	doc, ok := w.docs.open[uri]
	if !ok {
		w.mu.RUnlock()
		return
	}

	// Collect overlay content from all open documents.
	overlays := w.docs.collectOverlays()

	// Capture version before releasing lock
	entryVersion := doc.Version
	w.mu.RUnlock()

	// Find module root for this document (after releasing lock to avoid deadlock)
	path, err := lsputil.URIToPath(uri)
	if err != nil {
		w.logger.Warn("failed to parse document URI for analysis",
			slog.String("uri", uri),
			slog.String("error", err.Error()),
		)
		return
	}

	// Canonicalize path to match workspace roots (which are now canonical).
	// This ensures findModuleRoot comparisons work under symlinks.
	canonicalPath := path
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		canonicalPath = filepath.Clean(resolved)
	}

	moduleRoot := w.findModuleRoot(canonicalPath)

	// Perform analysis with cancellable context.
	// Use canonical path to ensure consistent SourceID creation.
	snapshot, err := w.sched.analyzer.Analyze(analyzeCtx, canonicalPath, overlays, moduleRoot, w.PositionEncoding())

	// Check if context was cancelled - abort silently
	if analyzeCtx.Err() != nil {
		w.logger.Debug("analysis cancelled", slog.String("uri", uri))
		return
	}

	// Version gate: skip publishing if document has been modified during analysis.
	// This prevents stale diagnostics from overwriting fresher results.
	w.mu.RLock()
	currentDoc := w.docs.open[uri]
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
		// Still publish diagnostics from partial snapshot if available.
		// This ensures users see parse errors instead of silent failure.
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

	// Store snapshot
	w.mu.Lock()
	w.snapshots[uri] = snapshot
	w.mu.Unlock()

	// Update dependency tracking for file watching
	w.UpdateDependencies(uri, snapshot.ImportedPaths)

	// Publish diagnostics
	w.publishSnapshotDiagnostics(notify, uri, snapshot)
}

// publishSnapshotDiagnostics publishes diagnostics from a snapshot.
// This method computes the publication plan under lock, then releases the lock
// before sending notifications to avoid potential deadlock if Notify blocks.
// If notify is nil (e.g., in tests without a transport), this is a no-op.
//
// URI remapping: Diagnostics from the loader use canonical (symlink-resolved)
// paths, but clients identify documents by the URI they used to open them
// (which may be a symlink path). This method remaps diagnostic URIs to match
// the client's open document URIs, ensuring diagnostics appear in the editor.
//
// entryURI identifies the entry file being analyzed, used for per-entry
// diagnostic tracking to avoid cross-entry interference in multi-file sessions.
func (w *Workspace) publishSnapshotDiagnostics(notify notifyFunc, entryURI string, snapshot *analysis.Snapshot) {
	if notify == nil {
		return // No-op in test context without transport
	}

	// Phase 1: Compute publication plan under lock
	diagsByURI, staleURIs, versionsByURI := w.computePublicationPlan(entryURI, snapshot)

	// Phase 2: Emit notifications outside the lock
	// Clear stale diagnostics
	for _, uri := range staleURIs {
		notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
			URI:         uri,
			Version:     versionsByURI[uri],
			Diagnostics: []protocol.Diagnostic{},
		})
	}

	// Publish current diagnostics
	for uri, diags := range diagsByURI {
		notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
			URI:         uri,
			Version:     versionsByURI[uri],
			Diagnostics: diags,
		})
	}
}

// computePublicationPlan computes the publication plan for diagnostics.
// It returns the diagnostics grouped by URI (with symlink remapping applied)
// and the list of stale URIs that need to be cleared.
//
// This method acquires the workspace lock internally.
func (w *Workspace) computePublicationPlan(entryURI string, snapshot *analysis.Snapshot) (diagsByURI map[string][]protocol.Diagnostic, staleURIs []string, versionsByURI map[string]*protocol.Integer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mapper.computePublicationPlan(entryURI, snapshot, w.docs.open)
}

// publishDiagnostics publishes diagnostics for a URI.
// This method does not require the workspace lock as it only sends a notification
// and does not access any workspace state. Calling notify under lock risks
// deadlock if the notification blocks on I/O.
// If notify is nil (e.g., in tests without a transport), this is a no-op.
// version may be nil (e.g., when clearing diagnostics on document close).
func (w *Workspace) publishDiagnostics(notify notifyFunc, uri string, version *protocol.Integer, diagnostics []protocol.Diagnostic) {
	if notify == nil {
		return // No-op in test context without transport
	}

	if diagnostics == nil {
		diagnostics = []protocol.Diagnostic{}
	}

	notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         uri,
		Version:     version,
		Diagnostics: diagnostics,
	})
}

// FileChanged handles a watched file change notification.
// It triggers reanalysis of any open documents that import the changed file.
//
// The incoming URI is canonicalized before lookup to handle symlink and path
// variations between what VS Code reports and what we store internally.
func (w *Workspace) FileChanged(notify notifyFunc, uri string, changeType protocol.UInteger) {
	// Canonicalize URI to match how we store reverseDeps keys.
	// VS Code may report symlinked or case-different paths.
	// Resolve symlinks to match the loader's canonicalization (makeCanonicalPath).
	canonicalURI := uri
	if path, err := lsputil.URIToPath(uri); err == nil {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = filepath.Clean(resolved)
		} else {
			// Symlink resolution failed (deleted file, broken symlink, permissions).
			// Fall back to filesystem-independent canonicalization via Clean.
			// This ensures we still attempt lookup even when the file is gone.
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
		w.ScheduleAnalysis(notify, entryURI)
	}
}

// UpdateDependencies updates the dependency tracking for an entry file.
// importedPaths contains the absolute paths of all imported files in the closure.
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
// This should be called during server shutdown to ensure clean termination.
func (w *Workspace) Shutdown() {
	w.sched.shutdown()
}

// findModuleRoot finds the module root for a file path.
//
// For multi-root workspaces, this selects the longest (deepest) matching
// workspace folder. For example, given roots ["/project", "/project/submodule"],
// a file at "/project/submodule/foo.yammm" will use "/project/submodule" as
// its module root.
//
// This method acquires its own lock to safely read w.roots and w.config.
// Callers must NOT hold w.mu when calling this method to avoid deadlock.
func (w *Workspace) findModuleRoot(path string) string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	// If configured module root, use it
	if w.config.ModuleRoot != "" {
		return w.config.ModuleRoot
	}

	// Find the nearest (deepest) workspace folder ancestor.
	// Select the longest matching prefix to handle nested workspace folders.
	// Use path-boundary check to avoid misclassifying paths like /project2/foo
	// as being under /project (strings.HasPrefix would incorrectly match).
	// Use filepath.Separator for cross-platform compatibility (/ on Unix, \ on Windows).
	var nearest string
	for _, root := range w.roots {
		if (path == root || strings.HasPrefix(path, root+string(filepath.Separator))) && len(root) > len(nearest) {
			nearest = root
		}
	}
	if nearest != "" {
		return nearest
	}

	// Fall back to the file's directory
	return filepath.Dir(path)
}

// LatestSnapshot returns the latest snapshot for a URI.
func (w *Workspace) LatestSnapshot(uri string) *analysis.Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.snapshots[uri]
}

// GetDocumentSnapshot returns an immutable snapshot of the document for a URI.
// The snapshot contains a copy of the document state at the time of the call,
// allowing safe access outside of locks without racing with DocumentChanged.
func (w *Workspace) GetDocumentSnapshot(uri string) *documentSnapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.docs.getSnapshot(uri)
}

// RemapPathToURI maps a path to the client's document URI if the file is open.
// This ensures definitions point to the same URI the client used to open the
// document (important for symlink scenarios where the client opens a symlink
// but the loader resolves to the canonical path).
func (w *Workspace) RemapPathToURI(input string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.mapper.remapPathToURI(input, w.docs.open)
}

// MarkdownDocumentOpened creates a markdownDocument with normalized text and version.
// Block extraction is deferred to AnalyzeMarkdownAndPublish.
func (w *Workspace) MarkdownDocumentOpened(uri string, version int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.docs.openMarkdownDocument(uri, version, text)
}

// MarkdownDocumentChanged updates text and version for a markdown document.
// Ignores stale updates (version <= current unless either is 0).
// Does NOT re-extract blocks — that is done atomically by AnalyzeMarkdownAndPublish.
func (w *Workspace) MarkdownDocumentChanged(uri string, version int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.docs.changeMarkdownDocument(uri, version, text, w.logger)
}

// MarkdownDocumentClosed removes a markdown document and clears its diagnostics.
func (w *Workspace) MarkdownDocumentClosed(notify notifyFunc, uri string) {
	w.mu.Lock()
	w.docs.removeMarkdownDocument(uri)

	hadPublished := w.mapper.publishedByEntry[uri] != nil
	delete(w.mapper.publishedByEntry, uri)
	w.mu.Unlock()

	if hadPublished {
		w.publishDiagnostics(notify, uri, nil, nil)
	}

	w.cancelPendingAnalysis(uri)
}

// GetMarkdownDocumentSnapshot returns an immutable snapshot of a markdown document.
func (w *Workspace) GetMarkdownDocumentSnapshot(uri string) *markdownDocumentSnapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.docs.getMarkdownSnapshot(uri)
}

// GetMarkdownCurrentText returns the current text of a markdown document.
func (w *Workspace) GetMarkdownCurrentText(uri string) (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.docs.getMarkdownCurrentText(uri)
}

// ScheduleMarkdownAnalysis schedules a debounced analysis for a markdown document.
func (w *Workspace) ScheduleMarkdownAnalysis(notify notifyFunc, uri string) {
	w.sched.schedule(notify, uri, w.AnalyzeMarkdownAndPublish)
}

// AnalyzeMarkdownAndPublish analyzes a markdown document's code blocks and publishes diagnostics.
func (w *Workspace) AnalyzeMarkdownAndPublish(notify notifyFunc, analyzeCtx context.Context, uri string) {
	// Read text and version under lock
	w.mu.RLock()
	md := w.docs.markdownDocs[uri]
	if md == nil {
		w.mu.RUnlock()
		return
	}
	text := md.Text
	entryVersion := md.Version
	w.mu.RUnlock()

	// Extract blocks
	blocks := markdown.ExtractCodeBlocks(text)

	// Assign virtual SourceIDs
	path, err := lsputil.URIToPath(uri)
	if err != nil {
		w.logger.Warn("failed to parse markdown URI", slog.String("uri", uri), slog.String("error", err.Error()))
		return
	}

	validBlocks := make([]markdown.CodeBlock, 0, len(blocks))
	for i, block := range blocks {
		id, err := markdown.VirtualSourceID(path, i)
		if err != nil {
			w.logger.Warn("failed to create virtual source ID",
				slog.String("uri", uri), slog.Int("block", i), slog.String("error", err.Error()))
			continue
		}
		block.SourceID = id
		validBlocks = append(validBlocks, block)
	}

	// Prepend synthetic schema declaration for snippet blocks that lack one.
	// This allows documentation snippets (e.g., type definitions without a schema
	// header) to get full LSP features without spurious E_SYNTAX errors.
	const snippetPrefix = "schema \"_snippet\"\n"
	for i := range validBlocks {
		if !markdown.HasSchemaDeclaration(validBlocks[i].Content) {
			validBlocks[i].Content = snippetPrefix + validBlocks[i].Content
			validBlocks[i].PrefixLines = 1
		}
	}

	// Analyze each block
	snapshots := make([]*analysis.Snapshot, len(validBlocks))
	for i, block := range validBlocks {
		virtualPath := block.SourceID.String()
		overlays := map[string][]byte{
			virtualPath: []byte(block.Content),
		}
		snapshot, err := w.sched.analyzer.Analyze(analyzeCtx, virtualPath, overlays, "", w.PositionEncoding(), load.WithDisallowImports())
		if err != nil {
			w.logger.Warn("markdown block analysis failed",
				slog.String("uri", uri), slog.Int("block", i), slog.String("error", err.Error()))
		}
		snapshots[i] = snapshot
	}

	// Post-analysis cancellation check
	if analyzeCtx.Err() != nil {
		w.logger.Debug("markdown analysis cancelled", slog.String("uri", uri))
		return
	}

	// Version-gate and store results atomically
	w.mu.Lock()
	md = w.docs.markdownDocs[uri]
	if md == nil || md.Version != entryVersion {
		w.mu.Unlock()
		w.logger.Debug("skipping stale markdown analysis results", slog.String("uri", uri))
		return
	}
	md.Blocks = validBlocks
	md.Snapshots = snapshots

	snap := &markdownDocumentSnapshot{
		URI:       md.URI,
		Version:   md.Version,
		Blocks:    make([]markdown.CodeBlock, len(validBlocks)),
		Snapshots: make([]*analysis.Snapshot, len(snapshots)),
	}
	copy(snap.Blocks, validBlocks)
	copy(snap.Snapshots, snapshots)
	w.mu.Unlock()

	// Publish diagnostics (no lock held)
	w.publishMarkdownDiagnostics(notify, snap)
}

// publishMarkdownDiagnostics collects diagnostics from all block snapshots,
// remaps positions from block-local to markdown coordinates, and publishes.
func (w *Workspace) publishMarkdownDiagnostics(notify notifyFunc, snap *markdownDocumentSnapshot) {
	if notify == nil {
		return
	}

	var allDiagnostics []protocol.Diagnostic

	for i, snapshot := range snap.Snapshots {
		if snapshot == nil {
			continue
		}

		for _, uriDiag := range snapshot.LSPDiagnostics {
			diag := uriDiag.Diagnostic

			// Skip diagnostics that reference synthetic prefix content
			if int(diag.Range.Start.Line) < snap.Blocks[i].PrefixLines {
				continue
			}

			// Downgrade E_IMPORT_NOT_ALLOWED to Hint in markdown blocks
			if diag.Code != nil {
				if codeVal, ok := diag.Code.Value.(string); ok && codeVal == "E_IMPORT_NOT_ALLOWED" {
					hint := protocol.DiagnosticSeverityHint
					diag.Severity = &hint
				}
			}

			// Convert primary range from block-local to markdown coordinates
			startLine, startChar := snap.BlockPositionToMarkdown(i,
				int(diag.Range.Start.Line), int(diag.Range.Start.Character))
			endLine, endChar := snap.BlockPositionToMarkdown(i,
				int(diag.Range.End.Line), int(diag.Range.End.Character))

			diag.Range = protocol.Range{
				Start: protocol.Position{
					Line:      analysis.ToUInteger(startLine),
					Character: analysis.ToUInteger(startChar),
				},
				End: protocol.Position{
					Line:      analysis.ToUInteger(endLine),
					Character: analysis.ToUInteger(endChar),
				},
			}

			// Remap RelatedInformation URIs and ranges
			if len(diag.RelatedInformation) > 0 {
				block := snap.Blocks[i]
				expectedURI := lsputil.PathToURI(block.SourceID.String())
				var remapped []protocol.DiagnosticRelatedInformation
				for _, rel := range diag.RelatedInformation {
					if rel.Location.URI != expectedURI {
						w.logger.Warn("related info URI does not match expected block SourceID; skipping remap",
							"expected", expectedURI, "got", rel.Location.URI)
						remapped = append(remapped, rel)
						continue
					}

					relStartLine, relStartChar := snap.BlockPositionToMarkdown(i,
						int(rel.Location.Range.Start.Line), int(rel.Location.Range.Start.Character))
					relEndLine, relEndChar := snap.BlockPositionToMarkdown(i,
						int(rel.Location.Range.End.Line), int(rel.Location.Range.End.Character))

					remapped = append(remapped, protocol.DiagnosticRelatedInformation{
						Location: protocol.Location{
							URI: snap.URI,
							Range: protocol.Range{
								Start: protocol.Position{
									Line:      analysis.ToUInteger(relStartLine),
									Character: analysis.ToUInteger(relStartChar),
								},
								End: protocol.Position{
									Line:      analysis.ToUInteger(relEndLine),
									Character: analysis.ToUInteger(relEndChar),
								},
							},
						},
						Message: rel.Message,
					})
				}
				diag.RelatedInformation = remapped
			}

			allDiagnostics = append(allDiagnostics, diag)
		}
	}

	// Update publishedByEntry tracking
	w.mu.Lock()
	w.mapper.publishedByEntry[snap.URI] = map[string]struct{}{snap.URI: {}}
	w.mu.Unlock()

	if allDiagnostics == nil {
		allDiagnostics = []protocol.Diagnostic{}
	}
	v := protocol.Integer(snap.Version) //nolint:gosec // LSP version fits int32
	notify(protocol.ServerTextDocumentPublishDiagnostics, protocol.PublishDiagnosticsParams{
		URI:         snap.URI,
		Version:     &v,
		Diagnostics: allDiagnostics,
	})
}
