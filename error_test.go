package acp

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestErrorUnmarshalJSONNull(t *testing.T) {
	for _, initial := range []struct {
		name  string
		value Error
	}{
		{name: "empty"},
		{name: "populated", value: Error{Src: "source", Dst: "target", Err: errors.New("old failure")}},
	} {
		t.Run(initial.name, func(t *testing.T) {
			// A null document clears a reused value without inventing a failure.
			decoded := initial.value
			if err := json.Unmarshal([]byte("null"), &decoded); err != nil {
				t.Fatalf("decode null error: %v", err)
			}
			if decoded.Src != "" || decoded.Dst != "" || decoded.Err != nil {
				t.Fatalf("decoded null = %#v, want an empty error value", decoded)
			}

			// Re-encoding and decoding must keep the failure absent.
			encoded, err := json.Marshal(&decoded)
			if err != nil {
				t.Fatalf("encode absent error: %v", err)
			}
			if string(encoded) != "{}" {
				t.Fatalf("encoded absent error = %s, want {}", encoded)
			}
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("decode absent error: %v", err)
			}
			if decoded.Err != nil {
				t.Fatalf("round-trip error = %v, want nil", decoded.Err)
			}
		})
	}
}
