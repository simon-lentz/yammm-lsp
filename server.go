// Package lsp implements a Language Server Protocol server for YAMMM schema files.
package lsp

import (
	"context"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/creachadair/jrpc2"
	"github.com/creachadair/jrpc2/channel"
	"github.com/creachadair/jrpc2/handler"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/workspace"
)

// PositionEncoding is an alias for lsputil.PositionEncoding within the lsp package.
type PositionEncoding = lsputil.PositionEncoding

const (
	// PositionEncodingUTF16 counts positions in UTF-16 code units (default).
	PositionEncodingUTF16 = lsputil.PositionEncodingUTF16

	// PositionEncodingUTF8 counts positions in UTF-8 bytes.
	PositionEncodingUTF8 = lsputil.PositionEncodingUTF8
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
	workspace *workspace.Workspace

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
		logger: logger.With(slog.String("component", "server")),
		config: cfg,
		workspace: workspace.NewWorkspace(logger, workspace.Config{
			ModuleRoot: cfg.ModuleRoot,
		}),
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

// Workspace returns the workspace for testing purposes.
func (s *Server) Workspace() *workspace.Workspace { return s.workspace }

// notifier creates a workspace.NotifyFunc from a jrpc2 handler context.
// Returns nil if ctx has no jrpc2 server (e.g., in unit tests).
func (s *Server) notifier(ctx context.Context) workspace.NotifyFunc {
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
func (s *Server) shutdown(_ context.Context) (any, error) {
	s.logger.Info("shutdown request received")
	s.shutdownCalled = true
	s.traceValue = protocol.TraceValueOff
	return nil, nil //nolint:nilnil // LSP shutdown response is always null
}

// exit handles the exit notification per LSP spec.
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
	s.workspace.OpenDocument(s.notifier(ctx), uri, int(params.TextDocument.Version), params.TextDocument.Text) //nolint:contextcheck // analysis uses workspace background context, not request context
	return nil
}

// textDocumentDidChange handles textDocument/didChange.
func (s *Server) textDocumentDidChange(ctx context.Context, params *protocol.DidChangeTextDocumentParams) error {
	uri := params.TextDocument.URI
	s.logger.Debug("textDocument/didChange",
		slog.String("uri", uri),
		slog.Int("version", int(params.TextDocument.Version)),
	)
	s.workspace.ChangeDocument(s.notifier(ctx), uri, int(params.TextDocument.Version), params.ContentChanges) //nolint:contextcheck // analysis uses workspace background context, not request context
	return nil
}

// textDocumentDidClose handles textDocument/didClose.
func (s *Server) textDocumentDidClose(ctx context.Context, params *protocol.DidCloseTextDocumentParams) error {
	s.logger.Debug("textDocument/didClose", slog.String("uri", params.TextDocument.URI))
	s.workspace.CloseDocument(s.notifier(ctx), params.TextDocument.URI)
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
func (s *Server) workspaceDidChangeWorkspaceFolders(ctx context.Context, params *protocol.DidChangeWorkspaceFoldersParams) error {
	for _, folder := range params.Event.Removed {
		s.logger.Debug("workspace folder removed", slog.String("uri", folder.URI))
		s.workspace.RemoveRoot(folder.URI)
	}

	for _, folder := range params.Event.Added {
		s.logger.Debug("workspace folder added", slog.String("uri", folder.URI))
		s.workspace.AddRoot(folder.URI)
	}

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
