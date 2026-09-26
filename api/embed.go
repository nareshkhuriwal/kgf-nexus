// Copyright 2026 Khuriwal Group
// SPDX-License-Identifier: Apache-2.0

// Package api holds the machine-readable description of the public REST API.
//
// openapi.yaml lives at the repository root, beside the other contracts
// (schemas/, proto/), rather than inside the gateway: it is read by people,
// the SDK and generators as much as by the gateway. go:embed cannot reach
// outside a package directory, so this tiny package exists to compile the
// file into any binary that imports it -- the gateway serves it at
// GET /v1/openapi.yaml -- without keeping a second copy that could drift.
package api

import (
	"bytes"
	_ "embed"
)

//go:embed openapi.yaml
var openAPIYAML []byte

// OpenAPIYAML returns the OpenAPI 3.1 document exactly as committed.
//
// A copy, so no caller can alter the bytes every other caller serves.
func OpenAPIYAML() []byte {
	return bytes.Clone(openAPIYAML)
}
