package lsp

import (
	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/markdown"
)

// MarkdownDocument tracks code blocks in an open markdown file.
// This is workspace-internal mutable state — server handlers must NOT
// access this type directly. Use MarkdownDocumentSnapshot (obtained via
// GetMarkdownDocumentSnapshot) for safe concurrent reads.
//
// ATOMICITY INVARIANT: Blocks and Snapshots must only be replaced together,
// atomically under the workspace lock, by AnalyzeMarkdownAndPublish.
type MarkdownDocument struct {
	URI       string
	Version   int
	Text      string
	Blocks    []markdown.CodeBlock
	Snapshots []*analysis.Snapshot
}

// MarkdownDocumentSnapshot is an immutable view of a MarkdownDocument.
// Text is deliberately excluded — handlers never need the full markdown content.
type MarkdownDocumentSnapshot struct {
	URI       string
	Version   int
	Blocks    []markdown.CodeBlock
	Snapshots []*analysis.Snapshot
}

// BlockPosition maps a markdown position to a specific block.
// Returned as a pointer from MarkdownPositionToBlock; nil means outside all blocks.
type BlockPosition struct {
	BlockIndex int
	LocalLine  int
	LocalChar  int
}

// MarkdownPositionToBlock converts a markdown position to block-local coordinates.
// Only line numbers are adjusted; character offsets pass through unchanged.
// When PrefixLines > 0, the local line is shifted to account for synthetic prefix
// content prepended during analysis (e.g., a synthetic schema declaration).
func (snap *MarkdownDocumentSnapshot) MarkdownPositionToBlock(line, char int) *BlockPosition {
	for i, block := range snap.Blocks {
		contentEndLine := block.EndLine - 1
		if line >= block.StartLine && line <= contentEndLine {
			return &BlockPosition{
				BlockIndex: i,
				LocalLine:  line - block.StartLine + block.PrefixLines,
				LocalChar:  char,
			}
		}
	}
	return nil
}

// BlockPositionToMarkdown converts block-local coordinates to markdown position.
// Only line numbers are adjusted; character offsets pass through unchanged.
// When PrefixLines > 0, the local line is shifted back to account for synthetic
// prefix content, converting from prefixed-content coordinates to markdown coordinates.
func (snap *MarkdownDocumentSnapshot) BlockPositionToMarkdown(blockIndex, localLine, localChar int) (int, int) {
	if blockIndex < 0 || blockIndex >= len(snap.Blocks) {
		return -1, -1
	}
	return snap.Blocks[blockIndex].StartLine + localLine - snap.Blocks[blockIndex].PrefixLines, localChar
}

// buildBlockDocumentSnapshot creates a DocumentSnapshot for a single code block
// within a markdown document. This is the shared utility used by all feature
// providers (hover, completion, definition, symbols) to bridge between
// markdown-level state and block-level analysis.
//
// URI and SourceID intentionally differ: URI is the markdown file URI (for
// display/logging), while SourceID is the virtual block identifier (for source
// registry lookups in position conversion).
func (s *Server) buildBlockDocumentSnapshot(mdSnap *MarkdownDocumentSnapshot, block markdown.CodeBlock) *DocumentSnapshot {
	depths, inComment := ComputeBraceDepths(block.Content)
	return &DocumentSnapshot{
		URI:      mdSnap.URI,
		SourceID: block.SourceID,
		Version:  mdSnap.Version,
		Text:     block.Content,
		LineState: &LineState{
			Version:        mdSnap.Version,
			BraceDepth:     depths,
			InBlockComment: inComment,
		},
	}
}
