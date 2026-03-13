package lsp

import (
	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/markdown"
)

// markdownDocument tracks code blocks in an open markdown file.
// This is workspace-internal mutable state — server handlers must NOT
// access this type directly. Use markdownDocumentSnapshot (obtained via
// GetMarkdownDocumentSnapshot) for safe concurrent reads.
//
// ATOMICITY INVARIANT: Blocks and Snapshots must only be replaced together,
// atomically under the workspace lock, by analyzeMarkdownAndPublish.
type markdownDocument struct {
	URI       string
	Version   int
	Text      string
	Blocks    []markdown.CodeBlock
	Snapshots []*analysis.Snapshot
}

// markdownDocumentSnapshot is an immutable view of a markdownDocument.
// Text is deliberately excluded — handlers never need the full markdown content.
type markdownDocumentSnapshot struct {
	URI       string
	Version   int
	Blocks    []markdown.CodeBlock
	Snapshots []*analysis.Snapshot
}

// blockPosition maps a markdown position to a specific block.
// Returned as a pointer from MarkdownPositionToBlock; nil means outside all blocks.
type blockPosition struct {
	BlockIndex int
	LocalLine  int
	LocalChar  int
}

// MarkdownPositionToBlock converts a markdown position to block-local coordinates.
// Only line numbers are adjusted; character offsets pass through unchanged.
// When PrefixLines > 0, the local line is shifted to account for synthetic prefix
// content prepended during analysis (e.g., a synthetic schema declaration).
func (snap *markdownDocumentSnapshot) MarkdownPositionToBlock(line, char int) *blockPosition {
	for i, block := range snap.Blocks {
		contentEndLine := block.EndLine - 1
		if line >= block.StartLine && line <= contentEndLine {
			return &blockPosition{
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
func (snap *markdownDocumentSnapshot) BlockPositionToMarkdown(blockIndex, localLine, localChar int) (int, int) {
	if blockIndex < 0 || blockIndex >= len(snap.Blocks) {
		return -1, -1
	}
	return snap.Blocks[blockIndex].StartLine + localLine - snap.Blocks[blockIndex].PrefixLines, localChar
}

// buildBlockDocumentSnapshot creates a documentSnapshot for a single code block
// within a markdown document. This is the shared utility used by all feature
// providers (hover, completion, definition, symbols) to bridge between
// markdown-level state and block-level analysis.
//
// URI and SourceID intentionally differ: URI is the markdown file URI (for
// display/logging), while SourceID is the virtual block identifier (for source
// registry lookups in position conversion).
func (s *Server) buildBlockDocumentSnapshot(mdSnap *markdownDocumentSnapshot, block markdown.CodeBlock) *documentSnapshot {
	depths, inComment := computeBraceDepths(block.Content)
	return &documentSnapshot{
		URI:      mdSnap.URI,
		SourceID: block.SourceID,
		Version:  mdSnap.Version,
		Text:     block.Content,
		lineState: &lineState{
			Version:        mdSnap.Version,
			BraceDepth:     depths,
			InBlockComment: inComment,
		},
	}
}
