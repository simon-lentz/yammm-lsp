// Package lsp implements a Language Server Protocol server for YAMMM schema files.
package lsp

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/channel"
	"github.com/creachadair/jrpc2/handler"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
)

const (
	serverName = "yammm-lsp"
)

// Config holds the server configuration.
type Config struct {
	// ModuleRoot overrides the computed module root for import resolution.
	ModuleRoot string
}

// Server is the YAMMM language server. It handles both standalone .yammm
// files and YAMMM code blocks embedded in Markdown documents (.md, .markdown).
type Server struct {
	logger    *slog.Logger
	config    Config
	mux       handler.Map
	jrpcSrv   *jrpc2.Server
	workspace *Workspace

	// traceValue stores the current trace level (replaces protocol.SetTraceValue global)
	traceValue protocol.TraceValue

	// shutdownCalled tracks whether shutdown was called before exit (LSP lifecycle)
	shutdownCalled bool

	// closeOnce ensures Close is idempotent
	closeOnce sync.Once
	closeErr  error
}

// NewServer creates a new YAMMM language server.
// If logger is nil, slog.Default() is used.
func NewServer(logger *slog.Logger, cfg Config) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		logger:    logger.With(slog.String("component", "server")),
		config:    cfg,
		workspace: NewWorkspace(logger, cfg), // Pass base logger; workspace adds its own component
	}

	s.mux = handler.Map{
		// Lifecycle
		"initialize":      handler.New(s.initialize),
		"initialized":     handler.New(s.initialized),
		"shutdown":        handler.New(s.shutdown),
		"exit":            handler.New(s.exit),
		"$/setTrace":      handler.New(s.setTrace),
		"$/cancelRequest": handler.New(s.cancelRequest),

		// Text Document Synchronization
		"textDocument/didOpen":   handler.New(s.textDocumentDidOpen),
		"textDocument/didChange": handler.New(s.textDocumentDidChange),
		"textDocument/didClose":  handler.New(s.textDocumentDidClose),

		// Language Features
		"textDocument/definition":     handler.New(s.textDocumentDefinition),
		"textDocument/hover":          handler.New(s.textDocumentHover),
		"textDocument/completion":     handler.New(s.textDocumentCompletion),
		"textDocument/documentSymbol": handler.New(s.textDocumentDocumentSymbol),
		"textDocument/formatting":     handler.New(s.textDocumentFormatting),

		// Workspace
		"workspace/didChangeWatchedFiles":     handler.New(s.workspaceDidChangeWatchedFiles),
		"workspace/didChangeWorkspaceFolders": handler.New(s.workspaceDidChangeWorkspaceFolders),
	}

	return s
}

// Mux returns the handler map for testing purposes.
func (s *Server) Mux() handler.Map { return s.mux }

// notifier creates a notifyFunc from a jrpc2 handler context.
// Returns nil if ctx has no jrpc2 server (e.g., in unit tests).
func (s *Server) notifier(ctx context.Context) notifyFunc {
	srv := recoverServerFromContext(ctx)
	if srv == nil {
		return nil
	}
	return func(method string, params any) {
		_ = srv.Notify(ctx, method, params) //nolint:errcheck // best-effort notification; no recovery path
	}
}

// recoverServerFromContext safely extracts the jrpc2 server from ctx.
// Returns nil if no server is present (jrpc2.ServerFromContext panics
// on a bare context because it uses an unsafe type assertion).
func recoverServerFromContext(ctx context.Context) (srv *jrpc2.Server) {
	defer func() {
		if r := recover(); r != nil {
			srv = nil
		}
	}()
	return jrpc2.ServerFromContext(ctx)
}

// RunStdio runs the server using stdio transport with LSP framing.
func (s *Server) RunStdio() error {
	ch := channel.LSP(os.Stdin, os.Stdout)
	s.jrpcSrv = jrpc2.NewServer(s.mux, &jrpc2.ServerOptions{
		AllowPush: true, // required for server-to-client notifications
	})
	s.jrpcSrv.Start(ch)
	return s.jrpcSrv.Wait() //nolint:wrapcheck // top-level server entrypoint
}

// Shutdown initiates graceful server shutdown.
// It cancels pending workspace operations to ensure clean termination.
func (s *Server) Shutdown() {
	s.logger.Info("initiating shutdown")
	s.workspace.Shutdown()
}

// Close stops the JSON-RPC server, causing RunStdio to return.
// This enables graceful shutdown when a signal is received.
//
// Close is idempotent: multiple calls return the same result and do not panic.
// It is safe to call before RunStdio (returns nil if server not started).
func (s *Server) Close() error {
	if s.jrpcSrv == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.jrpcSrv.Stop()
	})
	return s.closeErr
}

// logTiming logs elapsed time for an LSP request at debug level.
func (s *Server) logTiming(method string, start time.Time) {
	s.logger.Debug("request completed",
		slog.String("method", method),
		slog.Duration("elapsed", time.Since(start)),
	)
}

// initialize handles the initialize request.
func (s *Server) initialize(_ context.Context, params *protocol.InitializeParams) (any, error) {
	s.logger.Info("initialize request received",
		slog.String("client_name", clientName(params)),
		slog.String("root_uri", rootURI(params)),
	)

	// Log client capabilities summary
	s.logClientCapabilities(params.Capabilities)

	// Extract workspace folders
	switch {
	case params.WorkspaceFolders != nil:
		for _, folder := range params.WorkspaceFolders {
			s.workspace.AddRoot(folder.URI)
			s.logger.Debug("workspace folder", slog.String("uri", folder.URI))
		}
	case params.RootURI != nil:
		s.workspace.AddRoot(*params.RootURI)
	case params.RootPath != nil:
		// Fallback for older LSP clients that only provide RootPath
		s.workspace.AddRoot(lsputil.PathToURI(*params.RootPath))
	}

	// Use UTF-16 encoding (default for VS Code compatibility)
	posEncoding := PositionEncodingUTF16
	s.workspace.SetPositionEncoding(posEncoding)
	s.logger.Info("using position encoding", slog.String("encoding", string(posEncoding)))

	// Build capabilities manually (replaces CreateServerCapabilities)
	syncKind := protocol.TextDocumentSyncKindFull
	trueVal := true
	capabilities := protocol.ServerCapabilities{
		TextDocumentSync: &protocol.TextDocumentSyncOptions{
			OpenClose: &trueVal,
			Change:    &syncKind,
		},
		CompletionProvider: &protocol.CompletionOptions{
			TriggerCharacters: []string{".", " "},
		},
		HoverProvider:              true,
		DefinitionProvider:         true,
		DocumentSymbolProvider:     true,
		DocumentFormattingProvider: true,
		Workspace: &protocol.ServerCapabilitiesWorkspace{
			WorkspaceFolders: &protocol.WorkspaceFoldersServerCapabilities{
				Supported:           &trueVal,
				ChangeNotifications: &protocol.BoolOrString{Value: true},
			},
		},
	}

	version := "dev"
	return protocol.InitializeResult{
		Capabilities: capabilities,
		ServerInfo: &protocol.InitializeResultServerInfo{
			Name:    serverName,
			Version: &version,
		},
	}, nil
}

// initialized handles the initialized notification.
func (s *Server) initialized(_ context.Context, params *protocol.InitializedParams) error {
	s.logger.Info("server initialized")
	return nil
}

// shutdown handles the shutdown request.
// LSP shutdown is a request (returns a response), so it returns (any, error).
func (s *Server) shutdown(_ context.Context) (any, error) {
	s.logger.Info("shutdown request received")
	s.shutdownCalled = true
	s.traceValue = protocol.TraceValueOff
	return nil, nil //nolint:nilnil // LSP shutdown response is always null
}

// exit handles the exit notification per LSP spec.
// Exit code is 0 if shutdown was called first, 1 otherwise.
//
// os.Exit is intentional: the LSP spec requires the process to terminate on
// the exit notification. This bypasses deferred cleanup in main.run(), but
// the JSON logger does not buffer, so no output is lost.
func (s *Server) exit(_ context.Context) error {
	exitCode := 0
	if !s.shutdownCalled {
		s.logger.Warn("exit called without shutdown")
		exitCode = 1
	}
	s.logger.Info("exit notification received", slog.Int("exit_code", exitCode))
	os.Exit(exitCode)
	return nil // unreachable
}

// setTrace handles the $/setTrace notification.
func (s *Server) setTrace(_ context.Context, params *protocol.SetTraceParams) error {
	s.logger.Debug("setTrace", slog.String("value", params.Value))
	s.traceValue = params.Value
	return nil
}

// cancelRequest handles the $/cancelRequest notification.
//
// This method logs cancellation requests for debugging. The current implementation
// relies on context-based cancellation in ScheduleAnalysis for debounced operations.
func (s *Server) cancelRequest(_ context.Context, params *protocol.CancelParams) error {
	s.logger.Debug("cancelRequest", slog.Any("id", params.ID))
	return nil
}

// textDocumentDidOpen handles textDocument/didOpen.
func (s *Server) textDocumentDidOpen(ctx context.Context, params *protocol.DidOpenTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.logger.Debug("textDocument/didOpen",
		slog.String("uri", uri),
		slog.Int("version", int(params.TextDocument.Version)),
	)

	notify := s.notifier(ctx)

	switch {
	case lsputil.IsYammmURI(uri):
		s.workspace.DocumentOpened(uri, int(params.TextDocument.Version), params.TextDocument.Text)
		s.workspace.AnalyzeAndPublish(notify, s.workspace.BackgroundContext(), uri) //nolint:contextcheck // analysis outlives request

	case lsputil.IsMarkdownURI(uri):
		s.workspace.MarkdownDocumentOpened(uri, int(params.TextDocument.Version), params.TextDocument.Text)
		s.workspace.AnalyzeMarkdownAndPublish(notify, s.workspace.BackgroundContext(), uri) //nolint:contextcheck // analysis outlives request

	default:
		s.logger.Debug("ignoring didOpen for unsupported file type", slog.String("uri", uri))
	}

	return nil
}

// textDocumentDidChange handles textDocument/didChange.
func (s *Server) textDocumentDidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.logger.Debug("textDocument/didChange",
		slog.String("uri", uri),
		slog.Int("version", int(params.TextDocument.Version)),
	)

	switch {
	case lsputil.IsYammmURI(uri):
		// Existing .yammm path
		if len(params.ContentChanges) > 0 {
			var lastFullChange *protocol.TextDocumentContentChangeEventWhole
			for _, rawChange := range params.ContentChanges {
				if change, ok := rawChange.(protocol.TextDocumentContentChangeEventWhole); ok {
					lastFullChange = &change
				}
			}

			if lastFullChange != nil {
				s.workspace.DocumentChanged(uri, int(params.TextDocument.Version), lastFullChange.Text)
			} else if _, ok := params.ContentChanges[0].(protocol.TextDocumentContentChangeEvent); ok {
				s.logger.Warn("received incremental change but server advertises full sync",
					slog.String("uri", uri), slog.Int("version", int(params.TextDocument.Version)))
				s.applyIncrementalChanges(params)
			}
		}
		s.workspace.ScheduleAnalysis(s.notifier(ctx), uri) //nolint:contextcheck // notifyFunc captures ctx; ScheduleAnalysis is fire-and-forget

	case lsputil.IsMarkdownURI(uri):
		if len(params.ContentChanges) > 0 {
			var lastFullChange *protocol.TextDocumentContentChangeEventWhole
			for _, rawChange := range params.ContentChanges {
				if change, ok := rawChange.(protocol.TextDocumentContentChangeEventWhole); ok {
					lastFullChange = &change
				}
			}

			if lastFullChange != nil {
				s.workspace.MarkdownDocumentChanged(uri, int(params.TextDocument.Version), lastFullChange.Text)
			} else if _, ok := params.ContentChanges[0].(protocol.TextDocumentContentChangeEvent); ok {
				s.logger.Warn("received incremental change but server advertises full sync (markdown)",
					slog.String("uri", uri), slog.Int("version", int(params.TextDocument.Version)))
				currentText, ok := s.workspace.GetMarkdownCurrentText(uri)
				if ok {
					merged := mergeIncrementalChanges(currentText, s.workspace.PositionEncoding(),
						params.ContentChanges, s.logger)
					s.workspace.MarkdownDocumentChanged(uri, int(params.TextDocument.Version), merged)
				}
			}
		}
		s.workspace.ScheduleMarkdownAnalysis(s.notifier(ctx), uri) //nolint:contextcheck // notifyFunc captures ctx; schedule is fire-and-forget

	default:
		s.logger.Debug("ignoring didChange for unsupported file type", slog.String("uri", uri))
	}

	return nil
}

// applyIncrementalChanges applies incremental text changes to a document.
// This handles misbehaving clients that send incremental changes despite
// the server advertising full sync mode.
func (s *Server) applyIncrementalChanges(params *protocol.DidChangeTextDocumentParams) {
	doc := s.workspace.GetDocumentSnapshot(params.TextDocument.URI)
	if doc == nil {
		s.logger.Warn("incremental change for unknown document",
			slog.String("uri", params.TextDocument.URI),
		)
		return
	}

	text := mergeIncrementalChanges(doc.Text, s.workspace.PositionEncoding(), params.ContentChanges, s.logger)

	s.workspace.DocumentChanged(
		params.TextDocument.URI,
		int(params.TextDocument.Version),
		text,
	)
}

// mergeIncrementalChanges applies incremental content changes to currentText
// and returns the merged result. It is a pure function with no side effects.
func mergeIncrementalChanges(currentText string, enc PositionEncoding, changes []any, logger *slog.Logger) string {
	text := normalizeLineEndings(currentText)

	for _, rawChange := range changes {
		change, ok := rawChange.(protocol.TextDocumentContentChangeEvent)
		if !ok {
			continue
		}
		if change.Range == nil {
			text = normalizeLineEndings(change.Text)
			continue
		}

		lines := strings.Split(text, "\n")
		startOffset := rangeToByteOffset(lines, int(change.Range.Start.Line), int(change.Range.Start.Character), enc)
		endOffset := rangeToByteOffset(lines, int(change.Range.End.Line), int(change.Range.End.Character), enc)

		if startOffset <= len(text) && endOffset <= len(text) && startOffset <= endOffset {
			text = text[:startOffset] + normalizeLineEndings(change.Text) + text[endOffset:]
		} else {
			if logger != nil {
				logger.Warn("incremental change has invalid range, using full-text fallback",
					slog.Int("start_offset", startOffset),
					slog.Int("end_offset", endOffset),
					slog.Int("text_len", len(text)),
				)
			}
			text = normalizeLineEndings(change.Text)
		}
	}
	return text
}

// rangeToByteOffset converts an LSP position to a byte offset in the document.
// The encoding parameter specifies how character positions are counted (UTF-16 or UTF-8).
func rangeToByteOffset(lines []string, line, char int, enc PositionEncoding) int {
	offset := 0

	// Sum lengths of preceding lines (including newlines)
	for i := 0; i < line && i < len(lines); i++ {
		offset += len(lines[i]) + 1 // +1 for newline
	}

	// Add character offset within line using the negotiated encoding
	if line < len(lines) {
		lineContent := []byte(lines[line])
		var charOffset int
		switch enc {
		case PositionEncodingUTF8:
			// UTF-8 encoding: character offset IS byte offset
			charOffset = min(char, len(lineContent))
		default:
			// UTF-16 encoding (default): convert from UTF-16 code units to bytes
			charOffset = lsputil.UTF16CharToByteOffset(lineContent, 0, char)
		}
		offset += charOffset
	}

	return offset
}

// normalizeLineEndings converts CRLF and CR line endings to LF.
// This ensures consistent line ending handling across platforms.
// Windows clients may send CRLF (\r\n), which would cause incorrect
// byte offset calculations in rangeToByteOffset.
func normalizeLineEndings(text string) string {
	// First replace CRLF with LF, then replace any remaining CR with LF
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text
}

// textDocumentDidClose handles textDocument/didClose.
func (s *Server) textDocumentDidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.logger.Debug("textDocument/didClose", slog.String("uri", uri))

	notify := s.notifier(ctx)

	switch {
	case lsputil.IsYammmURI(uri):
		s.workspace.DocumentClosed(notify, uri)

	case lsputil.IsMarkdownURI(uri):
		s.workspace.MarkdownDocumentClosed(notify, uri)

	default:
		s.logger.Debug("ignoring didClose for unsupported file type", slog.String("uri", uri))
	}

	return nil
}

// workspaceDidChangeWatchedFiles handles workspace/didChangeWatchedFiles.
func (s *Server) workspaceDidChangeWatchedFiles(ctx context.Context, params *protocol.DidChangeWatchedFilesParams) error {
	for _, change := range params.Changes {
		s.logger.Debug("watched file changed",
			slog.String("uri", change.URI),
			slog.Int("type", int(change.Type)),
		)
		s.workspace.FileChanged(s.notifier(ctx), change.URI, change.Type) //nolint:contextcheck // notifyFunc captures ctx
	}
	return nil
}

// workspaceDidChangeWorkspaceFolders handles workspace/didChangeWorkspaceFolders.
// This is called when the user adds or removes workspace folders in VS Code.
func (s *Server) workspaceDidChangeWorkspaceFolders(ctx context.Context, params *protocol.DidChangeWorkspaceFoldersParams) error {
	// Process removed folders first to ensure clean state
	for _, folder := range params.Event.Removed {
		s.logger.Debug("workspace folder removed", slog.String("uri", folder.URI))
		s.workspace.RemoveRoot(folder.URI)
	}

	// Process added folders
	for _, folder := range params.Event.Added {
		s.logger.Debug("workspace folder added", slog.String("uri", folder.URI))
		s.workspace.AddRoot(folder.URI)
	}

	// Trigger re-analysis of open documents whose module root may have changed
	s.workspace.ReanalyzeOpenDocuments(s.notifier(ctx)) //nolint:contextcheck // notifyFunc captures ctx

	return nil
}

// Helper functions

func clientName(params *protocol.InitializeParams) string {
	if params.ClientInfo != nil {
		if params.ClientInfo.Version != nil {
			return params.ClientInfo.Name + " " + *params.ClientInfo.Version
		}
		return params.ClientInfo.Name
	}
	return "unknown"
}

func rootURI(params *protocol.InitializeParams) string {
	if params.RootURI != nil {
		return *params.RootURI
	}
	return ""
}

func (s *Server) logClientCapabilities(caps protocol.ClientCapabilities) {
	var features []string

	if caps.TextDocument != nil {
		if caps.TextDocument.Completion != nil {
			features = append(features, "completion")
			if caps.TextDocument.Completion.CompletionItem != nil {
				if caps.TextDocument.Completion.CompletionItem.SnippetSupport != nil &&
					*caps.TextDocument.Completion.CompletionItem.SnippetSupport {
					features = append(features, "snippets")
				}
			}
		}
		if caps.TextDocument.Hover != nil {
			features = append(features, "hover")
			if caps.TextDocument.Hover.ContentFormat != nil &&
				slices.Contains(caps.TextDocument.Hover.ContentFormat, protocol.MarkupKindMarkdown) {
				features = append(features, "hover-markdown")
			}
		}
		if caps.TextDocument.Definition != nil {
			features = append(features, "definition")
		}
		if caps.TextDocument.DocumentSymbol != nil {
			features = append(features, "document-symbol")
		}
		if caps.TextDocument.Formatting != nil {
			features = append(features, "formatting")
		}
	}

	s.logger.Info("client capabilities", slog.Any("features", features))
}
