package version

import (
	"fmt"
	"runtime"
)

var version string

// GetVersionString returns a standard version header
func GetVersionString(binary string) string {
	return fmt.Sprintf("%s, version %v", binary, version)
}

// GetVersionDetails returns a multi-line version string that includes the Go
// runtime version and the OS/architecture the binary was built for.
func GetVersionDetails(binary string) string {
	return fmt.Sprintf("%s\nGO version:   %s\nOS/Arch:      %s/%s",
		GetVersionString(binary),
		runtime.Version(),
		runtime.GOOS,
		runtime.GOARCH,
	)
}

// GetVersion returns the semver compatible version number
func GetVersion() string {
	return version
}
