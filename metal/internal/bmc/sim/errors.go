package sim

import (
	"errors"
	"fmt"
	"net/http"
)

// Error is a refusal the HTTP tier renders as a Redfish error body: an HTTP
// status and a message from the DMTF Base registry. The in-memory client hands
// the same value back as a plain Go error, so both transports fail for the
// same stated reason.
type Error struct {
	Status int
	// MessageID is the Base registry key, without registry prefix or version
	// ("PropertyValueNotInList").
	MessageID string
	Message   string
	Args      []string
}

func (e *Error) Error() string { return "sim: " + e.Message }

// baseRegistry is the registry prefix every MessageId carries — the Base
// registry version of the 2024.4 bundle.
const baseRegistry = "Base.1.19."

// errUnreachable is the dead-BMC path: every call fails while the fault is on.
var errUnreachable = errors.New("sim: endpoint unreachable (injected)")

// refuse is a 400 with the registry's catch-all — a request the service
// understood and declined, for a reason only the message text carries.
func refuse(format string, args ...any) *Error {
	return &Error{Status: http.StatusBadRequest, MessageID: "GeneralError", Message: fmt.Sprintf(format, args...)}
}

// notFound is a resource (or a member named by a request) that does not exist.
func notFound(kind, id string) *Error {
	return &Error{
		Status: http.StatusNotFound, MessageID: "ResourceNotFound",
		Message: fmt.Sprintf("the requested resource of type %s named %q was not found", kind, id),
		Args:    []string{kind, id},
	}
}

// notInList is a value outside the set a property or action parameter allows.
func notInList(messageID, name, value string) *Error {
	return &Error{
		Status: http.StatusBadRequest, MessageID: messageID,
		Message: fmt.Sprintf("the value %q for %s is not in the list of acceptable values", value, name),
		Args:    []string{value, name},
	}
}

// missing is a required action parameter the request left out.
func missing(action, parameter string) *Error {
	return &Error{
		Status: http.StatusBadRequest, MessageID: "ActionParameterMissing",
		Message: fmt.Sprintf("the action %s requires the parameter %s to be present in the request body", action, parameter),
		Args:    []string{action, parameter},
	}
}

// errorBody renders the specification's error shape: a top-level code and
// message, the detail as one @Message.ExtendedInfo member.
func errorBody(e *Error) map[string]any {
	args := e.Args
	if args == nil {
		args = []string{}
	}
	id := baseRegistry + e.MessageID
	return map[string]any{"error": map[string]any{
		"code":    id,
		"message": e.Message,
		"@Message.ExtendedInfo": []map[string]any{{
			"@odata.type":     "#Message.v1_2_1.Message",
			"MessageId":       id,
			"Message":         e.Message,
			"MessageArgs":     args,
			"MessageSeverity": "Critical",
			"Resolution":      "Correct the request and resubmit it.",
		}},
	}}
}

// asError maps any error onto a renderable one: typed refusals pass through,
// the injected outage becomes a 503 (an unreachable BMC does not answer at
// all; a 503 is the closest a live listener gets), anything else a 400.
func asError(err error) *Error {
	var e *Error
	switch {
	case errors.As(err, &e):
		return e
	case errors.Is(err, errUnreachable):
		return &Error{Status: http.StatusServiceUnavailable, MessageID: "ServiceTemporarilyUnavailable", Message: err.Error()}
	default:
		return &Error{Status: http.StatusBadRequest, MessageID: "GeneralError", Message: err.Error()}
	}
}
