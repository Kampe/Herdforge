package main

import (
	"flag"
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/beat"
)

func runIntegrationWakeAck(args []string) error {
	fs := flag.NewFlagSet("integration-wake", flag.ContinueOnError)
	sha := fs.String("candidate", "", "exact candidate SHA")
	generation := fs.Int64("generation", 0, "exact delivered wake generation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *sha == "" || *generation <= 0 {
		return fmt.Errorf("integration-wake requires --candidate and a positive --generation, with no positional arguments")
	}
	root, err := canonicalHerdRoot()
	if err != nil {
		return err
	}
	if err = beat.AcknowledgeIntegrationWake(integrationWakePath(root), *sha, *generation, time.Now().UTC()); err != nil {
		return err
	}
	fmt.Printf("integration wake %s generation %d acknowledged; no merge or board action performed\n", *sha, *generation)
	return nil
}
