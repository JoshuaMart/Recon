// Package version carries the build version, set at link time with
// -ldflags "-X github.com/JoshuaMart/recon/internal/version.Version=v1.2.3".
package version

// Version is "dev" outside a release build.
var Version = "dev"
