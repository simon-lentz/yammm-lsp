package hover

import (
	"strings"
	"testing"

	"github.com/simon-lentz/yammm/location"
	"github.com/simon-lentz/yammm/schema"

	"github.com/simon-lentz/yammm-lsp/internal/symbols"
)

func TestHoverForSchema(t *testing.T) {
	t.Parallel()

	sym := &symbols.Symbol{
		Name: "MySchema",
		Kind: symbols.SymbolSchema,
	}

	result := RenderSymbol(sym, "")

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

	result := RenderSymbol(sym, "")

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

	sourceID := location.MustNewSourceID("test://types.yammm")
	span := location.Range(sourceID, 1, 1, 10, 1)

	typ := schema.NewType("Person", sourceID, span, "A person entity.", false, false)

	sym := &symbols.Symbol{
		Name:     "Person",
		Kind:     symbols.SymbolType,
		SourceID: sourceID,
		Data:     typ,
	}

	result := RenderSymbol(sym, "/project")

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

	sourceID := location.MustNewSourceID("test://types.yammm")
	span := location.Range(sourceID, 1, 1, 10, 1)

	typ := schema.NewType("Entity", sourceID, span, "", true, false)

	sym := &symbols.Symbol{
		Name:     "Entity",
		Kind:     symbols.SymbolType,
		SourceID: sourceID,
		Data:     typ,
	}

	result := RenderSymbol(sym, "")

	if !strings.Contains(result, "**abstract type**") {
		t.Error("hover should contain 'abstract type' for abstract types")
	}
}

func TestHoverForType_Part(t *testing.T) {
	t.Parallel()

	sourceID := location.MustNewSourceID("test://types.yammm")
	span := location.Range(sourceID, 1, 1, 10, 1)

	typ := schema.NewType("Wheel", sourceID, span, "", false, true)

	sym := &symbols.Symbol{
		Name:     "Wheel",
		Kind:     symbols.SymbolType,
		SourceID: sourceID,
		Data:     typ,
	}

	result := RenderSymbol(sym, "")

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

	result := RenderSymbol(sym, "")

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

	result := RenderSymbol(sym, "")

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

	result := RenderSymbol(sym, "")

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

	result := RenderSymbol(sym, "")

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

	result := RenderSymbol(sym, "")

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

	result := RenderSymbol(sym, "")

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
			// RenderSymbol with a type symbol exercises relativeSourcePath
			// For this test, use the hover package's exported function indirectly
			sym := &symbols.Symbol{
				Name:     "TestType",
				Kind:     symbols.SymbolType,
				SourceID: tt.sourceID,
				Data:     schema.NewType("TestType", tt.sourceID, location.Span{}, "", false, false),
			}
			result := RenderSymbol(sym, tt.root)
			if !strings.Contains(result, tt.want) {
				t.Errorf("hover content should contain %q; got:\n%s", tt.want, result)
			}
		})
	}
}
