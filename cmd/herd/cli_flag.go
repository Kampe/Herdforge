package main

import (
	"fmt"
	"strings"
)

// takeCLIFlag reads --name VALUE or --name=VALUE from args[i].
//
// FAC-765: host-ingest and review-bind-evidence both spelled "--candidate="
// as a distinctive literal. TestNoNewDuplicatedRules treats that as one
// decision written twice. The attached form is composed here so both parsers
// share one authority.
func takeCLIFlag(args []string, i int, name string) (value string, next int, matched bool, err error) {
	if i < 0 || i >= len(args) || name == "" {
		return "", i, false, nil
	}
	a := args[i]
	if a == name {
		if i+1 >= len(args) {
			return "", i, true, fmt.Errorf("missing value for %s", name)
		}
		return strings.TrimSpace(args[i+1]), i + 1, true, nil
	}
	prefix := name + "="
	if strings.HasPrefix(a, prefix) {
		return strings.TrimSpace(strings.TrimPrefix(a, prefix)), i, true, nil
	}
	return "", i, false, nil
}
