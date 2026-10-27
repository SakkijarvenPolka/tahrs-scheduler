// Package version carries the build identifier printed in the startup
// fingerprint.
package version

// Build is overridden at link time:
//
//	go build -ldflags "-X github.com/SakkijarvenPolka/tahrs-scheduler/internal/version.Build=$(git describe --tags --always --dirty)"
var Build = "dev"

// Get returns the build identifier.
func Get() string {
	if Build == "" {
		return "dev"
	}
	return Build
}
