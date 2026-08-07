//go:build unix

package signerboundary

// processUID returns the uid of pid when the platform can observe it.
func processUID(pid int) (int, bool) {
	return processUIDPlatform(pid)
}
