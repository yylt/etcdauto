//go:build tools
// +build tools

package main

import (
	// https://golangci-lint.run
	_ "github.com/golangci/golangci-lint/cmd/golangci-lint"
	// https://go.dev/blog/vuln
	_ "golang.org/x/vuln/cmd/govulncheck"
	// https://github.com/kubernetes-sigs/controller-tools
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen"
)
