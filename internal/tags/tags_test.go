package tags

import "testing"

func TestMatch(t *testing.T) {
	m := MustDefault()
	for ref, want := range map[string]bool{
		"v1.20.0":         true,
		"v1.20.1":         true,
		"v10.0.12":        true,
		"main":            false,
		"release/v1.20.0": false,
		"v1.20":           false,
		"v1.20.0-rc1":     false,
		"":                false,
	} {
		if got := m.Match(ref); got != want {
			t.Errorf("Match(%q) = %v, want %v", ref, got, want)
		}
	}
	if _, err := New("("); err == nil {
		t.Fatal("bad pattern accepted")
	}
}
