package credits

import (
	"testing"
)

func TestPaceClassOf(t *testing.T) {
	tt := []struct {
		name              string
		used, elapsed, floor int
		want              PaceClass
	}{
		{"exhausted_96pct", 96, 50, 60, ClassExhausted},
		{"exhausted_95pct", 95, 20, 60, ClassExhausted},
		{"overpace_high", 70, 14, 60, ClassOverpace},
		{"overpace_below_floor", 32, 14, 20, ClassOverpace},
		{"onpace_mid", 32, 14, 60, ClassOnpace},
		{"onpace_5050", 50, 50, 60, ClassOnpace},
		{"underspent_low", 2, 0, 60, ClassUnderspent},
		{"elapsed_floor", 5, 1, 60, ClassOnpace},
	}

	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			got := PaceClassOf(tc.used, tc.elapsed, tc.floor)
			if got != tc.want {
				t.Errorf("PaceClassOf(%d,%d,%d) = %s, want %s", tc.used, tc.elapsed, tc.floor, got, tc.want)
			}
		})
	}
}

func TestClassConcurrency(t *testing.T) {
	tt := []struct {
		cls  PaceClass
		want int
	}{
		{ClassExhausted, 0},
		{ClassOverpace, 1},
		{ClassOnpace, 2},
		{ClassUnderspent, 3},
		{PaceClass("unknown"), 2},
	}

	for _, tc := range tt {
		t.Run(string(tc.cls), func(t *testing.T) {
			got := ClassConcurrency(tc.cls)
			if got != tc.want {
				t.Errorf("ClassConcurrency(%s) = %d, want %d", tc.cls, got, tc.want)
			}
		})
	}
}
