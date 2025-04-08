package utils

import (
	"path/filepath"
	"runtime"
	"strings"
)

var baseDir string

// AbsPath returns the absolute path for a file located relative to the caller’s file.
func AbsPath(relPath string) string {
	if baseDir != "" {
		return filepath.Join(baseDir, relPath)
	}
	pcs := make([]uintptr, 20)
	n := runtime.Callers(0, pcs)
	if n == 0 {
		return relPath
	}
	pcs = pcs[:n]
	frames := runtime.CallersFrames(pcs)
	var frame runtime.Frame
	found := false
	for {
		f, more := frames.Next()
		if !strings.Contains(f.File, "/src/runtime/") && !strings.Contains(f.File, "utils/") {
			frame = f
			found = true
			break
		}
		if !more {
			break
		}
	}
	if !found {
		return relPath
	}
	baseDir = filepath.Dir(frame.File)
	combinedPath := filepath.Join(baseDir, relPath)
	absPath, err := filepath.Abs(combinedPath)
	if err != nil {
		return ""
	}
	return absPath
}
