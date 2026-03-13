package lsp

import (
	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/docstate"
	"github.com/simon-lentz/yammm-lsp/internal/markdown"
	"github.com/simon-lentz/yammm-lsp/internal/workspace"
)

// analysisUnit is a normalized view of an analysis context for a single
// document or code block. Abstracts the difference between standalone
// .yammm files and yammm code blocks embedded in markdown.
type analysisUnit struct {
	Snapshot  *analysis.Snapshot // May be nil (completion gracefully degrades)
	Doc       *docstate.Snapshot
	LocalLine int // Position in document/block-local coordinates
	LocalChar int
	Remap     *blockRemap // nil for standalone .yammm files
}

// blockRemap translates block-local coordinates back to markdown-file coordinates.
type blockRemap struct {
	mdSnap     *workspace.MarkdownDocumentSnapshot
	blockIndex int
}

// RemapRange remaps a block-local range to markdown-file coordinates.
func (r *blockRemap) RemapRange(rng protocol.Range) protocol.Range {
	startLine, startChar := r.mdSnap.BlockPositionToMarkdown(r.blockIndex,
		int(rng.Start.Line), int(rng.Start.Character))
	endLine, endChar := r.mdSnap.BlockPositionToMarkdown(r.blockIndex,
		int(rng.End.Line), int(rng.End.Character))
	return protocol.Range{
		Start: protocol.Position{Line: analysis.ToUInteger(startLine), Character: analysis.ToUInteger(startChar)},
		End:   protocol.Position{Line: analysis.ToUInteger(endLine), Character: analysis.ToUInteger(endChar)},
	}
}

// RemapRangePtr remaps a range pointer. Returns nil for nil input.
func (r *blockRemap) RemapRangePtr(rng *protocol.Range) *protocol.Range {
	if rng == nil {
		return nil
	}
	remapped := r.RemapRange(*rng)
	return &remapped
}

// resolveUnit resolves the analysis unit for a cursor position.
// For .yammm: returns file snapshot + docSnapshot at the given position.
// For markdown: maps cursor to a code block, returns block snapshot + block docSnapshot at block-local position.
// snapshotRequired=true returns nil when snapshot is nil (hover, definition).
// snapshotRequired=false allows nil snapshot (completion degrades to keywords).
func (s *Server) resolveUnit(uri string, line, char int, snapshotRequired bool) *analysisUnit {
	// Try markdown first
	if mdSnap := s.workspace.GetMarkdownDocumentSnapshot(uri); mdSnap != nil {
		blockPos := mdSnap.MarkdownPositionToBlock(line, char)
		if blockPos == nil {
			return nil
		}

		if blockPos.BlockIndex >= len(mdSnap.Blocks) {
			return nil
		}

		var snapshot *analysis.Snapshot
		if blockPos.BlockIndex < len(mdSnap.Snapshots) {
			snapshot = mdSnap.Snapshots[blockPos.BlockIndex]
		}

		if snapshotRequired && snapshot == nil {
			return nil
		}

		blockDocSnap := s.buildBlockDocumentSnapshot(mdSnap, mdSnap.Blocks[blockPos.BlockIndex])

		return &analysisUnit{
			Snapshot:  snapshot,
			Doc:       blockDocSnap,
			LocalLine: blockPos.LocalLine,
			LocalChar: blockPos.LocalChar,
			Remap: &blockRemap{
				mdSnap:     mdSnap,
				blockIndex: blockPos.BlockIndex,
			},
		}
	}

	// Standalone .yammm file
	snapshot := s.workspace.LatestSnapshot(uri)
	if snapshotRequired && snapshot == nil {
		return nil
	}

	doc := s.workspace.GetDocumentSnapshot(uri)
	if doc == nil {
		return nil
	}

	return &analysisUnit{
		Snapshot:  snapshot,
		Doc:       doc,
		LocalLine: line,
		LocalChar: char,
		Remap:     nil,
	}
}

// resolveAllUnits returns one analysisUnit per analysis region in the document.
// For .yammm: single unit. For markdown: one per code block with non-nil snapshot.
// Used by symbols (document-wide, not cursor-centric).
func (s *Server) resolveAllUnits(uri string) []analysisUnit {
	// Try markdown first
	if mdSnap := s.workspace.GetMarkdownDocumentSnapshot(uri); mdSnap != nil {
		var units []analysisUnit
		for i, snapshot := range mdSnap.Snapshots {
			if snapshot == nil || i >= len(mdSnap.Blocks) {
				continue
			}
			blockDocSnap := s.buildBlockDocumentSnapshot(mdSnap, mdSnap.Blocks[i])
			units = append(units, analysisUnit{
				Snapshot: snapshot,
				Doc:      blockDocSnap,
				Remap: &blockRemap{
					mdSnap:     mdSnap,
					blockIndex: i,
				},
			})
		}
		return units
	}

	// Standalone .yammm file
	snapshot := s.workspace.LatestSnapshot(uri)
	if snapshot == nil {
		return nil
	}

	doc := s.workspace.GetDocumentSnapshot(uri)
	if doc == nil {
		return nil
	}

	return []analysisUnit{{
		Snapshot: snapshot,
		Doc:      doc,
		Remap:    nil,
	}}
}

// buildBlockDocumentSnapshot creates a docstate.Snapshot for a single code block
// within a markdown document. This is the shared utility used by all feature
// providers (hover, completion, definition, symbols) to bridge between
// markdown-level state and block-level analysis.
func (s *Server) buildBlockDocumentSnapshot(mdSnap *workspace.MarkdownDocumentSnapshot, block markdown.CodeBlock) *docstate.Snapshot {
	depths, inComment := docstate.ComputeBraceDepths(block.Content)
	return &docstate.Snapshot{
		URI:      mdSnap.URI,
		SourceID: block.SourceID,
		Version:  mdSnap.Version,
		Text:     block.Content,
		LineState: &docstate.LineState{
			Version:        mdSnap.Version,
			BraceDepth:     depths,
			InBlockComment: inComment,
		},
	}
}
