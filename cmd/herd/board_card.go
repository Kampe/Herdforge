package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

const boardCardUsage = `Usage: herd board-card <create|get|list|status> [flags]
  Coordinator-owned CRUD for task_provider.type=local.
  Reuses NewFromHerdConfig; refused for Kaneo/Linear/Jira/memory.
  Store: ./.herd/local-board/tasks.json (never escapes .herd).
  Flags may appear before or after positionals. Extra args are errors.`

func runBoardCard() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, boardCardUsage)
		os.Exit(2)
	}
	sub := os.Args[2]
	if sub == "-h" || sub == "--help" || sub == "help" {
		fmt.Println(boardCardUsage)
		return
	}
	cfg, err := config.LoadConfig(config.PathFor(""))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card: %v\n", err)
		os.Exit(1)
	}
	if strings.ToLower(strings.TrimSpace(cfg.TaskProvider.Type)) != "local" {
		fmt.Fprintf(os.Stderr, "herd board-card: task_provider.type is %q, not local\n", cfg.TaskProvider.Type)
		os.Exit(1)
	}
	tp, err := provider.NewFromHerdConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card: %v\n", err)
		os.Exit(1)
	}
	ctx := context.Background()
	switch sub {
	case "create":
		runBoardCardCreate(ctx, tp, cfg, os.Args[3:])
	case "get":
		runBoardCardGet(ctx, tp, os.Args[3:])
	case "list":
		runBoardCardList(ctx, tp, cfg, os.Args[3:])
	case "status":
		runBoardCardStatus(ctx, tp, os.Args[3:])
	default:
		fmt.Fprintf(os.Stderr, "herd board-card: unknown subcommand %q\n%s\n", sub, boardCardUsage)
		os.Exit(2)
	}
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func parseBoardCardFlags(name string, args []string, fs *flag.FlagSet) ([]string, error) {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				if takesValue(fs, a) {
					i++
					flags = append(flags, args[i])
				}
			}
			continue
		}
		pos = append(pos, a)
	}
	if err := fs.Parse(flags); err != nil {
		return nil, err
	}
	if fs.NArg() != 0 {
		return nil, fmt.Errorf("%s: unexpected extra args %q", name, fs.Args())
	}
	return pos, nil
}

func takesValue(fs *flag.FlagSet, name string) bool {
	n := strings.TrimLeft(name, "-")
	if i := strings.IndexByte(n, '='); i >= 0 {
		n = n[:i]
	}
	var takes bool
	fs.VisitAll(func(f *flag.Flag) {
		if f.Name == n {
			if _, isBool := f.Value.(interface{ IsBoolFlag() bool }); !isBool {
				takes = true
			}
		}
	})
	return takes
}

func runBoardCardCreate(ctx context.Context, tp provider.TaskProvider, cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("board-card create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	title := fs.String("title", "", "card title (required)")
	project := fs.String("project", "", "project id (defaults to task_provider.project_id)")
	ref := fs.String("ref", "", "stable task ref (optional)")
	prio := fs.String("priority", "", "urgent|high|medium|low")
	desc := fs.String("description", "", "card description/scope")
	asJSON := fs.Bool("json", false, "emit JSON")
	var labels stringList
	fs.Var(&labels, "label", "repeatable label (role, bounded, scope, ...)")
	pos, err := parseBoardCardFlags("board-card create", args, fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card create: %v\n", err)
		os.Exit(2)
	}
	if len(pos) != 0 {
		fmt.Fprintf(os.Stderr, "herd board-card create: unexpected extra args %q\n", pos)
		os.Exit(2)
	}
	if strings.TrimSpace(*title) == "" {
		fmt.Fprintln(os.Stderr, "usage: herd board-card create --title TEXT [--project ID] [--ref REF] [--priority P] [--description TEXT] [--label NAME]... [--json]")
		os.Exit(2)
	}
	proj := strings.TrimSpace(*project)
	if proj == "" {
		proj = strings.TrimSpace(cfg.TaskProvider.ProjectID)
	}
	creator, ok := tp.(provider.TaskCreator)
	if !ok {
		fmt.Fprintln(os.Stderr, "herd board-card create: provider does not support task creation")
		os.Exit(1)
	}
	created, err := creator.CreateTask(ctx, &provider.Task{
		Title:       strings.TrimSpace(*title),
		ProjectID:   proj,
		Ref:         strings.TrimSpace(*ref),
		Priority:    provider.Priority(strings.ToLower(strings.TrimSpace(*prio))),
		Description: strings.TrimSpace(*desc),
		Labels:      append([]string(nil), labels...),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card create: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		enc, _ := json.MarshalIndent(created, "", "  ")
		fmt.Println(string(enc))
		return
	}
	fmt.Printf("herd board-card: created %s ref=%s status=%s\n", created.ID, created.Ref, created.Status)
}

func runBoardCardGet(ctx context.Context, tp provider.TaskProvider, args []string) {
	fs := flag.NewFlagSet("board-card get", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseBoardCardFlags("board-card get", args, fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card get: %v\n", err)
		os.Exit(2)
	}
	if len(pos) != 1 {
		fmt.Fprintln(os.Stderr, "usage: herd board-card get [--json] <id-or-ref>")
		os.Exit(2)
	}
	task, err := tp.GetTask(ctx, pos[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card get: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		enc, _ := json.MarshalIndent(task, "", "  ")
		fmt.Println(string(enc))
		return
	}
	fmt.Printf("%s\t%s\t%s\t%s\t%s\n", task.ID, task.Ref, task.Status, task.Priority, task.Title)
}

func runBoardCardList(ctx context.Context, tp provider.TaskProvider, cfg *config.Config, args []string) {
	fs := flag.NewFlagSet("board-card list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	project := fs.String("project", "", "project filter (defaults to task_provider.project_id)")
	status := fs.String("status", "", "status filter")
	asJSON := fs.Bool("json", false, "emit JSON")
	pos, err := parseBoardCardFlags("board-card list", args, fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card list: %v\n", err)
		os.Exit(2)
	}
	if len(pos) != 0 {
		fmt.Fprintf(os.Stderr, "herd board-card list: unexpected extra args %q\n", pos)
		os.Exit(2)
	}
	proj := strings.TrimSpace(*project)
	if proj == "" {
		proj = strings.TrimSpace(cfg.TaskProvider.ProjectID)
	}
	tasks, err := tp.ListTasks(ctx, proj, strings.TrimSpace(*status))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card list: %v\n", err)
		os.Exit(1)
	}
	if *asJSON {
		enc, _ := json.MarshalIndent(tasks, "", "  ")
		fmt.Println(string(enc))
		return
	}
	for _, task := range tasks {
		fmt.Printf("%s\t%s\t%s\t%s\t%s\n", task.ID, task.Ref, task.Status, task.Priority, task.Title)
	}
}

func runBoardCardStatus(ctx context.Context, tp provider.TaskProvider, args []string) {
	fs := flag.NewFlagSet("board-card status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	pos, err := parseBoardCardFlags("board-card status", args, fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card status: %v\n", err)
		os.Exit(2)
	}
	if len(pos) != 2 {
		fmt.Fprintln(os.Stderr, "usage: herd board-card status <id-or-ref> <status>")
		os.Exit(2)
	}
	if err := tp.UpdateStatus(ctx, pos[0], pos[1]); err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card status: %v\n", err)
		os.Exit(1)
	}
	got, err := tp.GetTask(ctx, pos[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd board-card status: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd board-card: %s status=%s\n", got.ID, got.Status)
}
