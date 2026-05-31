// Package api embeds the OpenAPI 3.1 description of the locally-mirrored HTTP
// surface so the serve command can publish it at /openapi.yaml without depending
// on the file being present at runtime.
package api

import _ "embed"

//go:embed openapi.yaml
var OpenAPISpec []byte
