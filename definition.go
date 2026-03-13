package lsp

import (
	"context"
	"path/filepath"
	"time"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/definition"
	"github.com/simon-lentz/yammm-lsp/internal/docstate"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
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

	unit := s.workspace.ResolveUnit(uri, int(params.Position.Line), int(params.Position.Character), true)
	if unit == nil {
		return nil, nil
	}

	result := s.definitionAtPosition(unit.Snapshot, unit.Doc, unit.LocalLine, unit.LocalChar)
	if result == nil {
		return nil, nil
	}

	if unit.Remap != nil {
		if loc, ok := result.(*protocol.Location); ok && loc != nil {
			// Remap the location URI and range if it points to the virtual block SourceID.
			// SymbolToLocation calls RemapPathToURI which percent-encodes '#' in virtual
			// paths (e.g., /path/to/README.md%23block-0). URIToPath reverses this.
			block := unit.Remap.Block()
			locPath, pathErr := lsputil.URIToPath(loc.URI)
			if pathErr == nil && filepath.ToSlash(locPath) == block.SourceID.String() {
				loc.URI = unit.Remap.DocumentURI()
				loc.Range = unit.Remap.RemapRange(loc.Range)
			}
		}
	}
	return result, nil
}

// definitionAtPosition returns the definition location for the symbol at the given position.
// The line and char parameters are LSP-encoding coordinates.
// Returns nil when no definition is found.
func (s *Server) definitionAtPosition(snapshot *analysis.Snapshot, doc *docstate.Snapshot, line, char int) any {
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
		return nil
	}

	enc := s.workspace.PositionEncoding()
	mapURI := s.workspace.RemapPathToURI

	internalPos, ok := lsputil.PositionFromLSP(
		snapshot.Sources,
		doc.SourceID,
		line,
		char,
		enc,
	)
	if !ok {
		return nil
	}

	ref := idx.ReferenceAtPosition(internalPos)
	if ref != nil {
		loc := definition.ResolveReference(snapshot, ref, doc.SourceID, enc, mapURI)
		if loc == nil {
			s.logger.Debug("could not resolve reference",
				"target", ref.TargetName,
				"qualifier", ref.Qualifier,
			)
		}
		return loc
	}

	sym := idx.SymbolAtPosition(internalPos)
	if sym != nil {
		return definition.ResolveSymbol(snapshot, sym, enc, mapURI)
	}

	s.logger.Debug("no symbol or reference at position",
		"uri", doc.URI,
		"position", internalPos,
	)
	return nil
}
