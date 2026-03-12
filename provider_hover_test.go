package lsp

import (
	"strings"
	"testing"

	"github.com/simon-lentz/yammm/location"
	"github.com/simon-lentz/yammm/schema"

	"github.com/simon-lentz/yammm-lsp/internal/analysis"
	"github.com/simon-lentz/yammm-lsp/internal/symbols"
)

func TestHoverForSchema(t *testing.T) {
	t.Parallel()

	sym := &symbols.Symbol{
		Name: "MySchema",
		Kind: symbols.SymbolSchema,
	}

	result := hoverForSchema(sym)

	if !strings.Contains(result, "**schema**") {
		t.Error("hover should contain 'schema' keyword")
	}
	if !strings.Contains(result, "MySchema") {
		t.Error("hover should contain schema name")
	}
}

func TestHoverForImport(t *testing.T) {
	t.Parallel()

	sourceID := location.MustNewSourceID("test://main.yammm")
	importedID := location.MustNewSourceID("test://parts.yammm")
	span := location.Range(sourceID, 1, 1, 1, 30)

	imp := schema.NewImport("./parts", "parts", importedID, span)

	sym := &symbols.Symbol{
		Name: "parts",
		Kind: symbols.SymbolImport,
		Data: imp,
	}

	result := hoverForImport(sym)

	if !strings.Contains(result, "**import**") {
		t.Error("hover should contain 'import' keyword")
	}
	if !strings.Contains(result, "./parts") {
		t.Error("hover should contain import path")
	}
	if !strings.Contains(result, "parts") {
		t.Error("hover should contain alias")
	}
}

func TestHoverForType(t *testing.T) {
	t.Parallel()
	s := &Server{}

	sourceID := location.MustNewSourceID("test://types.yammm")
	span := location.Range(sourceID, 1, 1, 10, 1)

	typ := schema.NewType("Person", sourceID, span, "A person entity.", false, false)

	sym := &symbols.Symbol{
		Name:     "Person",
		Kind:     symbols.SymbolType,
		SourceID: sourceID,
		Data:     typ,
	}

	snapshot := &analysis.Snapshot{
		Root: "/project",
	}

	result := s.hoverForType(sym, snapshot)

	if !strings.Contains(result, "**type**") {
		t.Error("hover should contain 'type' keyword")
	}
	if !strings.Contains(result, "Person") {
		t.Error("hover should contain type name")
	}
	if !strings.Contains(result, "A person entity.") {
		t.Error("hover should contain documentation")
	}
}

func TestHoverForType_Abstract(t *testing.T) {
	t.Parallel()
	s := &Server{}

	sourceID := location.MustNewSourceID("test://types.yammm")
	span := location.Range(sourceID, 1, 1, 10, 1)

	typ := schema.NewType("Entity", sourceID, span, "", true, false)

	sym := &symbols.Symbol{
		Name:     "Entity",
		Kind:     symbols.SymbolType,
		SourceID: sourceID,
		Data:     typ,
	}

	result := s.hoverForType(sym, nil)

	if !strings.Contains(result, "**abstract type**") {
		t.Error("hover should contain 'abstract type' for abstract types")
	}
}

func TestHoverForType_Part(t *testing.T) {
	t.Parallel()
	s := &Server{}

	sourceID := location.MustNewSourceID("test://types.yammm")
	span := location.Range(sourceID, 1, 1, 10, 1)

	typ := schema.NewType("Wheel", sourceID, span, "", false, true)

	sym := &symbols.Symbol{
		Name:     "Wheel",
		Kind:     symbols.SymbolType,
		SourceID: sourceID,
		Data:     typ,
	}

	result := s.hoverForType(sym, nil)

	if !strings.Contains(result, "**part type**") {
		t.Error("hover should contain 'part type' for part types")
	}
}

func TestHoverForProperty(t *testing.T) {
	t.Parallel()

	span := location.Range(location.MustNewSourceID("test://p.yammm"), 1, 1, 1, 20)

	// NewProperty(name, span, doc, constraint, dataTypeRef, optional, isPrimaryKey, scope)
	prop := schema.NewProperty("name", span, "The person's name.", nil, schema.DataTypeRef{}, false, false, schema.DeclaringScope{})

	sym := &symbols.Symbol{
		Name:       "name",
		Kind:       symbols.SymbolProperty,
		ParentName: "Person",
		Data:       prop,
	}

	result := hoverForProperty(sym)

	if !strings.Contains(result, "**property**") {
		t.Error("hover should contain 'property' keyword")
	}
	if !strings.Contains(result, "Person.name") {
		t.Error("hover should contain qualified property name")
	}
	if !strings.Contains(result, "The person's name.") {
		t.Error("hover should contain documentation")
	}
}

func TestHoverForProperty_Required(t *testing.T) {
	t.Parallel()

	span := location.Range(location.MustNewSourceID("test://p.yammm"), 1, 1, 1, 20)

	// optional=false means required
	prop := schema.NewProperty("email", span, "", nil, schema.DataTypeRef{}, false, false, schema.DeclaringScope{})

	sym := &symbols.Symbol{
		Name:       "email",
		Kind:       symbols.SymbolProperty,
		ParentName: "User",
		Data:       prop,
	}

	result := hoverForProperty(sym)

	if !strings.Contains(result, "**property**") {
		t.Error("hover should contain 'property' keyword")
	}
}

func TestHoverForRelation_Association(t *testing.T) {
	t.Parallel()

	targetRef := schema.NewTypeRef("", "Address", location.Span{})

	// NewRelation(kind, name, fieldName, target, targetID, span, doc,
	//             optional, many, backref, reverseOptional, reverseMany, owner, properties)
	rel := schema.NewRelation(
		schema.RelationAssociation,
		"ADDRESSES",
		"addresses",
		targetRef,
		schema.TypeID{}, // empty target ID
		location.Span{},
		"",    // doc
		false, // optional
		true,  // many
		"",    // backref
		false, // reverseOptional
		false, // reverseMany
		"",    // owner
		nil,   // properties
	)

	sym := &symbols.Symbol{
		Name:       "ADDRESSES",
		Kind:       symbols.SymbolAssociation,
		ParentName: "Person",
		Data:       rel,
	}

	result := hoverForRelation(sym)

	if !strings.Contains(result, "**association**") {
		t.Error("hover should contain 'association' keyword")
	}
	if !strings.Contains(result, "-->") {
		t.Error("hover should contain association arrow")
	}
	if !strings.Contains(result, "many") {
		t.Error("hover should show multiplicity")
	}
	if !strings.Contains(result, "Address") {
		t.Error("hover should contain target type")
	}
}

func TestHoverForRelation_Composition(t *testing.T) {
	t.Parallel()

	targetRef := schema.NewTypeRef("", "Wheel", location.Span{})

	rel := schema.NewRelation(
		schema.RelationComposition,
		"WHEELS",
		"wheels",
		targetRef,
		schema.TypeID{},
		location.Span{},
		"",
		false,
		true,
		"",
		false,
		false,
		"",
		nil,
	)

	sym := &symbols.Symbol{
		Name:       "WHEELS",
		Kind:       symbols.SymbolComposition,
		ParentName: "Car",
		Data:       rel,
	}

	result := hoverForRelation(sym)

	if !strings.Contains(result, "**composition**") {
		t.Error("hover should contain 'composition' keyword")
	}
	if !strings.Contains(result, "*->") {
		t.Error("hover should contain composition arrow")
	}
}

func TestHoverForInvariant(t *testing.T) {
	t.Parallel()

	inv := schema.NewInvariant("age must be positive", nil, location.Span{}, "Ensures age is valid.")

	sym := &symbols.Symbol{
		Name:       "age must be positive",
		Kind:       symbols.SymbolInvariant,
		ParentName: "Person",
		Data:       inv,
	}

	result := hoverForInvariant(sym)

	if !strings.Contains(result, "**invariant**") {
		t.Error("hover should contain 'invariant' keyword")
	}
	if !strings.Contains(result, "age must be positive") {
		t.Error("hover should contain invariant message")
	}
	if !strings.Contains(result, "Ensures age is valid.") {
		t.Error("hover should contain documentation")
	}
}

func TestHoverForDataType(t *testing.T) {
	t.Parallel()

	constraint := schema.NewStringConstraint()
	dt := schema.NewDataType("ShortName", constraint, location.Span{}, "A short name string.")

	sym := &symbols.Symbol{
		Name: "ShortName",
		Kind: symbols.SymbolDataType,
		Data: dt,
	}

	result := hoverForDataType(sym)

	if !strings.Contains(result, "**datatype**") {
		t.Error("hover should contain 'datatype' keyword")
	}
	if !strings.Contains(result, "ShortName") {
		t.Error("hover should contain datatype name")
	}
	if !strings.Contains(result, "A short name string.") {
		t.Error("hover should contain documentation")
	}
}

func TestRelativeSourcePath(t *testing.T) {
	t.Parallel()
	s := &Server{}

	// Use SourceIDFromAbsolutePath for file-backed sources
	schemaSourceID, err := location.SourceIDFromAbsolutePath("/project/schemas/person.yammm")
	if err != nil {
		t.Fatalf("failed to create source ID: %v", err)
	}
	personSourceID, err := location.SourceIDFromAbsolutePath("/project/person.yammm")
	if err != nil {
		t.Fatalf("failed to create source ID: %v", err)
	}

	tests := []struct {
		name     string
		sourceID location.SourceID
		root     string
		want     string
	}{
		{
			name:     "relative path within root",
			sourceID: schemaSourceID,
			root:     "/project",
			want:     "./schemas/person.yammm",
		},
		{
			name:     "no root",
			sourceID: personSourceID,
			root:     "",
			want:     "/project/person.yammm",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			snapshot := &analysis.Snapshot{Root: tt.root}
			got := s.relativeSourcePath(tt.sourceID, snapshot)
			if got != tt.want {
				t.Errorf("relativeSourcePath() = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestBuildHoverForSymbol_NilData(t *testing.T) {
	t.Parallel()
	s := &Server{}

	sym := &symbols.Symbol{
		Name: "Unknown",
		Kind: symbols.SymbolType,
		Data: nil, // No data
	}

	hover, err := s.buildHoverForSymbolWithRange(sym, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hover != nil {
		t.Error("hover should be nil when symbol has no data")
	}
}

func TestBuildHoverForSymbol_UnknownKind(t *testing.T) {
	t.Parallel()
	s := &Server{}

	sym := &symbols.Symbol{
		Name: "Unknown",
		Kind: symbols.SymbolKind(99), // Unknown kind
	}

	hover, err := s.buildHoverForSymbolWithRange(sym, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hover != nil {
		t.Error("hover should be nil for unknown symbol kind")
	}
}

func TestBuildHoverForSymbolWithRange_AcceptsOverrideParameter(t *testing.T) {
	// Tests that buildHoverForSymbolWithRange accepts an override range parameter.
	// The override range is used for reference hovers to return the reference's
	// location instead of the target symbol's location.
	t.Parallel()
	s := &Server{}

	sourceID := location.MustNewSourceID("test://main.yammm")
	targetSourceID := location.MustNewSourceID("test://imported.yammm")

	// The symbol is from a different file with its own span
	targetSymSpan := location.Range(targetSourceID, 10, 1, 10, 20)
	sym := &symbols.Symbol{
		Name:      "TargetType",
		Kind:      symbols.SymbolType,
		SourceID:  targetSourceID,
		Selection: targetSymSpan,
		Data:      &schema.Type{}, // Non-nil data
	}

	// The reference span is in the current document (different from target)
	refSpan := location.Range(sourceID, 5, 10, 5, 20)

	// Without override - returns nil because snapshot is nil
	hoverWithoutOverride, err := s.buildHoverForSymbolWithRange(sym, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// With override - also returns nil because snapshot is nil
	hoverWithOverride, err := s.buildHoverForSymbolWithRange(sym, nil, &refSpan)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both return nil because snapshot is nil (early return in function)
	// The test validates that the function signature accepts the override parameter
	// Integration tests should verify the full behavior with a real workspace
	_ = hoverWithoutOverride
	_ = hoverWithOverride
}
