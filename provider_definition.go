package lsp

import (
	"context"
	"path/filepath"
	"time"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm/location"
	"github.com/simon-lentz/yammm/schema"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/symbols"
)

// textDocumentDefinition handles textDocument/definition requests.
// Returns nil, nil when no definition is found (standard LSP behavior).
//
//nolint:nilnil // LSP protocol: nil result means "no definition found"
func (s *Server) textDocumentDefinition(_ context.Context, params *protocol.DefinitionParams) (any, error) {
	defer s.logTiming("textDocument/definition", time.Now())
	uri := params.TextDocument.URI

	s.logger.Debug("definition request",
		"uri", uri,
		"line", params.Position.Line,
		"character", params.Position.Character,
	)

	unit := s.resolveUnit(uri, int(params.Position.Line), int(params.Position.Character), true)
	if unit == nil {
		return nil, nil
	}

	result, err := s.definitionAtPosition(unit.Snapshot, unit.Doc, unit.LocalLine, unit.LocalChar)
	if err != nil || result == nil {
		return result, err
	}

	if unit.Remap != nil {
		if loc, ok := result.(*protocol.Location); ok && loc != nil {
			// Remap the location URI and range if it points to the virtual block SourceID.
			// symbolToLocation calls RemapPathToURI which percent-encodes '#' in virtual
			// paths (e.g., /path/to/README.md%23block-0). URIToPath reverses this.
			block := unit.Remap.mdSnap.Blocks[unit.Remap.blockIndex]
			locPath, pathErr := lsputil.URIToPath(loc.URI)
			if pathErr == nil && filepath.ToSlash(locPath) == block.SourceID.String() {
				loc.URI = unit.Remap.mdSnap.URI
				loc.Range = unit.Remap.RemapRange(loc.Range)
			}
		}
	}
	return result, nil
}

// definitionAtPosition returns the definition location for the symbol at the given position.
// The line and char parameters are LSP-encoding coordinates.
// Returns nil, nil when no definition is found.
//
//nolint:nilnil // LSP protocol: nil result means "no definition found"
func (s *Server) definitionAtPosition(snapshot *analysis.Snapshot, doc *documentSnapshot, line, char int) (any, error) {
	if snapshot.EntryVersion != doc.Version {
		s.logger.Debug("serving stale snapshot for definition",
			"uri", doc.URI,
			"snapshot_version", snapshot.EntryVersion,
			"doc_version", doc.Version,
		)
	}

	idx := snapshot.SymbolIndexAt(doc.SourceID)
	if idx == nil {
		s.logger.Debug("no symbol index for source", "source", doc.SourceID)
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
		return s.resolveReferenceDefinition(snapshot, ref, doc.SourceID)
	}

	sym := idx.SymbolAtPosition(internalPos)
	if sym != nil {
		return s.resolveSymbolDefinition(snapshot, sym)
	}

	s.logger.Debug("no symbol or reference at position",
		"uri", doc.URI,
		"position", internalPos,
	)
	return nil, nil
}

// resolveReferenceDefinition resolves a type reference to its definition location.
//
//nolint:nilnil // LSP protocol: nil result means "no definition found"
func (s *Server) resolveReferenceDefinition(snapshot *analysis.Snapshot, ref *symbols.ReferenceSymbol, fromSourceID location.SourceID) (any, error) {
	// Resolve the reference to its target symbol
	targetSym := snapshot.ResolveTypeReference(ref, fromSourceID)
	if targetSym == nil {
		s.logger.Debug("could not resolve reference",
			"target", ref.TargetName,
			"qualifier", ref.Qualifier,
		)
		return nil, nil
	}

	return s.symbolToLocation(snapshot, targetSym), nil
}

// resolveSymbolDefinition handles definition requests on symbol declarations.
// For most symbols, returns the symbol's own location. For import aliases,
// navigates to the imported file.
//
//nolint:nilnil // LSP protocol: nil result means "no definition found"
func (s *Server) resolveSymbolDefinition(snapshot *analysis.Snapshot, sym *symbols.Symbol) (any, error) {
	switch sym.Kind {
	case symbols.SymbolImport:
		// For imports, navigate to the imported schema's declaration.
		// Uses RemapPathToURI to ensure the URI matches the client's open document URI
		// (important for symlink scenarios).
		if imp, ok := sym.Data.(*schema.Import); ok && imp.Schema() != nil {
			importedSchema := imp.Schema()
			uri := s.workspace.RemapPathToURI(importedSchema.SourceID().String())
			schemaSpan := importedSchema.Span()

			// Try proper UTF-16 conversion if sources are available
			start, end, ok := lsputil.SpanToLSPRange(snapshot.Sources, schemaSpan, s.workspace.PositionEncoding())
			if ok {
				return &protocol.Location{
					URI: uri,
					Range: protocol.Range{
						Start: protocol.Position{Line: analysis.ToUInteger(start[0]), Character: analysis.ToUInteger(start[1])},
						End:   protocol.Position{Line: analysis.ToUInteger(end[0]), Character: analysis.ToUInteger(end[1])},
					},
				}, nil
			}

			// Fallback to naive conversion from span (1-indexed to 0-indexed)
			if !schemaSpan.IsZero() {
				return &protocol.Location{
					URI: uri,
					Range: protocol.Range{
						Start: protocol.Position{
							Line:      analysis.ToUInteger(schemaSpan.Start.Line - 1),
							Character: analysis.ToUInteger(schemaSpan.Start.Column - 1),
						},
						End: protocol.Position{
							Line:      analysis.ToUInteger(schemaSpan.End.Line - 1),
							Character: analysis.ToUInteger(schemaSpan.End.Column - 1),
						},
					},
				}, nil
			}

			// Last resort: point to beginning of file
			return &protocol.Location{
				URI:   uri,
				Range: protocol.Range{},
			}, nil
		}
		return nil, nil

	default:
		// For other symbols, return the symbol's own location
		return s.symbolToLocation(snapshot, sym), nil
	}
}

// symbolToLocation converts a Symbol to an LSP Location using proper UTF-16 conversion.
// Uses RemapPathToURI to ensure the URI matches the client's open document URI
// (important for symlink scenarios).
func (s *Server) symbolToLocation(snapshot *analysis.Snapshot, sym *symbols.Symbol) *protocol.Location {
	if sym == nil || sym.Range.IsZero() {
		return nil
	}

	uri := s.workspace.RemapPathToURI(sym.SourceID.String())

	// Use proper UTF-16 conversion for the range
	start, end, ok := lsputil.SpanToLSPRange(snapshot.Sources, sym.Selection, s.workspace.PositionEncoding())
	if !ok {
		// Fallback to naive conversion if span conversion fails
		return &protocol.Location{
			URI: uri,
			Range: protocol.Range{
				Start: protocol.Position{
					Line:      analysis.ToUInteger(sym.Selection.Start.Line - 1),
					Character: analysis.ToUInteger(sym.Selection.Start.Column - 1),
				},
				End: protocol.Position{
					Line:      analysis.ToUInteger(sym.Selection.End.Line - 1),
					Character: analysis.ToUInteger(sym.Selection.End.Column - 1),
				},
			},
		}
	}

	return &protocol.Location{
		URI: uri,
		Range: protocol.Range{
			Start: protocol.Position{Line: analysis.ToUInteger(start[0]), Character: analysis.ToUInteger(start[1])},
			End:   protocol.Position{Line: analysis.ToUInteger(end[0]), Character: analysis.ToUInteger(end[1])},
		},
	}
}
