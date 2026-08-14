package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Kampe/Herdforge/pkg/timeline"
)

func runTimeline() { os.Exit(runTimelineCommand(os.Args[2:], os.Stdout, os.Stderr)) }

// runTimelineCommand is intentionally read-only. The execution timeline is a
// secondary observation log, never a lifecycle-state mutation path.
func runTimelineCommand(args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("timeline", flag.ContinueOnError)
	fs.SetOutput(errOut)
	path := fs.String("file", ".herd/execution-events.jsonl", "Timeline JSONL path")
	buildRun := fs.String("build-run", "", "Filter by build run")
	task := fs.String("task", "", "Filter by task")
	lane := fs.String("lane", "", "Filter by lane")
	session := fs.String("session", "", "Filter by session")
	model := fs.String("model", "", "Filter by model")
	provider := fs.String("provider", "", "Filter by provider")
	source := fs.String("source", "", "Filter by source")
	typeName := fs.String("type", "", "Filter by event type")
	after := fs.String("after", "", "Exclusive RFC3339 timestamp")
	before := fs.String("before", "", "Exclusive RFC3339 timestamp")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		if err == nil {
			fmt.Fprintln(errOut, "herd timeline: positional arguments are not supported")
		}
		return 2
	}
	filter := timeline.Filter{BuildRun: *buildRun, Task: *task, Lane: *lane, Session: *session, Model: *model, Provider: *provider, Source: *source, Type: *typeName}
	var err error
	if *after != "" {
		filter.After, err = time.Parse(time.RFC3339, *after)
		if err != nil {
			fmt.Fprintf(errOut, "herd timeline: invalid --after: %v\n", err)
			return 2
		}
	}
	if *before != "" {
		filter.Before, err = time.Parse(time.RFC3339, *before)
		if err != nil {
			fmt.Fprintf(errOut, "herd timeline: invalid --before: %v\n", err)
			return 2
		}
	}
	store, err := timeline.Open(*path)
	if err != nil {
		fmt.Fprintf(errOut, "herd timeline: %v\n", err)
		return 1
	}
	events, err := store.Read(filter)
	if err != nil {
		fmt.Fprintf(errOut, "herd timeline: %v\n", err)
		return 1
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(events); err != nil {
		fmt.Fprintf(errOut, "herd timeline: encode: %v\n", err)
		return 1
	}
	return 0
}
