package main

import "testing"

func TestParseNextArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want nextRequest
	}{
		{name: "role", args: []string{"--role", "forge-smith"}, want: nextRequest{Role: "forge-smith"}},
		{name: "lane", args: []string{"--lane", "worker-a"}, want: nextRequest{Lane: "worker-a"}},
		{name: "both", args: []string{"--role=forge-smith", "--lane=worker-a"}, want: nextRequest{Role: "forge-smith", Lane: "worker-a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseNextArgs(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("parseNextArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

func TestParseNextArgsRejectsPositionals(t *testing.T) {
	if _, err := parseNextArgs([]string{"FAC-1"}); err == nil {
		t.Fatal("parseNextArgs accepted an unexpected positional argument")
	}
}
