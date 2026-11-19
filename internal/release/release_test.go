package release

import "testing"

func TestClassify(t *testing.T) {
	c := MustDefault()
	cases := []struct {
		in   string
		ok   bool
		want Info
	}{
		{"v1.20.0", true, Info{Line: "v1.20", Version: "v1.20.0", Type: Release}},
		{"v1.20.1", true, Info{Line: "v1.20", Version: "v1.20.1", Type: HotfixTag}},
		{"v1.20.12", true, Info{Line: "v1.20", Version: "v1.20.12", Type: HotfixTag}},
		{"v2.0.00", true, Info{Line: "v2.0", Version: "v2.0.00", Type: Release}},
		{"release/v1.20.0", true, Info{Line: "v1.20", Type: HotfixMerge}},
		{"main", false, Info{}},
		{"feature/x", false, Info{}},
		{"v1.20", false, Info{}},
		{"v1.20.0-rc1", false, Info{}},
		{"release/main", false, Info{}},
		{"release/v1.20", false, Info{}},
		{"", false, Info{}},
	}
	for _, tc := range cases {
		got, ok := c.Classify(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("Classify(%q) = %+v, %v; want %+v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(`^v(\d+)\.(\d+)$`, "release/"); err == nil {
		t.Error("expected error for pattern with 2 groups")
	}
	if _, err := New(`(`, "release/"); err == nil {
		t.Error("expected error for invalid regex")
	}
	if _, err := New(DefaultTagPattern, ""); err == nil {
		t.Error("expected error for empty prefix")
	}
}

func TestCustomPattern(t *testing.T) {
	c, err := New(`^(\d+)\.(\d+)\.(\d+)$`, "rel-")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := c.Classify("rel-3.4.0")
	if !ok || got.Line != "v3.4" || got.Type != HotfixMerge {
		t.Errorf("got %+v, %v", got, ok)
	}
}
