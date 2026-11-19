package ids

import "testing"

func TestDeterministicAndDistinct(t *testing.T) {
	k1 := RunKey("org/a", 123, 1)
	k2 := RunKey("org/a", 123, 2)
	if k1 != "org/a#123#1" {
		t.Fatalf("RunKey = %q", k1)
	}
	if Trace(k1) != Trace(k1) || RunSpan(k1) != RunSpan(k1) {
		t.Fatal("IDs not deterministic")
	}
	if Trace(k1) == Trace(k2) {
		t.Fatal("attempts must get different traces")
	}
	seen := map[SpanID]string{}
	for name, id := range map[string]SpanID{
		"run":   RunSpan(k1),
		"call":  CallSpan(k1, "deploy"),
		"call2": CallSpan(k1, "build"),
		"job":   JobSpan(1),
		"job2":  JobSpan(2),
		"step":  StepSpan(1, 1),
		"step2": StepSpan(1, 2),
	} {
		if id == (SpanID{}) {
			t.Errorf("%s: zero span ID", name)
		}
		if other, dup := seen[id]; dup {
			t.Errorf("%s collides with %s", name, other)
		}
		seen[id] = name
	}
}
