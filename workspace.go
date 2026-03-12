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

// documentSnapshot is an immutable view of a document at a point in time.
// Use this when you need to access document state outside of locks to avoid
// data races with concurrent DocumentChanged calls.
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
	// mu protects: open, markdownDocs, openCounter, canonicalToURI, snapshots,
	// posEncoding, importsByEntry, reverseDeps, publishedByEntry, roots.
	mu sync.RWMutex

	logger *slog.Logger
	config Config

	// Workspace roots (from workspaceFolders)
	roots []string

	// Open documents keyed by URI
	open map[string]*document

	// Open markdown documents keyed by URI
	markdownDocs map[string]*markdownDocument

	// Counter for deterministic document ordering (symlink disambiguation)
	openCounter int

	// Cached mapping from canonical paths to client URIs (for symlink resolution).
	//
	// Path normalization invariants:
	//   - Keys use forward-slash normalized paths matching SourceID.String() format
	//   - This enables cross-platform consistency (Windows paths converted to forward slashes)
	//   - The location.CanonicalPath type guarantees: absolute, clean, NFC-normalized, forward slashes
	//   - findModuleRoot operates on OS-native paths separately (uses filepath.Separator)
	//
	// Nil when invalidated; lazily rebuilt on first access after mutation.
	// Invalidated by DocumentOpened/DocumentClosed.
	canonicalToURI map[string]string

	// Latest analysis snapshots keyed by entry URI
	snapshots map[string]*analysis.Snapshot

	// Position encoding negotiated with client
	posEncoding PositionEncoding

	// Forward dependencies: entry URI -> set of imported URIs
	importsByEntry map[string]map[string]struct{}

	// Reverse dependencies: imported URI -> set of entry URIs that import it
	reverseDeps map[string]map[string]struct{}

	// debounceMu protects: debounces.
	// Must not be acquired while holding mu (see lock ordering above).
	debounceMu sync.Mutex

	// Debounce entries for analysis, keyed by URI.
	// Each entry tracks a pending analysis timer and its cancellation function.
	// Using a single map of entry pointers enables safe cleanup via pointer identity.
	debounces map[string]*debounceEntry

	// Previously published diagnostics URIs, keyed by entry URI.
	// This per-entry tracking ensures that analyzing one entry doesn't
	// clear diagnostics published by a different entry file.
	publishedByEntry map[string]map[string]struct{}

	// bgCtx is the workspace-lifetime context for analysis goroutines.
	// Cancelled during Shutdown to abort in-flight analysis.
	bgCtx    context.Context //nolint:containedctx // workspace-lifetime context, not request-scoped
	bgCancel context.CancelFunc

	// Analyzer for schema loading
	analyzer *analysis.Analyzer
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
		logger:           logger.With(slog.String("component", "workspace")),
		config:           cfg,
		bgCtx:            bgCtx,
		bgCancel:         bgCancel,
		roots:            make([]string, 0),
		open:             make(map[string]*document),
		markdownDocs:     make(map[string]*markdownDocument),
		snapshots:        make(map[string]*analysis.Snapshot),
		posEncoding:      PositionEncodingUTF16,
		importsByEntry:   make(map[string]map[string]struct{}),
		reverseDeps:      make(map[string]map[string]struct{}),
		debounces:        make(map[string]*debounceEntry),
		publishedByEntry: make(map[string]map[string]struct{}),
		analyzer:         analysis.NewAnalyzer(logger), // Pass base logger; analyzer adds its own component
	}
}

// BackgroundContext returns the workspace's background context for analysis.
// This context is cancelled during Shutdown.
func (w *Workspace) BackgroundContext() context.Context {
	return w.bgCtx
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
	// Note: EvalSymlinks also cleans the path, but we call Clean explicitly
	// to make the invariant visible (canonical paths are always clean).
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

	// Normalize line endings (CRLF/CR → LF) for consistent offset calculations.
	// Windows clients may send CRLF, which would cause mismatches with
	// line-based operations if not normalized here at the storage layer.
	text = normalizeLineEndings(text)

	// Eagerly compute lineState on the write path to avoid lock juggling in
	// GetDocumentSnapshot. For typical yammm files (<1000 lines) this is
	// sub-millisecond and dominated by the 150ms debounce delay.
	depths, inComment := computeBraceDepths(text)

	w.openCounter++
	w.open[uri] = &document{
		URI:       uri,
		SourceID:  sourceID,
		Version:   version,
		Text:      text,
		OpenOrder: w.openCounter,
		lineState: &lineState{
			Version:        version,
			BraceDepth:     depths,
			InBlockComment: inComment,
		},
	}

	// Invalidate canonical-to-URI cache (new document may map to a canonical path)
	w.canonicalToURI = nil
}

// DocumentChanged handles a document content change.
// Ignores stale updates where version <= current version (unless version is 0/unknown).
func (w *Workspace) DocumentChanged(uri string, version int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if doc, ok := w.open[uri]; ok {
		// Ignore stale updates (version <= current) unless version is 0 (unknown).
		// This prevents out-of-order updates from overwriting newer content.
		if version != 0 && doc.Version != 0 && version <= doc.Version {
			w.logger.Debug("ignoring stale document change",
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
}

// DocumentClosed handles a document being closed.
// notify is the notification function for clearing diagnostics; if nil,
// diagnostics are not cleared (useful in tests).
func (w *Workspace) DocumentClosed(notify notifyFunc, uri string) {
	w.mu.Lock()
	delete(w.open, uri)
	delete(w.snapshots, uri)

	// Invalidate canonical-to-URI cache (document removal changes mapping)
	w.canonicalToURI = nil

	// Get URIs published from this entry's analysis and remove tracking
	publishedFromEntry := w.publishedByEntry[uri]
	delete(w.publishedByEntry, uri)

	// Determine which URIs to clear: only those not published by any other entry
	urisToClear := make([]string, 0)
	for pubURI := range publishedFromEntry {
		stillPublished := false
		for _, otherPubs := range w.publishedByEntry {
			if _, ok := otherPubs[pubURI]; ok {
				stillPublished = true
				break
			}
		}
		if !stillPublished {
			urisToClear = append(urisToClear, pubURI)
		}
	}
	w.mu.Unlock()

	// Clear diagnostics for URIs only published by this entry
	for _, pubURI := range urisToClear {
		w.publishDiagnostics(notify, pubURI, nil, nil)
	}

	// Cancel any pending analysis
	w.cancelPendingAnalysis(uri)

	// Clean up dependency tracking for this entry
	w.UpdateDependencies(uri, nil)
}

// ReanalyzeOpenDocuments triggers re-analysis of all open documents.
// This is called when workspace folders change, as module root selection
// may have changed for existing documents.
func (w *Workspace) ReanalyzeOpenDocuments(notify notifyFunc) {
	w.mu.RLock()
	uris := make([]string, 0, len(w.open))
	for uri := range w.open {
		uris = append(uris, uri)
	}
	w.mu.RUnlock()

	for _, uri := range uris {
		w.ScheduleAnalysis(notify, uri)
	}
}

// ScheduleAnalysis schedules a debounced analysis for the given document.
func (w *Workspace) ScheduleAnalysis(notify notifyFunc, uri string) {
	w.debounceMu.Lock()
	defer w.debounceMu.Unlock()

	// Cancel existing entry
	if existing, ok := w.debounces[uri]; ok {
		existing.timer.Stop()
		existing.cancel()
	}

	// Create new cancellation context derived from workspace background context.
	// This ensures in-flight analysis is cancelled when the workspace shuts down.
	analyzeCtx, cancel := context.WithCancel(w.bgCtx)

	// Create entry before timer so we can capture its pointer for identity check.
	// This pointer identity is used in the callback to ensure we only clean up
	// our own entry, not a newer one that may have been scheduled while we were
	// running analysis.
	entry := &debounceEntry{cancel: cancel}

	// Schedule new analysis, capturing entry pointer for identity check
	entry.timer = time.AfterFunc(debounceDelay, func() {
		select {
		case <-analyzeCtx.Done():
			return
		default:
			w.AnalyzeAndPublish(notify, analyzeCtx, uri)
			// Clean up only if this is still our entry.
			// If a new ScheduleAnalysis call occurred while we were running,
			// a new entry will be in the map and we must not delete it.
			w.debounceMu.Lock()
			if w.debounces[uri] == entry {
				delete(w.debounces, uri)
			}
			w.debounceMu.Unlock()
		}
	})

	w.debounces[uri] = entry
}

// AnalyzeAndPublish analyzes a document and publishes diagnostics.
// analyzeCtx is a cancellable context; if cancelled, analysis aborts early.
// notify is the notification function for publishing diagnostics; if nil,
// diagnostics are computed but not published (useful in tests).
func (w *Workspace) AnalyzeAndPublish(notify notifyFunc, analyzeCtx context.Context, uri string) {
	w.mu.RLock()
	doc, ok := w.open[uri]
	if !ok {
		w.mu.RUnlock()
		return
	}

	// Collect overlay content from all open documents.
	// Use canonical SourceID as key to ensure symlinks and path variations
	// map to the same entry that the loader will use.
	overlays := make(map[string][]byte)
	for _, d := range w.open {
		overlays[d.SourceID.String()] = []byte(d.Text)
	}

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
	snapshot, err := w.analyzer.Analyze(analyzeCtx, canonicalPath, overlays, moduleRoot, w.PositionEncoding())

	// Check if context was cancelled - abort silently
	if analyzeCtx.Err() != nil {
		w.logger.Debug("analysis cancelled", slog.String("uri", uri))
		return
	}

	// Version gate: skip publishing if document has been modified during analysis.
	// This prevents stale diagnostics from overwriting fresher results.
	w.mu.RLock()
	currentDoc := w.open[uri]
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
// entryURI identifies the entry file being analyzed. Per-entry tracking ensures
// that analyzing one entry doesn't clear diagnostics from another entry.
//
// This method acquires the workspace lock internally.
func (w *Workspace) computePublicationPlan(entryURI string, snapshot *analysis.Snapshot) (diagsByURI map[string][]protocol.Diagnostic, staleURIs []string, versionsByURI map[string]*protocol.Integer) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Build map from canonical paths to open document URIs.
	// The loader resolves symlinks, so diagnostics have canonical paths.
	// We need to map them back to the URIs the client used to open documents.
	// Reuse cached map if available (invalidated by DocumentOpened/DocumentClosed).
	if w.canonicalToURI == nil {
		w.canonicalToURI = w.buildCanonicalToURIMapLocked()
	}
	canonicalToDocURI := w.canonicalToURI

	// Collect current URIs with diagnostics
	currentURIs := make(map[string]struct{})

	// Group diagnostics by URI, remapping to open document URIs where applicable
	diagsByURI = make(map[string][]protocol.Diagnostic)

	for _, lspDiag := range snapshot.LSPDiagnostics {
		// Remap the diagnostic URI to the open document URI if available
		pubURI := w.remapToOpenDocURILocked(lspDiag.URI, canonicalToDocURI)

		// Also remap URIs in RelatedInformation
		diag := lspDiag.Diagnostic
		if len(diag.RelatedInformation) > 0 {
			remapped := make([]protocol.DiagnosticRelatedInformation, len(diag.RelatedInformation))
			for i, rel := range diag.RelatedInformation {
				remapped[i] = protocol.DiagnosticRelatedInformation{
					Location: protocol.Location{
						URI:   w.remapToOpenDocURILocked(rel.Location.URI, canonicalToDocURI),
						Range: rel.Location.Range,
					},
					Message: rel.Message,
				}
			}
			diag.RelatedInformation = remapped
		}

		diagsByURI[pubURI] = append(diagsByURI[pubURI], diag)
		currentURIs[pubURI] = struct{}{}
	}

	// Find stale URIs (published by THIS entry before but not now)
	// Per-entry tracking: only clear URIs from this entry's previous publication
	previousURIs := w.publishedByEntry[entryURI]
	staleURIs = make([]string, 0)
	for uri := range previousURIs {
		if _, ok := currentURIs[uri]; !ok {
			staleURIs = append(staleURIs, uri)
		}
	}

	// Update published URIs tracking for this entry only
	w.publishedByEntry[entryURI] = currentURIs

	// Capture document versions while lock is held.
	// LSP document versions fit in int32 per spec.
	versionsByURI = make(map[string]*protocol.Integer)
	for uri := range diagsByURI {
		if doc, ok := w.open[uri]; ok {
			v := protocol.Integer(doc.Version) //nolint:gosec // LSP version fits int32
			versionsByURI[uri] = &v
		}
	}
	for _, uri := range staleURIs {
		if doc, ok := w.open[uri]; ok {
			v := protocol.Integer(doc.Version) //nolint:gosec // LSP version fits int32
			versionsByURI[uri] = &v
		}
	}

	return diagsByURI, staleURIs, versionsByURI
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
	// If this file is a dependency of open documents, reanalyze them
	deps := make(map[string]struct{})
	for k := range w.reverseDeps[canonicalURI] {
		deps[k] = struct{}{}
	}
	w.mu.RUnlock()

	for entryURI := range deps {
		w.ScheduleAnalysis(notify, entryURI)
	}
}

// UpdateDependencies updates the dependency tracking for an entry file.
// importedPaths contains the absolute paths of all imported files in the closure.
// This uses an atomic update algorithm: remove old edges, add new edges, store new set.
func (w *Workspace) UpdateDependencies(entryURI string, importedPaths []string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Convert imported paths to URIs
	importedURIs := make(map[string]struct{}, len(importedPaths))
	for _, path := range importedPaths {
		importedURIs[lsputil.PathToURI(path)] = struct{}{}
	}

	// Step 1: Remove old edges from reverse deps
	if oldImports, ok := w.importsByEntry[entryURI]; ok {
		for importURI := range oldImports {
			if entries, exists := w.reverseDeps[importURI]; exists {
				delete(entries, entryURI)
				// Clean up empty reverse dep entries
				if len(entries) == 0 {
					delete(w.reverseDeps, importURI)
				}
			}
		}
	}

	// Step 2: Add new edges to reverse deps
	for importURI := range importedURIs {
		if w.reverseDeps[importURI] == nil {
			w.reverseDeps[importURI] = make(map[string]struct{})
		}
		w.reverseDeps[importURI][entryURI] = struct{}{}
	}

	// Step 3: Store new import set (or delete if empty)
	if len(importedURIs) == 0 {
		delete(w.importsByEntry, entryURI)
	} else {
		w.importsByEntry[entryURI] = importedURIs
	}

	w.logger.Debug("updated dependencies",
		slog.String("entry", entryURI),
		slog.Int("imports", len(importedURIs)),
	)
}

// cancelPendingAnalysis cancels any pending analysis for a URI.
func (w *Workspace) cancelPendingAnalysis(uri string) {
	w.debounceMu.Lock()
	defer w.debounceMu.Unlock()

	if entry, ok := w.debounces[uri]; ok {
		entry.timer.Stop()
		entry.cancel()
		delete(w.debounces, uri)
	}
}

// Shutdown cancels all pending analysis operations.
// This should be called during server shutdown to ensure clean termination.
func (w *Workspace) Shutdown() {
	// Cancel the background context first to abort any in-flight analysis
	// goroutines that may be running outside the debounce map.
	w.bgCancel()

	w.debounceMu.Lock()
	defer w.debounceMu.Unlock()

	for uri, entry := range w.debounces {
		entry.timer.Stop()
		entry.cancel()
		delete(w.debounces, uri)
	}
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

	doc := w.open[uri]
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

// RemapPathToURI maps a path to the client's document URI if the file is open.
// This ensures definitions point to the same URI the client used to open the
// document (important for symlink scenarios where the client opens a symlink
// but the loader resolves to the canonical path).
//
// Input normalization: accepts canonical path, file:// URI, or raw filesystem
// path. All forms are normalized to a canonical path for lookup.
//
// When multiple documents resolve to the same canonical path (e.g., user opened
// both a symlink and the real file), returns the URI of the first-opened
// document for determinism.
//
// If the file is not open, returns a file:// URI for the path.
//
// Uses a cached mapping for O(1) lookup; cache is lazily rebuilt after
// document open/close operations.
func (w *Workspace) RemapPathToURI(input string) string {
	// Normalize input to canonical path
	var rawPath string
	if path, err := lsputil.URIToPath(input); err == nil {
		// Valid file:// URI - use extracted path
		rawPath = path
	} else if lsputil.HasURIScheme(input) {
		// Non-file URI scheme (e.g., untitled:) - return as-is
		return input
	} else {
		// Raw filesystem path
		rawPath = input
	}

	// Clean and convert to forward slashes to match SourceID.String() format.
	// On Windows, filepath operations produce backslashes, but SourceID
	// uses forward slashes for cross-platform consistency.
	cleanedPath := filepath.ToSlash(filepath.Clean(rawPath))

	w.mu.Lock()
	defer w.mu.Unlock()

	// Rebuild cache if invalidated
	if w.canonicalToURI == nil {
		w.canonicalToURI = w.buildCanonicalToURIMapLocked()
	}

	// Fast path: try direct lookup first. This succeeds when input is already
	// a canonical SourceID.String() path (common case from the schema loader),
	// avoiding the filesystem I/O of EvalSymlinks on hot paths like hover/definition.
	if docURI, ok := w.canonicalToURI[cleanedPath]; ok {
		return docURI
	}

	// Slow path: resolve symlinks and retry. This handles cases where input
	// is a symlink path but the map keys are resolved paths.
	if resolved, err := filepath.EvalSymlinks(rawPath); err == nil {
		canonicalPath := filepath.ToSlash(filepath.Clean(resolved))
		if canonicalPath != cleanedPath {
			if docURI, ok := w.canonicalToURI[canonicalPath]; ok {
				return docURI
			}
		}
		// File not open - return file:// URI for the canonical path
		return lsputil.PathToURI(canonicalPath)
	}

	// EvalSymlinks failed (nonexistent file, broken symlink, etc.)
	// Return file:// URI for the cleaned path
	return lsputil.PathToURI(cleanedPath)
}

// buildCanonicalToURIMap builds a mapping from canonical (symlink-resolved)
// paths to the URIs used by clients to open documents.
//
// This is needed because the schema loader resolves symlinks (via makeCanonicalPath),
// so diagnostics reference canonical paths. But clients identify documents by the
// URI they used to open them, which may be a symlink path. This mapping allows
// us to translate diagnostic URIs back to client-expected URIs.
//
// This method prefers document.SourceID (set at open time via SourceIDFromAbsolutePath)
// over runtime EvalSymlinks, since SourceID is the canonical identity used by the
// loader and is already computed. This also works for new files that don't yet
// exist on disk, where EvalSymlinks would fail.
//
// Must be called with w.mu held.
func (w *Workspace) buildCanonicalToURIMapLocked() map[string]string {
	canonicalToURI := make(map[string]string, len(w.open))
	openOrderByCanonical := make(map[string]int, len(w.open))

	for uri, doc := range w.open {
		var canonical string

		// Prefer SourceID which is already canonicalized at open time
		if !doc.SourceID.IsZero() {
			canonical = doc.SourceID.String()
		} else {
			// Fallback for non-file URIs or when SourceID unavailable
			path, err := lsputil.URIToPath(uri)
			if err != nil {
				continue
			}

			// Resolve symlinks to get the canonical path (matching loader behavior).
			// filepath.EvalSymlinks also cleans the path.
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				// If symlink resolution fails (broken symlink, permissions, etc.),
				// fall back to the cleaned path. This ensures we still have a mapping
				// even when EvalSymlinks fails.
				canonical = filepath.Clean(path)
			} else {
				canonical = resolved
			}
			// Convert to forward slashes to match SourceID.String() format
			canonical = filepath.ToSlash(canonical)
		}

		// For determinism when multiple URIs resolve to the same canonical path,
		// prefer the document that was opened first (lowest OpenOrder).
		if existingOrder, exists := openOrderByCanonical[canonical]; !exists || doc.OpenOrder < existingOrder {
			canonicalToURI[canonical] = uri
			openOrderByCanonical[canonical] = doc.OpenOrder
		}
	}

	return canonicalToURI
}

// remapToOpenDocURI remaps a diagnostic URI to an open document URI if the
// underlying file matches an open buffer.
//
// If the diagnostic's path (after symlink resolution) matches an open document,
// returns the open document's URI. Otherwise, returns a valid file:// URI.
//
// This function tolerates both file:// URIs and raw filesystem paths as input,
// since RelatedInformation may come from various sources. It always returns
// a valid URI to maintain LSP protocol correctness.
//
// Must be called with w.mu held.
func (w *Workspace) remapToOpenDocURILocked(diagURI string, canonicalToDocURI map[string]string) string {
	path, err := lsputil.URIToPath(diagURI)
	if err != nil {
		// Not a valid file:// URI.
		// Check if it's a non-file URI scheme - preserve as-is.
		if lsputil.HasURIScheme(diagURI) {
			return diagURI
		}
		// Raw filesystem path - use directly for lookup
		path = diagURI
	}

	// Convert to forward slashes to match map keys (SourceID.String() format).
	// On Windows, URIToPath produces backslashes but map keys use forward slashes.
	lookupPath := filepath.ToSlash(path)

	// The diagnostic URI is already using a canonical path (from the loader),
	// so we can look it up directly in the map.
	if docURI, ok := canonicalToDocURI[lookupPath]; ok {
		return docURI
	}

	// No match found.
	// If input was a raw path, convert to proper file:// URI for protocol correctness.
	// If input was already a file:// URI, return unchanged.
	if err != nil {
		return lsputil.PathToURI(path)
	}
	return diagURI
}

// MarkdownDocumentOpened creates a markdownDocument with normalized text and version.
// Block extraction is deferred to AnalyzeMarkdownAndPublish.
func (w *Workspace) MarkdownDocumentOpened(uri string, version int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.markdownDocs[uri] = &markdownDocument{
		URI:     uri,
		Version: version,
		Text:    normalizeLineEndings(text),
	}
}

// MarkdownDocumentChanged updates text and version for a markdown document.
// Ignores stale updates (version <= current unless either is 0).
// Does NOT re-extract blocks — that is done atomically by AnalyzeMarkdownAndPublish.
func (w *Workspace) MarkdownDocumentChanged(uri string, version int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	md := w.markdownDocs[uri]
	if md == nil {
		return
	}

	if version != 0 && md.Version != 0 && version <= md.Version {
		w.logger.Debug("ignoring stale markdown document change",
			slog.String("uri", uri),
			slog.Int("incoming_version", version),
			slog.Int("current_version", md.Version),
		)
		return
	}
	md.Version = version
	md.Text = normalizeLineEndings(text)
}

// MarkdownDocumentClosed removes a markdown document and clears its diagnostics.
func (w *Workspace) MarkdownDocumentClosed(notify notifyFunc, uri string) {
	w.mu.Lock()
	delete(w.markdownDocs, uri)

	hadPublished := w.publishedByEntry[uri] != nil
	delete(w.publishedByEntry, uri)
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

	md := w.markdownDocs[uri]
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

// GetMarkdownCurrentText returns the current text of a markdown document.
func (w *Workspace) GetMarkdownCurrentText(uri string) (string, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	md := w.markdownDocs[uri]
	if md == nil {
		return "", false
	}
	return md.Text, true
}

// ScheduleMarkdownAnalysis schedules a debounced analysis for a markdown document.
func (w *Workspace) ScheduleMarkdownAnalysis(notify notifyFunc, uri string) {
	w.debounceMu.Lock()
	defer w.debounceMu.Unlock()

	if existing, ok := w.debounces[uri]; ok {
		existing.timer.Stop()
		existing.cancel()
	}

	analyzeCtx, cancel := context.WithCancel(w.bgCtx)
	entry := &debounceEntry{cancel: cancel}

	entry.timer = time.AfterFunc(debounceDelay, func() {
		select {
		case <-analyzeCtx.Done():
			return
		default:
			w.AnalyzeMarkdownAndPublish(notify, analyzeCtx, uri)
			w.debounceMu.Lock()
			if w.debounces[uri] == entry {
				delete(w.debounces, uri)
			}
			w.debounceMu.Unlock()
		}
	})

	w.debounces[uri] = entry
}

// AnalyzeMarkdownAndPublish analyzes a markdown document's code blocks and publishes diagnostics.
func (w *Workspace) AnalyzeMarkdownAndPublish(notify notifyFunc, analyzeCtx context.Context, uri string) {
	// Read text and version under lock
	w.mu.RLock()
	md := w.markdownDocs[uri]
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
		snapshot, err := w.analyzer.Analyze(analyzeCtx, virtualPath, overlays, "", w.PositionEncoding(), load.WithDisallowImports())
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
	md = w.markdownDocs[uri]
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
	w.publishedByEntry[snap.URI] = map[string]struct{}{snap.URI: {}}
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
