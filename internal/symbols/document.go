package symbols

import (
	"fmt"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm/source"
)

// toUInteger safely converts an int to protocol.UInteger (uint32).
// Negative values are clamped to 0.
func toUInteger(n int) protocol.UInteger {
	if n < 0 {
		return 0
	}
	return protocol.UInteger(n) //nolint:gosec // clamped to non-negative
}

// SymbolParentKey creates a unique key for parent lookup to prevent name collisions.
// Uses kind:name format to ensure types and schemas with the same name don't collide.
func SymbolParentKey(kind SymbolKind, name string) string {
	return fmt.Sprintf("%d:%s", kind, name)
}

// DocSymWithKey wraps a DocumentSymbol with its parent key for hierarchical building.
type DocSymWithKey struct {
	Sym protocol.DocumentSymbol
	Key string // composite key (kind:name) for looking up children
}

// BuildDocumentSymbols converts a SymbolIndex to hierarchical DocumentSymbols.
// The sources parameter provides source content for position encoding conversion.
// The enc parameter is the negotiated LSP position encoding (UTF-16 or UTF-8).
func BuildDocumentSymbols(idx *SymbolIndex, sources *source.Registry, enc lsputil.PositionEncoding) []protocol.DocumentSymbol {
	if idx == nil || len(idx.Symbols) == 0 {
		return nil
	}

	// Group symbols by parent using composite key (kind:name) to prevent collisions
	// when a schema name equals a type name.

	// First pass: identify top-level symbols (schema, imports, types, datatypes)
	var topLevel []DocSymWithKey
	childrenByParent := make(map[string][]DocSymWithKey)

	// Find the schema name and build its key - first pass for order-independent detection
	var schemaKey string
	var hasSchemaSymbol bool
	for i := range idx.Symbols {
		sym := &idx.Symbols[i]
		if sym.Kind == SymbolSchema {
			schemaKey = SymbolParentKey(SymbolSchema, sym.Name)
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
			if idx.Symbols[i].Kind == SymbolImport {
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
		schemaKey = SymbolParentKey(SymbolSchema, syntheticSchemaName)
		detail := "parse error"
		topLevel = append(topLevel, DocSymWithKey{
			Sym: protocol.DocumentSymbol{
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
			Key: schemaKey,
		})
	}

	// Process all symbols
	for i := range idx.Symbols {
		sym := &idx.Symbols[i]
		docSym := SymbolToDocumentSymbol(sym, sources, enc)
		symKey := SymbolParentKey(sym.Kind, sym.Name)

		switch sym.Kind {
		case SymbolSchema:
			// Schema is always top-level, will contain imports
			topLevel = append(topLevel, DocSymWithKey{Sym: docSym, Key: symKey})

		case SymbolImport:
			// Imports are children of schema (real or synthetic)
			if schemaKey != "" {
				childrenByParent[schemaKey] = append(childrenByParent[schemaKey], DocSymWithKey{Sym: docSym, Key: symKey})
			} else {
				topLevel = append(topLevel, DocSymWithKey{Sym: docSym, Key: symKey})
			}

		case SymbolType, SymbolDataType:
			// Types and datatypes are children of schema (real or synthetic in error states).
			// In synthetic mode, all types are nested under the synthetic root for stability.
			switch {
			case useSyntheticSchema:
				// Synthetic mode: nest all types under synthetic schema
				childrenByParent[schemaKey] = append(childrenByParent[schemaKey], DocSymWithKey{Sym: docSym, Key: symKey})
			case schemaKey != "":
				// Real schema exists: nest types that belong to it
				parentKey := SymbolParentKey(SymbolSchema, sym.ParentName)
				if parentKey == schemaKey {
					childrenByParent[schemaKey] = append(childrenByParent[schemaKey], DocSymWithKey{Sym: docSym, Key: symKey})
				} else {
					topLevel = append(topLevel, DocSymWithKey{Sym: docSym, Key: symKey})
				}
			default:
				topLevel = append(topLevel, DocSymWithKey{Sym: docSym, Key: symKey})
			}

		case SymbolProperty, SymbolAssociation, SymbolComposition, SymbolInvariant:
			// These are children of their parent type
			if sym.ParentName != "" {
				// Parent is a type, not a schema
				parentKey := SymbolParentKey(SymbolType, sym.ParentName)
				childrenByParent[parentKey] = append(childrenByParent[parentKey], DocSymWithKey{Sym: docSym, Key: symKey})
			}
		}
	}

	// Second pass: attach children to parents
	result := AttachChildren(topLevel, childrenByParent)

	return result
}

// AttachChildren recursively attaches children to parent symbols.
func AttachChildren(syms []DocSymWithKey, childrenByParent map[string][]DocSymWithKey) []protocol.DocumentSymbol {
	result := make([]protocol.DocumentSymbol, len(syms))

	for i, dsym := range syms {
		result[i] = dsym.Sym
		if children, ok := childrenByParent[dsym.Key]; ok {
			// Recursively attach children to these children
			attachedChildren := AttachChildren(children, childrenByParent)
			result[i].Children = attachedChildren
		}
	}

	return result
}

// SymbolToDocumentSymbol converts a Symbol to a protocol.DocumentSymbol.
// The sources parameter provides source content for position encoding conversion.
// The enc parameter is the negotiated LSP position encoding (UTF-16 or UTF-8).
func SymbolToDocumentSymbol(sym *Symbol, sources *source.Registry, enc lsputil.PositionEncoding) protocol.DocumentSymbol {
	kind := SymbolKindToLSP(sym.Kind)

	// Build detail string
	detail := sym.Detail
	if detail == "" {
		detail = sym.Kind.String()
	}

	// Convert spans using proper UTF-16 conversion
	rangeStart, rangeEnd, rangeOk := lsputil.SpanToLSPRange(sources, sym.Range, enc)
	selStart, selEnd, selOk := lsputil.SpanToLSPRange(sources, sym.Selection, enc)

	// Build ranges (fallback to naive conversion if needed)
	var symRange, selRange protocol.Range
	if rangeOk {
		symRange = protocol.Range{
			Start: protocol.Position{Line: toUInteger(rangeStart[0]), Character: toUInteger(rangeStart[1])},
			End:   protocol.Position{Line: toUInteger(rangeEnd[0]), Character: toUInteger(rangeEnd[1])},
		}
	} else {
		symRange = protocol.Range{
			Start: protocol.Position{
				Line:      toUInteger(sym.Range.Start.Line - 1),
				Character: toUInteger(sym.Range.Start.Column - 1),
			},
			End: protocol.Position{
				Line:      toUInteger(sym.Range.End.Line - 1),
				Character: toUInteger(sym.Range.End.Column - 1),
			},
		}
	}

	if selOk {
		selRange = protocol.Range{
			Start: protocol.Position{Line: toUInteger(selStart[0]), Character: toUInteger(selStart[1])},
			End:   protocol.Position{Line: toUInteger(selEnd[0]), Character: toUInteger(selEnd[1])},
		}
	} else {
		selRange = protocol.Range{
			Start: protocol.Position{
				Line:      toUInteger(sym.Selection.Start.Line - 1),
				Character: toUInteger(sym.Selection.Start.Column - 1),
			},
			End: protocol.Position{
				Line:      toUInteger(sym.Selection.End.Line - 1),
				Character: toUInteger(sym.Selection.End.Column - 1),
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

// SymbolKindToLSP converts a SymbolKind to LSP SymbolKind.
func SymbolKindToLSP(kind SymbolKind) protocol.SymbolKind {
	switch kind {
	case SymbolSchema:
		return protocol.SymbolKindModule
	case SymbolImport:
		return protocol.SymbolKindPackage
	case SymbolType:
		return protocol.SymbolKindClass
	case SymbolDataType:
		return protocol.SymbolKindTypeParameter
	case SymbolProperty:
		return protocol.SymbolKindField
	case SymbolAssociation:
		return protocol.SymbolKindProperty
	case SymbolComposition:
		return protocol.SymbolKindProperty
	case SymbolInvariant:
		return protocol.SymbolKindEvent
	default:
		return protocol.SymbolKindVariable
	}
}
