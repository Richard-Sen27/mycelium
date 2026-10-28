// Package e2e runs the multi-process acceptance tests: it builds the mycel
// binary once, then starts one hub and several nodes as separate processes
// with their own MYCEL_HOME directories and random ports.
//
// Build tag: e2e. Run with `make e2e` or `go test -tags e2e ./test/e2e/...`.
package e2e
