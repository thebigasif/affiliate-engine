//go:build tools

// Package tools pins staticcheck with a golang.org/x/tools new enough to
// read the export data of the Go toolchain in the root go.mod. Build it
// with: go build -C tools/staticcheck -o ../../.bin/staticcheck honnef.co/go/tools/cmd/staticcheck
package tools

import _ "honnef.co/go/tools/cmd/staticcheck"
