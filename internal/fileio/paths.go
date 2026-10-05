package fileio

import (
	"fmt"
	"unicode/utf8"
)

// CheckJSONPaths refuses paths that encoding/json would silently change to different names.
func CheckJSONPaths(paths ...string) error {
	for _, path := range paths {
		if !utf8.ValidString(path) {
			return fmt.Errorf("path is not valid UTF-8: %q", path)
		}
	}
	return nil
}
