// Package protocol defines LSP protocol types used by yammm-lsp.
// Only types actually referenced in the codebase are defined.
package protocol

import (
	"encoding/json"
	"strconv"
)

// Integer defines an integer number in the range of -2^31 to 2^31 - 1.
type Integer = int32

// UInteger defines an unsigned integer number in the range of 0 to 2^31 - 1.
type UInteger = uint32

// Method is the LSP method name.
type Method = string

// IntegerOrString can hold either an Integer or a string.
type IntegerOrString struct {
	Value any // Integer | string
}

func (s IntegerOrString) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Value) //nolint:wrapcheck // protocol marshaling
}

func (s *IntegerOrString) UnmarshalJSON(data []byte) error {
	var intVal Integer
	if err := json.Unmarshal(data, &intVal); err == nil {
		s.Value = intVal
		return nil
	}
	var strVal string
	if err := json.Unmarshal(data, &strVal); err == nil {
		s.Value = strVal
		return nil
	}
	return json.Unmarshal(data, &s.Value) //nolint:wrapcheck // protocol marshaling
}

// BoolOrString can hold either a bool or a string.
type BoolOrString struct {
	Value any // bool | string
}

func (s BoolOrString) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.Value) //nolint:wrapcheck // protocol marshaling
}

func (s *BoolOrString) UnmarshalJSON(data []byte) error {
	var boolVal bool
	if err := json.Unmarshal(data, &boolVal); err == nil {
		s.Value = boolVal
		return nil
	}
	var strVal string
	if err := json.Unmarshal(data, &strVal); err == nil {
		s.Value = strVal
		return nil
	}
	return json.Unmarshal(data, &s.Value) //nolint:wrapcheck // protocol marshaling
}

func (s BoolOrString) String() string {
	if value, ok := s.Value.(bool); ok {
		return strconv.FormatBool(value)
	}
	return s.Value.(string) //nolint:errcheck,forcetypeassert // type is guaranteed by construction
}
