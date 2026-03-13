package lsp

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	protocol "github.com/simon-lentz/yammm-lsp/internal/protocol"

	"github.com/simon-lentz/yammm/location"
	"github.com/simon-lentz/yammm/schema"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/completion"
	"github.com/simon-lentz/yammm-lsp/internal/docstate"
	"github.com/simon-lentz/yammm-lsp/internal/lsputil"
	"github.com/simon-lentz/yammm-lsp/internal/symbols"
)

// builtinTypes are the built-in type keywords available for property types.
var builtinTypes = []string{
	"Boolean",
	"Date",
	"Enum",
	"Float",
	"Integer",
	"List",
	"Pattern",
	"String",
	"Timestamp",
	"UUID",
	"Vector",
}

// textDocumentCompletion handles textDocument/completion requests.
//
//nolint:nilnil // LSP protocol: nil result means no completions
func (s *Server) textDocumentCompletion(_ context.Context, params *protocol.CompletionParams) (any, error) {
	defer s.logTiming("textDocument/completion", time.Now())
	uri := params.TextDocument.URI

	s.logger.Debug("completion request",
		"uri", uri,
		"line", params.Position.Line,
		"character", params.Position.Character,
	)

	unit := s.workspace.ResolveUnit(uri, int(params.Position.Line), int(params.Position.Character), false)
	if unit == nil {
		return nil, nil
	}

	return s.completionAtPosition(unit.Snapshot, unit.Doc, unit.LocalLine, unit.LocalChar), nil
}

// completionAtPosition returns completion items for the given position.
// snapshot may be nil — graceful degradation provides keywords, snippets,
// and built-in types without a schema.
// The line and char parameters are LSP-encoding coordinates.
func (s *Server) completionAtPosition(snapshot *analysis.Snapshot, doc *docstate.Snapshot, line, char int) any {
	if snapshot != nil && snapshot.EntryVersion != doc.Version {
		s.logger.Debug("serving stale snapshot for completion",
			"uri", doc.URI,
			"snapshot_version", snapshot.EntryVersion,
			"doc_version", doc.Version,
		)
	}

	var byteOffset int
	usedRegistry := false
	if snapshot != nil && snapshot.Sources != nil {
		if offset, ok := lsputil.ByteOffsetFromLSP(
			snapshot.Sources,
			doc.SourceID,
			line,
			char,
			s.workspace.PositionEncoding(),
		); ok {
			byteOffset = offset
			usedRegistry = true
			lineStart, lineOk := snapshot.Sources.LineStartByte(doc.SourceID, line+1)
			if lineOk {
				byteOffset -= lineStart
				if byteOffset < 0 {
					byteOffset = 0
				}
			}
		}
	}
	if !usedRegistry {
		byteOffset = s.computeByteOffsetFromText(doc.Text, line, char)
	}

	ctx := completion.DetectContext(doc, line, byteOffset)

	s.logger.Debug("completion context", "context", ctx)

	var items []protocol.CompletionItem

	switch ctx {
	case completion.TopLevel:
		items = s.topLevelCompletions()
	case completion.TypeBody:
		items = s.typeBodyCompletions()
	case completion.Extends:
		items = s.typeCompletions(snapshot, doc.SourceID)
	case completion.PropertyType:
		items = s.propertyTypeCompletions(snapshot, doc.SourceID)
	case completion.RelationTarget:
		items = s.typeCompletions(snapshot, doc.SourceID)
	case completion.ImportPath:
		items = importCompletions()
	default:
		items = s.topLevelCompletions()
	}

	slices.SortFunc(items, func(a, b protocol.CompletionItem) int {
		if a.SortText != nil && b.SortText != nil {
			return cmp.Compare(*a.SortText, *b.SortText)
		}
		return cmp.Compare(a.Label, b.Label)
	})

	return items
}

// computeByteOffsetFromText computes a byte offset within a line from document text.
// This is used when no source registry is available (before first analysis).
// It respects the negotiated position encoding (UTF-16 or UTF-8).
func (s *Server) computeByteOffsetFromText(text string, lspLine, lspChar int) int {
	lines := strings.Split(text, "\n")
	if lspLine >= len(lines) {
		return lspChar // fallback
	}
	return lsputil.CharToByteOnLine([]byte(lines[lspLine]), lspChar, s.workspace.PositionEncoding())
}

// topLevelCompletions returns completions for top-level context.
func (s *Server) topLevelCompletions() []protocol.CompletionItem {
	items := []protocol.CompletionItem{
		keywordCompletion("schema", "schema \"${1:name}\"", "Schema declaration"),
		keywordCompletion("import", "import \"${1:./path}\"${2: as ${3:alias}}", "Import statement"),
		keywordCompletion("type", "type ${1:Name} {\n\t$0\n}", "Type declaration"),
		keywordCompletion("abstract type", "abstract type ${1:Name} {\n\t$0\n}", "Abstract type declaration"),
		keywordCompletion("part type", "part type ${1:Name} {\n\t$0\n}", "Part type declaration"),
	}

	// Add datatype completions (3.6)
	items = append(items, keywordCompletion("datatype", "type ${1:Name} = ${2|String,Integer,Float,Boolean,UUID,Date,Timestamp,Enum,Pattern|}", "Datatype alias"))
	items = append(items, keywordCompletion("datatype with constraint", "type ${1:Name} = ${2|String,Integer|}[${3:min}, ${4:max}]", "Datatype alias with numeric constraints"))

	return items
}

// typeBodyCompletions returns completions for inside a type body.
func (s *Server) typeBodyCompletions() []protocol.CompletionItem {
	items := []protocol.CompletionItem{
		// Property snippets - modifiers are space-separated per grammar (only 'primary' or 'required')
		// Format: ${N|, modifier1, modifier2|} - empty first option, space-prefixed subsequent
		snippetCompletion("property", "${1:name} ${2|String,Integer,Float,Boolean,UUID|}${3|, required, primary|}", "Property declaration"),
		snippetCompletion("property with constraint", "${1:name} ${2:String}[${3:1}, ${4:100}]${5|, required|}", "Property with constraint"),

		// Relation snippets
		snippetCompletion("association", "--> ${1:NAME} (${2|one,many|}) ${3:TargetType}", "Association declaration"),
		snippetCompletion("composition", "*-> ${1:CONTAINS} (${2|one,many|}) ${3:PartType}", "Composition declaration"),

		// Invariant snippet
		snippetCompletion("invariant", "! \"${1:message}\" ${2:expression}", "Invariant declaration"),
	}

	// Add built-in types for quick access
	for _, t := range builtinTypes {
		sortText := "2_" + t
		kind := protocol.CompletionItemKindTypeParameter
		items = append(items, protocol.CompletionItem{
			Label:    t,
			Kind:     &kind,
			SortText: &sortText,
			Detail:   new("Built-in type"),
		})
	}

	return items
}

// typeCompletions returns type name completions.
// sourceID should be the canonical (symlink-resolved) SourceID from the document.
func (s *Server) typeCompletions(snapshot *analysis.Snapshot, sourceID location.SourceID) []protocol.CompletionItem {
	items := make([]protocol.CompletionItem, 0)

	if snapshot == nil || snapshot.Schema == nil {
		return items
	}

	// Add local types
	for name := range snapshot.Schema.Types() {
		sortText := "0_" + name // Local types first
		kind := protocol.CompletionItemKindClass
		items = append(items, protocol.CompletionItem{
			Label:    name,
			Kind:     &kind,
			SortText: &sortText,
			Detail:   new("Type"),
		})
	}

	// Add imported types with qualifier
	idx := snapshot.SymbolIndexAt(sourceID)
	if idx != nil {
		for i := range idx.Symbols {
			sym := &idx.Symbols[i]
			if sym.Kind == symbols.SymbolImport {
				imp, ok := sym.Data.(*schema.Import)
				if !ok || imp.Schema() == nil {
					continue
				}

				alias := imp.Alias()
				for typeName := range imp.Schema().Types() {
					qualifiedName := alias + "." + typeName
					sortText := "1_" + qualifiedName // Imported types second
					kind := protocol.CompletionItemKindClass
					items = append(items, protocol.CompletionItem{
						Label:    qualifiedName,
						Kind:     &kind,
						SortText: &sortText,
						Detail:   new("Imported type from " + alias),
					})
				}
			}
		}
	}

	return items
}

// propertyTypeCompletions returns completions for property type position.
// sourceID should be the canonical (symlink-resolved) SourceID from the document.
func (s *Server) propertyTypeCompletions(snapshot *analysis.Snapshot, sourceID location.SourceID) []protocol.CompletionItem {
	var items []protocol.CompletionItem

	// Add built-in types first
	for _, t := range builtinTypes {
		sortText := "0_" + t
		kind := protocol.CompletionItemKindTypeParameter
		items = append(items, protocol.CompletionItem{
			Label:    t,
			Kind:     &kind,
			SortText: &sortText,
			Detail:   new("Built-in type"),
		})
	}

	if snapshot == nil || snapshot.Schema == nil {
		return items
	}

	// Add local datatypes
	for name := range snapshot.Schema.DataTypes() {
		sortText := "1_" + name
		kind := protocol.CompletionItemKindTypeParameter
		items = append(items, protocol.CompletionItem{
			Label:    name,
			Kind:     &kind,
			SortText: &sortText,
			Detail:   new("Datatype"),
		})
	}

	// Add imported datatypes with qualifier
	idx := snapshot.SymbolIndexAt(sourceID)
	if idx != nil {
		for i := range idx.Symbols {
			sym := &idx.Symbols[i]
			if sym.Kind == symbols.SymbolImport {
				imp, ok := sym.Data.(*schema.Import)
				if !ok || imp.Schema() == nil {
					continue
				}

				alias := imp.Alias()
				for dtName := range imp.Schema().DataTypes() {
					qualifiedName := alias + "." + dtName
					sortText := "2_" + qualifiedName
					kind := protocol.CompletionItemKindTypeParameter
					items = append(items, protocol.CompletionItem{
						Label:    qualifiedName,
						Kind:     &kind,
						SortText: &sortText,
						Detail:   new("Imported datatype from " + alias),
					})
				}
			}
		}
	}

	return items
}

// importCompletions returns completions for import context.
func importCompletions() []protocol.CompletionItem {
	// Use optional alias form: ${2: as ${3:alias}} makes the " as alias" part optional.
	// This matches the grammar (alias is optional) and topLevelCompletions().
	return []protocol.CompletionItem{
		snippetCompletion("import", "import \"${1:./path}\"${2: as ${3:alias}}", "Import statement"),
	}
}

// keywordCompletion creates a keyword completion item.
func keywordCompletion(label, insertText, detail string) protocol.CompletionItem {
	kind := protocol.CompletionItemKindKeyword
	format := protocol.InsertTextFormatSnippet
	sortText := "0_" + label
	return protocol.CompletionItem{
		Label:            label,
		Kind:             &kind,
		Detail:           &detail,
		InsertText:       &insertText,
		InsertTextFormat: &format,
		SortText:         &sortText,
	}
}

// snippetCompletion creates a snippet completion item.
func snippetCompletion(label, insertText, detail string) protocol.CompletionItem {
	kind := protocol.CompletionItemKindSnippet
	format := protocol.InsertTextFormatSnippet
	sortText := "1_" + label
	return protocol.CompletionItem{
		Label:            label,
		Kind:             &kind,
		Detail:           &detail,
		InsertText:       &insertText,
		InsertTextFormat: &format,
		SortText:         &sortText,
	}
}
