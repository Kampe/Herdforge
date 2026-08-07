package signerboundary

import (
	"os"
	"runtime"
	"time"
)

func openGOOS() string { return runtime.GOOS }

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func nowUTC() time.Time { return time.Now().UTC() }
