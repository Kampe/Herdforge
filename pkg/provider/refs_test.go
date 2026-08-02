package provider

import "testing"

func TestCompareRefs(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"FAC-9", "FAC-61", -1},
		{"FAC-99", "FAC-100", -1},
		{"FAC-61", "FAC-61", 0},
		{"FAC-100", "FAC-99", 1},
		{"fac-2", "FAC-10", -1},
		{"XYZ-5", "FAC-5", 1}, // different prefixes: lexical
		{"garbage", "FAC-5", 1},
	}
	for _, c := range cases {
		got := CompareRefs(c.a, c.b)
		if (got < 0) != (c.want < 0) || (got > 0) != (c.want > 0) {
			t.Errorf("CompareRefs(%q, %q) = %d, want sign of %d", c.a, c.b, got, c.want)
		}
	}
}
