package v1alpha1

import (
	"reflect"
	"strings"
)

// actionKeys is the JSON key of each HostOperationAction field, by field
// index. Read once: the union's shape is fixed at compile time.
var actionKeys = func() []string {
	t := reflect.TypeFor[HostOperationAction]()
	keys := make([]string, t.NumField())
	for i := range t.NumField() {
		keys[i], _, _ = strings.Cut(t.Field(i).Tag.Get("json"), ",")
	}
	return keys
}()

// Key is the union key of the action that is set (powerCycle, talosReboot,
// …): the step name the executor records, and what the CLI shows. It is
// derived from the field's JSON name, so a new action needs no second list
// to stay in step with. An action with no key set is "unknown".
func (a HostOperationAction) Key() string {
	v := reflect.ValueOf(a)
	for i := range v.NumField() {
		if !v.Field(i).IsNil() {
			return actionKeys[i]
		}
	}
	return "unknown"
}

// ActionKeys lists every action key the API defines, in field order.
func ActionKeys() []string { return append([]string(nil), actionKeys...) }
