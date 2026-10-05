package fileio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"unicode/utf8"
)

// ReadJSON loads exactly one nonnull document, refusing bytes JSON would silently replace.
func ReadJSON(path string, value any) error {
	// Validate the original bytes before decoding loses unsupported filename identities.
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("JSON is not valid UTF-8")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("JSON document is null")
	}

	// Unmarshal consumes the entire document, including any trailing non-whitespace bytes.
	return json.Unmarshal(data, value)
}
