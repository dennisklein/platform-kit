// Package seed builds the demo state repo on the forge: the initial commit,
// the merged history (one merge commit with trailers per change) and the open
// changes with branches, pull requests, lab statuses and approvals. It reads
// the embedded tree from seedfs (seed/state and seed/changes, format in
// seed/README.md). pk forge-init uses it.
package seed
