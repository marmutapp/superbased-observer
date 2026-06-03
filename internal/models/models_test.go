package models

import (
	"reflect"
	"testing"
)

// TestActionMetadata_IsZeroCoversEveryField is the reflection
// invariant from Invariant #50: ActionMetadata.IsZero MUST list every
// field on the struct, or sparse-zero rows will marshal to non-NULL
// "{}" and pollute the actions.metadata column.
//
// Procedure: for each field on ActionMetadata, build a struct value
// with ONLY that field set to a non-zero value, then assert
// IsZero() reports false. If a new field is added without extending
// IsZero, exactly that field's case fails — pointing the author at
// the missing line.
func TestActionMetadata_IsZeroCoversEveryField(t *testing.T) {
	t.Parallel()

	// Sentinel must be unambiguously non-zero for every field type
	// ActionMetadata currently uses. Update if new types arrive.
	rt := reflect.TypeOf(ActionMetadata{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		t.Run(f.Name, func(t *testing.T) {
			var m ActionMetadata
			rv := reflect.ValueOf(&m).Elem()
			fv := rv.Field(i)
			switch f.Type.Kind() {
			case reflect.String:
				fv.SetString("x")
			case reflect.Bool:
				fv.SetBool(true)
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				fv.SetInt(1)
			case reflect.Float32, reflect.Float64:
				fv.SetFloat(1.0)
			default:
				t.Fatalf("field %q has unsupported kind %s — extend the test sentinel switch", f.Name, f.Type.Kind())
			}
			if m.IsZero() {
				t.Fatalf("ActionMetadata.IsZero returned true with only field %q set non-zero — IsZero must be extended to cover this field, or the column will marshal to non-NULL {} on sparse rows (Invariant #50)", f.Name)
			}
		})
	}

	// Sanity: a true-zero struct really does report zero.
	var z ActionMetadata
	if !z.IsZero() {
		t.Fatalf("zero-valued ActionMetadata.IsZero returned false")
	}
}
