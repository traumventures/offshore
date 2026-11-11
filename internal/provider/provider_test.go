package provider

import "testing"

func TestInt(t *testing.T) {
	opts := map[string]any{"a": int64(7), "b": float64(8), "c": "x"}
	if Int(opts, "a", 1) != 7 || Int(opts, "b", 1) != 8 || Int(opts, "c", 1) != 1 || Int(opts, "d", 2) != 2 {
		t.Fatal("Int")
	}
}
