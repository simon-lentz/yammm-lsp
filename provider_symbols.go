package lsp

import (
	"context"
	"fmt"
	"time"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/symbols"
)

// textDocumentDocumentSymbol handles textDocument/documentSymbol requests.
//
//nolint:nilnil // LSP protocol: nil result means no symbols
func (s *Server) textDocumentDocumentSymbol(_ context.Context, params *protocol.DocumentSymbolParams) (any, error) {
	defer s.logTiming("textDocument/documentSymbol", time.Now())
	uri := params.TextDocument.URI

	s.logger.Debug("documentSymbol request", "uri", uri)

	if mdSnap := s.workspace.GetMarkdownDocumentSnapshot(uri); mdSnap != nil {
		return s.markdownDocumentSymbols(mdSnap), nil
	}

	snapshot := s.workspace.LatestSnapshot(uri)
	if snapshot == nil {
		return nil, nil
	}

	doc := s.workspace.GetDocumentSnapshot(uri)
	if doc == nil {
		return nil, nil
	}

	symbols := s.documentSymbolsFor(snapshot, doc)
	return symbols, nil
}

// markdownDocumentSymbols returns document symbols from all code blocks in a markdown file.
// Unlike cursor-centric handlers, this iterates all blocks and aggregates symbols.
func (s *Server) markdownDocumentSymbols(mdSnap *markdownDocumentSnapshot) []protocol.DocumentSymbol {
	var allSymbols []protocol.DocumentSymbol

	for i, snapshot := range mdSnap.Snapshots {
		if snapshot == nil || i >= len(mdSnap.Blocks) {
			continue
		}

		blockDocSnap := s.buildBlockDocumentSnapshot(mdSnap, mdSnap.Blocks[i])
		symbols := s.documentSymbolsFor(snapshot, blockDocSnap)
		if len(symbols) > 0 {
			remapped := remapDocumentSymbolRanges(symbols, mdSnap, i)
			allSymbols = append(allSymbols, remapped...)
		}
	}

	return allSymbols
}

// remapDocumentSymbolRanges remaps Range and SelectionRange of document symbols
// from block-local coordinates to markdown coordinates. Recursively processes Children.
// Returns nil for empty input. Creates copies to avoid mutating the originals.
func remapDocumentSymbolRanges(symbols []protocol.DocumentSymbol, mdSnap *markdownDocumentSnapshot, blockIndex int) []protocol.DocumentSymbol {
	if len(symbols) == 0 {
		return nil
	}

	result := make([]protocol.DocumentSymbol, len(symbols))
	for i, sym := range symbols {
		result[i] = sym

		// Remap Range
		startLine, startChar := mdSnap.BlockPositionToMarkdown(blockIndex,
			int(sym.Range.Start.Line), int(sym.Range.Start.Character))
		endLine, endChar := mdSnap.BlockPositionToMarkdown(blockIndex,
			int(sym.Range.End.Line), int(sym.Range.End.Character))
		result[i].Range = protocol.Range{
			Start: protocol.Position{Line: analysis.ToUInteger(startLine), Character: analysis.ToUInteger(startChar)},
			End:   protocol.Position{Line: analysis.ToUInteger(endLine), Character: analysis.ToUInteger(endChar)},
		}

		// Remap SelectionRange
		selStartLine, selStartChar := mdSnap.BlockPositionToMarkdown(blockIndex,
			int(sym.SelectionRange.Start.Line), int(sym.SelectionRange.Start.Character))
		selEndLine, selEndChar := mdSnap.BlockPositionToMarkdown(blockIndex,
			int(sym.SelectionRange.End.Line), int(sym.SelectionRange.End.Character))
		result[i].SelectionRange = protocol.Range{
			Start: protocol.Position{Line: analysis.ToUInteger(selStartLine), Character: analysis.ToUInteger(selStartChar)},
			End:   protocol.Position{Line: analysis.ToUInteger(selEndLine), Character: analysis.ToUInteger(selEndChar)},
		}

		// Recursively remap children
		if len(sym.Children) > 0 {
			result[i].Children = remapDocumentSymbolRanges(sym.Children, mdSnap, blockIndex)
		}
	}

	return result
}

// documentSymbolsFor returns document symbols for the given document within a snapshot.
// Returns nil when no symbols are available.
func (s *Server) documentSymbolsFor(snapshot *analysis.Snapshot, doc *documentSnapshot) []protocol.DocumentSymbol {
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

	return s.buildDocumentSymbols(idx, snapshot)
}

// symbolParentKey creates a unique key for parent lookup to prevent name collisions.
// Uses kind:name format to ensure types and schemas with the same name don't collide.
func symbolParentKey(kind symbols.SymbolKind, name string) string {
	return fmt.Sprintf("%d:%s", kind, name)
}

// docSymWithKey wraps a DocumentSymbol with its parent key for hierarchical building.
type docSymWithKey struct {
	sym protocol.DocumentSymbol
	key string // composite key (kind:name) for looking up children
}

// buildDocumentSymbols converts a SymbolIndex to hierarchical DocumentSymbols.
func (s *Server) buildDocumentSymbols(idx *symbols.SymbolIndex, snapshot *analysis.Snapshot) []protocol.DocumentSymbol {
	if idx == nil || len(idx.Symbols) == 0 {
		return nil
	}

	// Group symbols by parent using composite key (kind:name) to prevent collisions
	// when a schema name equals a type name.

	// First pass: identify top-level symbols (schema, imports, types, datatypes)
	var topLevel []docSymWithKey
	childrenByParent := make(map[string][]docSymWithKey)

	// Find the schema name and build its key - first pass for order-independent detection
	var schemaKey string
	var hasSchemaSymbol bool
	for i := range idx.Symbols {
		sym := &idx.Symbols[i]
		if sym.Kind == symbols.SymbolSchema {
			schemaKey = symbolParentKey(symbols.SymbolSchema, sym.Name)
			hasSchemaSymbol = true
			break
		}
	}

	// Check for orphan imports (only if no schema found) - second pass.
	// Orphan imports indicate a parse error where the schema declaration failed
	// but imports were successfully extracted. In this case, create a synthetic
	// root to keep the outline stable.
	var hasOrphanImports bool
	var useSyntheticSchema bool
	if !hasSchemaSymbol {
		for i := range idx.Symbols {
			if idx.Symbols[i].Kind == symbols.SymbolImport {
				hasOrphanImports = true
				break
			}
		}
		useSyntheticSchema = hasOrphanImports
	}

	// If no schema symbol exists but there are orphan imports, create a synthetic
	// root to maintain consistent hierarchy in the document outline. When synthetic
	// schema is created, ALL orphan symbols (imports, types, datatypes) are nested
	// under it to keep the outline stable and predictable during partial parses.
	const syntheticSchemaName = "(schema)"
	if useSyntheticSchema {
		schemaKey = symbolParentKey(symbols.SymbolSchema, syntheticSchemaName)
		detail := "parse error"
		topLevel = append(topLevel, docSymWithKey{
			sym: protocol.DocumentSymbol{
				Name:   syntheticSchemaName,
				Detail: &detail,
				Kind:   protocol.SymbolKindModule,
				Range: protocol.Range{
					Start: protocol.Position{Line: 0, Character: 0},
					End:   protocol.Position{Line: 0, Character: 0},
				},
				SelectionRange: protocol.Range{
					Start: protocol.Position{Line: 0, Character: 0},
					End:   protocol.Position{Line: 0, Character: 0},
				},
			},
			key: schemaKey,
		})
	}

	// Process all symbols
	for i := range idx.Symbols {
		sym := &idx.Symbols[i]
		docSym := s.symbolToDocumentSymbol(sym, snapshot)
		symKey := symbolParentKey(sym.Kind, sym.Name)

		switch sym.Kind {
		case symbols.SymbolSchema:
			// Schema is always top-level, will contain imports
			topLevel = append(topLevel, docSymWithKey{sym: docSym, key: symKey})

		case symbols.SymbolImport:
			// Imports are children of schema (real or synthetic)
			if schemaKey != "" {
				childrenByParent[schemaKey] = append(childrenByParent[schemaKey], docSymWithKey{sym: docSym, key: symKey})
			} else {
				topLevel = append(topLevel, docSymWithKey{sym: docSym, key: symKey})
			}

		case symbols.SymbolType, symbols.SymbolDataType:
			// Types and datatypes are children of schema (real or synthetic in error states).
			// In synthetic mode, all types are nested under the synthetic root for stability.
			switch {
			case useSyntheticSchema:
				// Synthetic mode: nest all types under synthetic schema
				childrenByParent[schemaKey] = append(childrenByParent[schemaKey], docSymWithKey{sym: docSym, key: symKey})
			case schemaKey != "":
				// Real schema exists: nest types that belong to it
				parentKey := symbolParentKey(symbols.SymbolSchema, sym.ParentName)
				if parentKey == schemaKey {
					childrenByParent[schemaKey] = append(childrenByParent[schemaKey], docSymWithKey{sym: docSym, key: symKey})
				} else {
					topLevel = append(topLevel, docSymWithKey{sym: docSym, key: symKey})
				}
			default:
				topLevel = append(topLevel, docSymWithKey{sym: docSym, key: symKey})
			}

		case symbols.SymbolProperty, symbols.SymbolAssociation, symbols.SymbolComposition, symbols.SymbolInvariant:
			// These are children of their parent type
			if sym.ParentName != "" {
				// Parent is a type, not a schema
				parentKey := symbolParentKey(symbols.SymbolType, sym.ParentName)
				childrenByParent[parentKey] = append(childrenByParent[parentKey], docSymWithKey{sym: docSym, key: symKey})
			}
		}
	}

	// Second pass: attach children to parents
	result := attachChildren(topLevel, childrenByParent)

	return result
}

// attachChildren recursively attaches children to parent symbols.
func attachChildren(symbols []docSymWithKey, childrenByParent map[string][]docSymWithKey) []protocol.DocumentSymbol {
	result := make([]protocol.DocumentSymbol, len(symbols))

	for i, dsym := range symbols {
		result[i] = dsym.sym
		if children, ok := childrenByParent[dsym.key]; ok {
			// Recursively attach children to these children
			attachedChildren := attachChildren(children, childrenByParent)
			result[i].Children = attachedChildren
		}
	}

	return result
}

// symbolToDocumentSymbol converts a Symbol to a protocol.DocumentSymbol.
func (s *Server) symbolToDocumentSymbol(sym *symbols.Symbol, snapshot *analysis.Snapshot) protocol.DocumentSymbol {
	kind := symbolKindToLSP(sym.Kind)

	// Build detail string
	detail := sym.Detail
	if detail == "" {
		detail = sym.Kind.String()
	}

	// Convert spans using proper UTF-16 conversion
	enc := s.workspace.PositionEncoding()

	rangeStart, rangeEnd, rangeOk := lsputil.SpanToLSPRange(snapshot.Sources, sym.Range, enc)
	selStart, selEnd, selOk := lsputil.SpanToLSPRange(snapshot.Sources, sym.Selection, enc)

	// Build ranges (fallback to naive conversion if needed)
	var symRange, selRange protocol.Range
	if rangeOk {
		symRange = protocol.Range{
			Start: protocol.Position{Line: analysis.ToUInteger(rangeStart[0]), Character: analysis.ToUInteger(rangeStart[1])},
			End:   protocol.Position{Line: analysis.ToUInteger(rangeEnd[0]), Character: analysis.ToUInteger(rangeEnd[1])},
		}
	} else {
		symRange = protocol.Range{
			Start: protocol.Position{
				Line:      analysis.ToUInteger(sym.Range.Start.Line - 1),
				Character: analysis.ToUInteger(sym.Range.Start.Column - 1),
			},
			End: protocol.Position{
				Line:      analysis.ToUInteger(sym.Range.End.Line - 1),
				Character: analysis.ToUInteger(sym.Range.End.Column - 1),
			},
		}
	}

	if selOk {
		selRange = protocol.Range{
			Start: protocol.Position{Line: analysis.ToUInteger(selStart[0]), Character: analysis.ToUInteger(selStart[1])},
			End:   protocol.Position{Line: analysis.ToUInteger(selEnd[0]), Character: analysis.ToUInteger(selEnd[1])},
		}
	} else {
		selRange = protocol.Range{
			Start: protocol.Position{
				Line:      analysis.ToUInteger(sym.Selection.Start.Line - 1),
				Character: analysis.ToUInteger(sym.Selection.Start.Column - 1),
			},
			End: protocol.Position{
				Line:      analysis.ToUInteger(sym.Selection.End.Line - 1),
				Character: analysis.ToUInteger(sym.Selection.End.Column - 1),
			},
		}
	}

	return protocol.DocumentSymbol{
		Name:           sym.Name,
		Detail:         &detail,
		Kind:           kind,
		Range:          symRange,
		SelectionRange: selRange,
	}
}

// symbolKindToLSP converts our SymbolKind to LSP SymbolKind.
func symbolKindToLSP(kind symbols.SymbolKind) protocol.SymbolKind {
	switch kind {
	case symbols.SymbolSchema:
		return protocol.SymbolKindModule
	case symbols.SymbolImport:
		return protocol.SymbolKindPackage
	case symbols.SymbolType:
		return protocol.SymbolKindClass
	case symbols.SymbolDataType:
		return protocol.SymbolKindTypeParameter
	case symbols.SymbolProperty:
		return protocol.SymbolKindField
	case symbols.SymbolAssociation:
		return protocol.SymbolKindProperty
	case symbols.SymbolComposition:
		return protocol.SymbolKindProperty
	case symbols.SymbolInvariant:
		return protocol.SymbolKindEvent
	default:
		return protocol.SymbolKindVariable
	}
}
