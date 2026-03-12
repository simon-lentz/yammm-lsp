package lsp

import (
	"context"
	"strings"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm/diag"
	"github.com/simon-lentz/yammm/schema/load"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/format"
)

// textDocumentFormatting handles textDocument/formatting requests.
// params.Options (FormattingOptions) is intentionally ignored: yammm formatting
// is canonical (like gofmt) — tabs for indentation, trailing whitespace trimmed,
// final newline enforced. All style decisions are hardcoded.
func (s *Server) textDocumentFormatting(_ context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	uri := params.TextDocument.URI

	if isMarkdownURI(uri) {
		return []protocol.TextEdit{}, nil
	}

	s.logger.Debug("formatting request",
		"uri", uri,
	)

	// Get the document snapshot
	doc := s.workspace.GetDocumentSnapshot(uri)
	if doc == nil {
		return nil, nil
	}

	// Check for syntax errors only - semantic errors like unresolved imports
	// should not prevent formatting. This ensures we don't corrupt files with
	// syntax errors while still allowing formatting for files with imports.
	ctx := context.Background()
	_, result, err := load.LoadString(ctx, doc.Text, "format-check") //nolint:contextcheck // intentional: analysis context is independent of LSP request
	if err != nil {
		s.logger.Debug("formatting skipped due to load error",
			"uri", uri,
			"error", err,
		)
		return []protocol.TextEdit{}, nil
	}

	// Only skip formatting if there are syntax errors (not semantic errors)
	if hasSyntaxErrors(result) {
		s.logger.Debug("formatting skipped due to syntax errors",
			"uri", uri,
		)
		return []protocol.TextEdit{}, nil
	}

	// Format the document with parse-aware token spacing. Fall back to the
	// conservative line-by-line formatter if internal formatting fails.
	formatted, formatErr := format.FormatTokenStream(doc.Text)
	if formatErr != nil {
		s.logger.Debug("token-stream formatting failed, falling back",
			"uri", uri,
			"error", formatErr,
		)
		formatted = format.FormatDocument(doc.Text)
	}

	// If no changes, return empty edits
	if formatted == doc.Text {
		return []protocol.TextEdit{}, nil
	}

	// Return a single edit that replaces the entire document
	lines := strings.Split(doc.Text, "\n")
	lastLine := len(lines) - 1
	lastLineContent := []byte(lines[lastLine])

	// Convert byte length based on negotiated position encoding.
	// UTF-8: character offset IS byte offset (no conversion needed)
	// UTF-16: convert byte offset to UTF-16 code units for non-ASCII safety
	var lastChar int
	switch s.workspace.PositionEncoding() {
	case PositionEncodingUTF8:
		// UTF-8: character offset is byte offset
		lastChar = len(lastLineContent)
	case PositionEncodingUTF16:
		fallthrough
	default:
		// UTF-16 (default): convert byte offset to UTF-16 code units
		// ByteToUTF16Offset(content, lineStart, targetByte) - pass 0 as lineStart
		// since we're converting just the line content
		lastChar = ByteToUTF16Offset(lastLineContent, 0, len(lastLineContent))
	}

	return []protocol.TextEdit{
		{
			Range: protocol.Range{
				Start: protocol.Position{Line: 0, Character: 0},
				End: protocol.Position{
					Line:      analysis.ToUInteger(lastLine),
					Character: analysis.ToUInteger(lastChar),
				},
			},
			NewText: formatted,
		},
	}, nil
}

// hasSyntaxErrors checks if the result contains any syntax parsing errors.
// This is used by formatting to distinguish between:
//   - Syntax errors (unparseable file - don't format)
//   - Semantic errors like unresolved imports (formattable file)
//
// We iterate Issues() rather than Errors() and explicitly check severity to be
// robust against future syntax diagnostics that might use different severities.
// This blocks formatting for Fatal/Error syntax issues but allows formatting
// for files with Warning-level syntax diagnostics (e.g., deprecation warnings).
func hasSyntaxErrors(result diag.Result) bool {
	for issue := range result.Issues() {
		if issue.Code().Category() == diag.CategorySyntax &&
			issue.Severity().IsAtLeastAsSevereAs(diag.Error) {
			return true
		}
	}
	return false
}
