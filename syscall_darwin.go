//go:build darwin
// +build darwin

package acp

import (
	"os"
	"strings"

	"github.com/samuelncui/acp/signature"
)

func truncate(file *os.File, size int64) error {
	if err := file.Truncate(size); err != nil {
		return err
	}
	return nil
}

func checkXattrKey(key string) bool {
	return !strings.HasPrefix(key, "system.") && !signature.IsAttribute(key)
}
