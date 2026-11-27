package nativeguest

import (
	"strings"
	"testing"
)

func TestMetadataTokenWalkHasIndependentResourceBounds(t *testing.T) {
	for name, raw := range map[string]string{
		"deep":        `{"x":` + strings.Repeat("[", 2000) + "0" + strings.Repeat("]", 2000) + "}",
		"many-values": `{"x":[` + strings.Repeat("0,", 65536) + "0]}",
	} {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			if err := strictJSON([]byte(raw), &value); err == nil || !strings.Contains(err.Error(), "bound") {
				t.Fatal("resource-heavy guest metadata was not rejected during token validation", err)
			}
		})
	}
	for name, raw := range map[string]string{
		"ordinary": `{"x":[{"small":"unchanged"}]}`,
		// Root object, its key and the first array contribute three tokens.
		"exact-token-bound": `{"x":[` + strings.Repeat("0,", maxMetadataTokens-4) + "0]}",
		// The root is at depth zero; the final scalar is at depth 64.
		"exact-depth-bound": `{"x":` + strings.Repeat("[", maxMetadataDepth-1) + "0" + strings.Repeat("]", maxMetadataDepth-1) + "}",
	} {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			if err := strictJSON([]byte(raw), &value); err != nil {
				t.Fatal("metadata inside the resource bounds was rejected", err)
			}
		})
	}
}
