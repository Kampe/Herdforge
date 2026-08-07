package stash

import "os"

func environ() []string { return os.Environ() }
