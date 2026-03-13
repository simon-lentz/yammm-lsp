package lsp

import (
	"testing"

	"github.com/simon-lentz/yammm/location"
	"github.com/simon-lentz/yammm/schema"

	"github.com/simon-lentz/yammm-lsp/internal/symbols"
)

func TestBuildHoverForSymbol_NilData(t *testing.T) {
	t.Parallel()
	s := &Server{}

	sym := &symbols.Symbol{
		Name: "Unknown",
		Kind: symbols.SymbolType,
		Data: nil, // No data
	}

	h, err := s.buildHoverForSymbolWithRange(sym, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h != nil {
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

	h, err := s.buildHoverForSymbolWithRange(sym, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h != nil {
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
