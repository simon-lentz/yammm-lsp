package lsp

import (
	"path/filepath"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
)

// uriMapper manages canonical path mapping and per-entry publication tracking.
//
// All fields are protected by Workspace.mu (external lock).
// Callers must hold Workspace.mu before calling any method.
type uriMapper struct {
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

	// Previously published diagnostics URIs, keyed by entry URI.
	// This per-entry tracking ensures that analyzing one entry doesn't
	// clear diagnostics published by a different entry file.
	publishedByEntry map[string]map[string]struct{}
}

// invalidateCache marks the canonical-to-URI cache as stale.
// Must be called with Workspace.mu held.
func (m *uriMapper) invalidateCache() {
	m.canonicalToURI = nil
}

// ensureCache rebuilds the canonical-to-URI cache if invalidated.
// Returns the current cache. Must be called with Workspace.mu held.
func (m *uriMapper) ensureCache(open map[string]*document) map[string]string {
	if m.canonicalToURI == nil {
		m.canonicalToURI = m.buildCanonicalToURIMap(open)
	}
	return m.canonicalToURI
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
// Must be called with Workspace.mu held.
func (m *uriMapper) buildCanonicalToURIMap(open map[string]*document) map[string]string {
	canonicalToURI := make(map[string]string, len(open))
	openOrderByCanonical := make(map[string]int, len(open))

	for uri, doc := range open {
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
// Must be called with Workspace.mu held.
func (m *uriMapper) remapToOpenDocURI(diagURI string, cache map[string]string) string {
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
	if docURI, ok := cache[lookupPath]; ok {
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

// computePublicationPlan computes the publication plan for diagnostics.
// It returns the diagnostics grouped by URI (with symlink remapping applied),
// the list of stale URIs that need to be cleared, and document versions.
//
// entryURI identifies the entry file being analyzed. Per-entry tracking ensures
// that analyzing one entry doesn't clear diagnostics from another entry.
//
// Must be called with Workspace.mu held.
func (m *uriMapper) computePublicationPlan(entryURI string, snapshot *analysis.Snapshot, open map[string]*document) (diagsByURI map[string][]protocol.Diagnostic, staleURIs []string, versionsByURI map[string]*protocol.Integer) {
	// Build map from canonical paths to open document URIs.
	cache := m.ensureCache(open)

	// Collect current URIs with diagnostics
	currentURIs := make(map[string]struct{})

	// Group diagnostics by URI, remapping to open document URIs where applicable
	diagsByURI = make(map[string][]protocol.Diagnostic)

	for _, lspDiag := range snapshot.LSPDiagnostics {
		// Remap the diagnostic URI to the open document URI if available
		pubURI := m.remapToOpenDocURI(lspDiag.URI, cache)

		// Also remap URIs in RelatedInformation
		diag := lspDiag.Diagnostic
		if len(diag.RelatedInformation) > 0 {
			remapped := make([]protocol.DiagnosticRelatedInformation, len(diag.RelatedInformation))
			for i, rel := range diag.RelatedInformation {
				remapped[i] = protocol.DiagnosticRelatedInformation{
					Location: protocol.Location{
						URI:   m.remapToOpenDocURI(rel.Location.URI, cache),
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
	previousURIs := m.publishedByEntry[entryURI]
	staleURIs = make([]string, 0)
	for uri := range previousURIs {
		if _, ok := currentURIs[uri]; !ok {
			staleURIs = append(staleURIs, uri)
		}
	}

	// Update published URIs tracking for this entry only
	m.publishedByEntry[entryURI] = currentURIs

	// Capture document versions while lock is held.
	// LSP document versions fit in int32 per spec.
	versionsByURI = make(map[string]*protocol.Integer)
	for uri := range diagsByURI {
		if doc, ok := open[uri]; ok {
			v := protocol.Integer(doc.Version) //nolint:gosec // LSP version fits int32
			versionsByURI[uri] = &v
		}
	}
	for _, uri := range staleURIs {
		if doc, ok := open[uri]; ok {
			v := protocol.Integer(doc.Version) //nolint:gosec // LSP version fits int32
			versionsByURI[uri] = &v
		}
	}

	return diagsByURI, staleURIs, versionsByURI
}

// clearEntryPublications removes and returns the set of URIs published by the
// given entry. Used during DocumentClosed to determine what diagnostics to clear.
//
// Must be called with Workspace.mu held.
func (m *uriMapper) clearEntryPublications(entryURI string) map[string]struct{} {
	published := m.publishedByEntry[entryURI]
	delete(m.publishedByEntry, entryURI)
	return published
}

// uriStillPublishedByOthers checks if any other entry still publishes diagnostics
// to the given URI. Used during DocumentClosed to avoid clearing diagnostics that
// are still relevant from another entry's analysis.
//
// Must be called with Workspace.mu held.
func (m *uriMapper) uriStillPublishedByOthers(pubURI string) bool {
	for _, otherPubs := range m.publishedByEntry {
		if _, ok := otherPubs[pubURI]; ok {
			return true
		}
	}
	return false
}

// remapPathToURI maps a path to the client's document URI if the file is open.
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
//
// Must be called with Workspace.mu held.
func (m *uriMapper) remapPathToURI(input string, open map[string]*document) string {
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

	// Rebuild cache if invalidated
	cache := m.ensureCache(open)

	// Fast path: try direct lookup first. This succeeds when input is already
	// a canonical SourceID.String() path (common case from the schema loader),
	// avoiding the filesystem I/O of EvalSymlinks on hot paths like hover/definition.
	if docURI, ok := cache[cleanedPath]; ok {
		return docURI
	}

	// Slow path: resolve symlinks and retry. This handles cases where input
	// is a symlink path but the map keys are resolved paths.
	if resolved, err := filepath.EvalSymlinks(rawPath); err == nil {
		canonicalPath := filepath.ToSlash(filepath.Clean(resolved))
		if canonicalPath != cleanedPath {
			if docURI, ok := cache[canonicalPath]; ok {
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
