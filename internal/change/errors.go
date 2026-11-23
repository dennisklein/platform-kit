package change

import "errors"

// Errors the bench maps to HTTP status codes.
var (
	ErrNotFound   = errors.New("change not found")                 // 404
	ErrNoWorktree = errors.New("change has no local worktree")     // 409
	ErrForbidden  = errors.New("forbidden")                        // 403
	ErrAgentDeny  = errors.New("agents never approve or merge")    // 403
	ErrBehind     = errors.New("behind target, rebase first")      // 409
	ErrConflict   = errors.New("conflict")                         // 409
	ErrGates      = errors.New("policy gates are not met")         // 409
	ErrDirty      = errors.New("worktree has uncommitted changes") // 409
)
