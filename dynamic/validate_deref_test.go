package dynamic

import "testing"

func TestValidate_TypedNilAndPointerNumerics(t *testing.T) {
	var nilInt *int
	var nilFloat *float64
	five := 5
	pct := 99.5

	// A typed nil pointer is "absent", exactly like an untyped nil.
	for name, raw := range map[string]any{"untyped": nil, "*int": nilInt, "*float64": nilFloat} {
		if !isEmptyValue(raw) {
			t.Errorf("isEmptyValue(%s) = false, want true", name)
		}
	}
	// A non-nil pointer to a number is a number.
	for name, raw := range map[string]any{"*int": &five, "*float64": &pct, "int": 5, "string number": "5"} {
		if !isNumeric(raw) {
			t.Errorf("isNumeric(%s) = false, want true", name)
		}
	}
	if isNumeric("abc") || isNumeric(&struct{}{}) {
		t.Errorf("non-numeric values must stay non-numeric")
	}
	if isEmptyValue(&five) {
		t.Errorf("a non-nil pointer must not read as empty")
	}
}
