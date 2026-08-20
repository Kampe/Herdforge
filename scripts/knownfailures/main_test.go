package main

import "testing"

func TestCompareExactSet(t *testing.T) {
	want := []failure{{"pkg/a", "TestA"}, {"pkg/b", "TestB"}}
	cases := []struct {
		name    string
		actual  []failure
		wantErr bool
	}{
		{"same set", want, false}, {"order independent", []failure{want[1], want[0]}, false},
		{"missing failure", []failure{want[0]}, true},
		{"unexpected failure", append(append([]failure{}, want...), failure{"pkg/c", "TestFake"}), true},
		{"duplicate is not drift", append(append([]failure{}, want...), want[0]), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if (compare(want, tc.actual) != nil) != tc.wantErr {
				t.Fatalf("compare(%v), wantErr=%v", tc.actual, tc.wantErr)
			}
		})
	}
}
