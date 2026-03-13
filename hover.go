package lsp

import (
	"context"
	"time"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm/location"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/docstate"
	"github.com/simon-lentz/yammm-lsp/internal/hover"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/symbols"
)

// textDocumentHover handles textDocument/hover requests.
//
//nolint:nilnil // LSP protocol: nil result means "no hover info"
func (s *Server) textDocumentHover(_ context.Context, params *protocol.HoverParams) (*protocol.Hover, error) {
	defer s.logTiming("textDocument/hover", time.Now())
	uri := params.TextDocument.URI

	s.logger.Debug("hover request",
		"uri", uri,
		"line", params.Position.Line,
		"character", params.Position.Character,
	)

	unit := s.workspace.ResolveUnit(uri, int(params.Position.Line), int(params.Position.Character), true)
	if unit == nil {
		return nil, nil
	}

	result, err := s.hoverAtPosition(unit.Snapshot, unit.Doc, unit.LocalLine, unit.LocalChar)
	if err != nil || result == nil {
		return result, err
	}

	if unit.Remap != nil {
		result.Range = unit.Remap.RemapRangePtr(result.Range)
	}
	return result, nil
}

// hoverAtPosition returns hover info for the given position within a document.
// The line and char parameters are LSP-encoding coordinates.
// Returns nil, nil when no hover info is found.
//
//nolint:nilnil // LSP protocol: nil result means "no hover info"
func (s *Server) hoverAtPosition(snapshot *analysis.Snapshot, doc *docstate.Snapshot, line, char int) (*protocol.Hover, error) {
	if snapshot.EntryVersion != doc.Version {
		s.logger.Debug("serving stale snapshot for hover",
			"uri", doc.URI,
			"snapshot_version", snapshot.EntryVersion,
			"doc_version", doc.Version,
		)
	}

	idx := snapshot.SymbolIndexAt(doc.SourceID)
	if idx == nil {
		return nil, nil
	}

	internalPos, ok := lsputil.PositionFromLSP(
		snapshot.Sources,
		doc.SourceID,
		line,
		char,
		s.workspace.PositionEncoding(),
	)
	if !ok {
		return nil, nil
	}

	ref := idx.ReferenceAtPosition(internalPos)
	if ref != nil {
		targetSym := snapshot.ResolveTypeReference(ref, doc.SourceID)
		if targetSym != nil {
			return s.buildHoverForSymbolWithRange(targetSym, snapshot, &ref.Span)
		}
	}

	sym := idx.SymbolAtPosition(internalPos)
	if sym == nil {
		return nil, nil
	}

	return s.buildHoverForSymbolWithRange(sym, snapshot, nil)
}

// buildHoverForSymbolWithRange generates hover content for a symbol.
// If overrideRange is provided, it is used for the hover range instead of the symbol's span.
// This is used when hovering a reference to use the reference's location, not the target's location.
//
//nolint:nilnil // nil result means no hover info for this symbol
func (s *Server) buildHoverForSymbolWithRange(sym *symbols.Symbol, snapshot *analysis.Snapshot, overrideRange *location.Span) (*protocol.Hover, error) {
	if sym == nil || snapshot == nil {
		return nil, nil
	}

	content := hover.RenderSymbol(sym, snapshot.Root)
	if content == "" {
		return nil, nil
	}

	// Always use Markdown: all hover renderers emit Markdown formatting (bold, backticks,
	// fenced blocks, etc.). All mainstream LSP clients support Markdown.
	contentKind := protocol.MarkupKindMarkdown

	// Use override range if provided (e.g., when hovering a reference),
	// otherwise use the symbol's own selection span.
	rangeSpan := sym.Selection
	if overrideRange != nil {
		rangeSpan = *overrideRange
	}

	// Use proper UTF-16 conversion for the hover range
	start, end, ok := lsputil.SpanToLSPRange(snapshot.Sources, rangeSpan, s.workspace.PositionEncoding())
	if !ok {
		// Fallback to naive conversion if span conversion fails
		return &protocol.Hover{
			Contents: protocol.MarkupContent{
				Kind:  contentKind,
				Value: content,
			},
			Range: &protocol.Range{
				Start: protocol.Position{
					Line:      analysis.ToUInteger(rangeSpan.Start.Line - 1),
					Character: analysis.ToUInteger(rangeSpan.Start.Column - 1),
				},
				End: protocol.Position{
					Line:      analysis.ToUInteger(rangeSpan.End.Line - 1),
					Character: analysis.ToUInteger(rangeSpan.End.Column - 1),
				},
			},
		}, nil
	}

	return &protocol.Hover{
		Contents: protocol.MarkupContent{
			Kind:  contentKind,
			Value: content,
		},
		Range: &protocol.Range{
			Start: protocol.Position{Line: analysis.ToUInteger(start[0]), Character: analysis.ToUInteger(start[1])},
			End:   protocol.Position{Line: analysis.ToUInteger(end[0]), Character: analysis.ToUInteger(end[1])},
		},
	}, nil
}
