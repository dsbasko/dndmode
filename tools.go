//go:build tools

// Package main is a placeholder used only to pin tooling and runtime
// dependencies that are required across the project but not yet referenced
// by production code in of Phase 1 (bootstrap).
//
// This file is gated behind the `tools` build tag, so it is NEVER compiled
// into the production binary. Its sole purpose is to ensure `go mod tidy`
// keeps the listed modules in the require block at exact versions until
// real production code imports them in subsequent plans.
//
// Modules pinned here:
// - github.com/goccy/go-yaml — used by internal/config
//   - go.uber.org/mock         — used as a tool (mockgen) and runtime in tests
//
// Once internal/config imports go-yaml directly, this file may
// stay or be deleted; it remains harmless either way.
package main

import (
	_ "github.com/goccy/go-yaml"
	_ "go.uber.org/mock/gomock"
)
