//go:build tools

// This file pins codegen-time dependencies that no compiled package
// imports yet: oapi-codegen's runtime is imported by generated .gen.go
// files (make openapi), and the v1.4.1 pin must survive go mod tidy
// until the first context lands its generated ports. The build tag is
// never enabled — the file only feeds the module graph.
package tools

import (
	_ "github.com/oapi-codegen/runtime"
)
