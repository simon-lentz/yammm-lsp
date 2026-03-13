package lsp

import (
	"context"
	"time"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/docstate"
	"github.com/simon-lentz/yammm-lsp/internal/symbols"
	"github.com/simon-lentz/yammm-lsp/internal/workspace"
)

// textDocumentDocumentSymbol handles textDocument/documentSymbol requests.
//
//nolint:nilnil // LSP protocol: nil result means no symbols
func (s *Server) textDocumentDocumentSymbol(_ context.Context, params *protocol.DocumentSymbolParams) (any, error) {
	defer s.logTiming("textDocument/documentSymbol", time.Now())
	uri := params.TextDocument.URI

	s.logger.Debug("documentSymbol request", "uri", uri)

	units := s.workspace.ResolveAllUnits(uri)
	if units == nil {
		return nil, nil
	}

	var allSymbols []protocol.DocumentSymbol
	for _, unit := range units {
		syms := s.documentSymbolsFor(unit.Snapshot, unit.Doc)
		if len(syms) > 0 && unit.Remap != nil {
			syms = remapDocumentSymbolRanges(syms, unit.Remap)
		}
		allSymbols = append(allSymbols, syms...)
	}
	return allSymbols, nil
}

// remapDocumentSymbolRanges remaps Range and SelectionRange of document symbols
// from block-local coordinates to markdown coordinates. Recursively processes Children.
// Returns nil for empty input. Creates copies to avoid mutating the originals.
func remapDocumentSymbolRanges(docSymbols []protocol.DocumentSymbol, remap *workspace.BlockRemap) []protocol.DocumentSymbol {
	if len(docSymbols) == 0 {
		return nil
	}

	result := make([]protocol.DocumentSymbol, len(docSymbols))
	for i, sym := range docSymbols {
		result[i] = sym
		result[i].Range = remap.RemapRange(sym.Range)
		result[i].SelectionRange = remap.RemapRange(sym.SelectionRange)

		// Recursively remap children
		if len(sym.Children) > 0 {
			result[i].Children = remapDocumentSymbolRanges(sym.Children, remap)
		}
	}

	return result
}

// documentSymbolsFor returns document symbols for the given document within a snapshot.
// Returns nil when no symbols are available.
func (s *Server) documentSymbolsFor(snapshot *analysis.Snapshot, doc *docstate.Snapshot) []protocol.DocumentSymbol {
	if snapshot.EntryVersion != doc.Version {
		s.logger.Debug("serving stale snapshot for documentSymbol",
			"uri", doc.URI,
			"snapshot_version", snapshot.EntryVersion,
			"doc_version", doc.Version,
		)
	}

	idx := snapshot.SymbolIndexAt(doc.SourceID)
	if idx == nil {
		return nil
	}

	return symbols.BuildDocumentSymbols(idx, snapshot.Sources, s.workspace.PositionEncoding())
}
