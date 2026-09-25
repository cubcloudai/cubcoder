package tools

import (
	"strings"
	"testing"
)

func TestToolValidate(t *testing.T) {
	reg := NewIn("")

	get := func(name string) *Tool {
		tl, ok := reg.Get(name)
		if !ok {
			t.Fatalf("tool %q not registered", name)
		}
		return tl
	}

	t.Run("missing required field is reported by name", func(t *testing.T) {
		err := get("read_file").Validate(`{"offset":1}`)
		if err == nil {
			t.Fatal("expected an error for missing path")
		}
		if !strings.Contains(err.Error(), `"path"`) || !strings.Contains(err.Error(), "required") {
			t.Errorf("error should name the missing required field: %v", err)
		}
	})

	t.Run("required field present passes", func(t *testing.T) {
		if err := get("read_file").Validate(`{"path":"x.go"}`); err != nil {
			t.Errorf("Validate = %v, want nil", err)
		}
	})

	t.Run("null required field counts as missing", func(t *testing.T) {
		if err := get("write_file").Validate(`{"path":null,"content":"x"}`); err == nil {
			t.Error("expected null path to be treated as missing")
		}
	})

	t.Run("malformed JSON is rejected with a hint", func(t *testing.T) {
		err := get("edit_file").Validate(`{"path":"x", "search":`)
		if err == nil {
			t.Fatal("expected an error for malformed JSON")
		}
		if !strings.Contains(err.Error(), "JSON object") {
			t.Errorf("error should explain expected shape: %v", err)
		}
	})

	t.Run("empty args ok when nothing is required", func(t *testing.T) {
		// list_dir has only optional params, so empty args resolve to {}.
		if err := get("list_dir").Validate(""); err != nil {
			t.Errorf("Validate(empty) = %v, want nil", err)
		}
	})

	t.Run("arg hint lists required fields first", func(t *testing.T) {
		hint := get("read_file").argHint()
		if !strings.HasPrefix(hint, "path (required)") {
			t.Errorf("hint should lead with the required field: %q", hint)
		}
		for _, want := range []string{"offset", "limit"} {
			if !strings.Contains(hint, want) {
				t.Errorf("hint missing optional field %q: %q", want, hint)
			}
		}
	})
}
