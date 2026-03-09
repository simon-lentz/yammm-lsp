package lsp

// This file provides package-level forwarding for position conversion functions
// that were moved to internal/lsputil. These thin wrappers allow existing code
// in the lsp package (and its tests) to call the functions without a prefix.

import (
	"bytes"

	"github.com/simon-lentz/yammm/location"
	"github.com/simon-lentz/yammm/source"

	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
)

// ByteOffsetFromLSP delegates to lsputil.ByteOffsetFromLSP.
func ByteOffsetFromLSP(sources *source.Registry, id location.SourceID, line, char int, enc PositionEncoding) (int, bool) {
	return lsputil.ByteOffsetFromLSP(sources, id, line, char, enc)
}

// PositionFromLSP delegates to lsputil.PositionFromLSP.
func PositionFromLSP(
	sources *source.Registry,
	sourceID location.SourceID,
	lspLine, lspChar int,
	enc PositionEncoding,
) (location.Position, bool) {
	return lsputil.PositionFromLSP(sources, sourceID, lspLine, lspChar, enc)
}

// ByteToUTF16Offset delegates to lsputil.ByteToUTF16Offset.
func ByteToUTF16Offset(content []byte, lineStart, targetByte int) int {
	return lsputil.ByteToUTF16Offset(content, lineStart, targetByte)
}

// SpanToLSPRange delegates to lsputil.SpanToLSPRange.
func SpanToLSPRange(sources *source.Registry, span location.Span, enc PositionEncoding) (start, end [2]int, ok bool) {
	return lsputil.SpanToLSPRange(sources, span, enc)
}

// hasURIScheme delegates to lsputil.HasURIScheme.
// Kept unexported for backward compatibility with existing test code.
func hasURIScheme(s string) bool {
	return lsputil.HasURIScheme(s)
}

// utf16CharToByteOffset delegates to lsputil.UTF16CharToByteOffset.
// Kept unexported for backward compatibility with existing code.
func utf16CharToByteOffset(content []byte, lineStart, charOffset int) int { //nolint:unparam // lineStart is part of the API contract
	return lsputil.UTF16CharToByteOffset(content, lineStart, charOffset)
}

// clampToLineEnd is a thin wrapper mirroring lsputil's unexported clampToLineEnd.
// Provided for test backward compatibility only.
func clampToLineEnd(content []byte, lineStart, offset int) int {
	if offset < lineStart {
		return lineStart
	}
	lineContent := content[lineStart:]
	if idx := bytes.IndexByte(lineContent, '\n'); idx >= 0 {
		lineEnd := lineStart + idx
		if offset > lineEnd {
			return lineEnd
		}
	} else if offset > len(content) {
		return len(content)
	}
	return offset
}
