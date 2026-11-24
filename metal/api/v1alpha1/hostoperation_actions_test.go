package v1alpha1

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every union member is an optional pointer with its own JSON key, which is
// what Key relies on; and Key names the member that is set.
func TestActionKey(t *testing.T) {
	typ := reflect.TypeFor[HostOperationAction]()
	seen := map[string]bool{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		require.Equal(t, reflect.Pointer, f.Type.Kind(), "%s is a pointer", f.Name)
		key := ActionKeys()[i]
		require.NotEmpty(t, key, "%s has a json name", f.Name)
		require.False(t, seen[key], "%s is unique", key)
		seen[key] = true

		var a HostOperationAction
		reflect.ValueOf(&a).Elem().Field(i).Set(reflect.New(f.Type.Elem()))
		assert.Equal(t, key, a.Key())
	}
	assert.Equal(t, "unknown", HostOperationAction{}.Key())
	assert.Equal(t, "talosReboot", HostOperationAction{TalosReboot: &TalosRebootAction{}}.Key())
}
