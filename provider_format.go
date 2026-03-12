package lsp

import (
	"context"
	"strings"
	"time"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/format"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
)

// textDocumentFormatting handles textDocument/formatting requests.
// params.Options (FormattingOptions) is intentionally ignored: yammm formatting
// is canonical (like gofmt) — tabs for indentation, trailing whitespace trimmed,
// final newline enforced. All style decisions are hardcoded.
func (s *Server) textDocumentFormatting(_ context.Context, params *protocol.DocumentFormattingParams) ([]protocol.TextEdit, error) {
	defer s.logTiming("textDocument/formatting", time.Now())
	uri := params.TextDocument.URI

	if lsputil.IsMarkdownURI(uri) {
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

	// Format using parse-aware token spacing. FormatTokenStream uses ANTLR
	// internally, catching syntax errors without a separate load.LoadString
	// pre-parse. If parsing fails, skip formatting to avoid corrupting files
	// with syntax errors.
	formatted, formatErr := format.FormatTokenStream(doc.Text)
	if formatErr != nil {
		s.logger.Debug("formatting skipped due to parse error",
			"uri", uri,
			"error", formatErr,
		)
		return []protocol.TextEdit{}, nil
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
		// lsputil.ByteToUTF16Offset(content, lineStart, targetByte) - pass 0 as lineStart
		// since we're converting just the line content
		lastChar = lsputil.ByteToUTF16Offset(lastLineContent, 0, len(lastLineContent))
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
