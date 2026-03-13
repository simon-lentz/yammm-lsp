package workspace

import (
	"context"
	"sync"
	"time"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
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

// analysisScheduler manages debounced analysis scheduling and background context.
//
// debounceMu protects the debounces map. It has its own lock (not Workspace.mu)
// because debounce operations must never be called while holding Workspace.mu
// (lock ordering: debounceMu must never be acquired while holding mu).
//
// bgCtx/bgCancel and analyzer are set once at construction and are immutable.
type analysisScheduler struct {
	debounceMu sync.Mutex
	debounces  map[string]*debounceEntry

	// bgCtx is the workspace-lifetime context for analysis goroutines.
	// Cancelled during shutdown to abort in-flight analysis.
	bgCtx    context.Context //nolint:containedctx // workspace-lifetime context, not request-scoped
	bgCancel context.CancelFunc

	// analyzer for schema loading
	analyzer *analysis.Analyzer
}

// schedule schedules a debounced analysis for the given URI.
// analyzeFn is called after the debounce delay with (notify, analyzeCtx, uri).
// This unified method handles both .yammm and markdown analysis scheduling.
func (s *analysisScheduler) schedule(notify NotifyFunc, uri string, analyzeFn func(NotifyFunc, context.Context, string)) {
	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()

	// Cancel existing entry
	if existing, ok := s.debounces[uri]; ok {
		existing.timer.Stop()
		existing.cancel()
	}

	// Create new cancellation context derived from workspace background context.
	// This ensures in-flight analysis is cancelled when the workspace shuts down.
	analyzeCtx, cancel := context.WithCancel(s.bgCtx)

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
			analyzeFn(notify, analyzeCtx, uri)
			// Clean up only if this is still our entry.
			// If a new schedule call occurred while we were running,
			// a new entry will be in the map and we must not delete it.
			s.debounceMu.Lock()
			if s.debounces[uri] == entry {
				delete(s.debounces, uri)
			}
			s.debounceMu.Unlock()
		}
	})

	s.debounces[uri] = entry
}

// cancelPending cancels any pending analysis for a URI.
func (s *analysisScheduler) cancelPending(uri string) {
	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()

	if entry, ok := s.debounces[uri]; ok {
		entry.timer.Stop()
		entry.cancel()
		delete(s.debounces, uri)
	}
}

// shutdown cancels all pending analysis operations.
// This should be called during server shutdown to ensure clean termination.
func (s *analysisScheduler) shutdown() {
	// Cancel the background context first to abort any in-flight analysis
	// goroutines that may be running outside the debounce map.
	s.bgCancel()

	s.debounceMu.Lock()
	defer s.debounceMu.Unlock()

	for uri, entry := range s.debounces {
		entry.timer.Stop()
		entry.cancel()
		delete(s.debounces, uri)
	}
}

// backgroundContext returns the workspace-lifetime context for analysis.
// This context is cancelled during shutdown.
func (s *analysisScheduler) backgroundContext() context.Context {
	return s.bgCtx
}
