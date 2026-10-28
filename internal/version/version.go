// Package version holds the build version, set at link time:
//
//	go build -ldflags "-X github.com/richard-sen27/mycelium/internal/version.Version=v0.1.0"
package version

// Version is the build version; "dev" for local builds.
var Version = "dev"
