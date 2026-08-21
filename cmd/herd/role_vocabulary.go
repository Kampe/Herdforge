package main

import (
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
)

// projectImplementationRoleVocabulary collects the ownership roles this
// repository actually uses.
//
// FAC-567: a card labelled with a canonical project lane was refused because
// role resolution only knew generic nouns. The vocabulary comes from what the
// repository has already declared, so an operator does not maintain a second
// list that can drift from the roster:
//
//   - every configured lane's role and name, since a board label naming a lane
//     is the normal way this fleet expresses ownership;
//   - HERD_IMPLEMENTATION_ROLES, a comma-separated escape hatch for board
//     labels that intentionally have no lane (e.g. a legacy label kept for
//     historical cards).
//
// This only widens what is RECOGNIZED. An unknown label still refuses, so a
// typo cannot claim ownership.
func projectImplementationRoleVocabulary(cfg *config.Config) []string {
	var roles []string
	if cfg != nil {
		for _, lane := range cfg.Lanes {
			if r := strings.TrimSpace(lane.Role); r != "" {
				roles = append(roles, r)
			}
			if n := strings.TrimSpace(lane.Name); n != "" {
				roles = append(roles, n)
			}
		}
	}
	for _, extra := range strings.Split(os.Getenv("HERD_IMPLEMENTATION_ROLES"), ",") {
		if e := strings.TrimSpace(extra); e != "" {
			roles = append(roles, e)
		}
	}
	return roles
}
