// Package api embeds the OpenAPI spec of the bench. The CLI and the MCP
// server build their commands and tools from it at run time.
package api

import _ "embed"

// Spec is api/openapi.yaml.
//
//go:embed openapi.yaml
var Spec []byte
