// Package version holds build metadata for harnessd.
package version

import "fmt"

// These values are overridden at build time via -ldflags.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a human-readable version string.
func String() string {
	return fmt.Sprintf("harnessd %s (commit %s, built %s)", Version, Commit, Date)
}
