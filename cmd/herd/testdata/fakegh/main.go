package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/Kampe/Herdforge/pkg/remoteci"
)

func main() {
	candidate := strings.TrimSpace(os.Getenv("HERD_FAKE_GH_CANDIDATE"))
	repository := strings.TrimSpace(os.Getenv("HERD_FAKE_GH_REPOSITORY"))
	checkName := strings.TrimSpace(os.Getenv("HERD_FAKE_GH_CHECK"))
	statePath := strings.TrimSpace(os.Getenv("HERD_FAKE_GH_STATE"))
	if candidate == "" || repository == "" || checkName == "" || statePath == "" {
		fmt.Fprintln(os.Stderr, "fake gh: fixture environment is incomplete")
		os.Exit(2)
	}
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "run" && args[1] == "list" {
		if !hasPair(args, "--commit", candidate) || !hasPair(args, "--repo", repository) {
			fmt.Fprintf(os.Stderr, "fake gh: exact run binding missing from argv: %q\n", args)
			os.Exit(3)
		}
		fmt.Printf("[{\"databaseId\":33463253256,\"attempt\":1,\"name\":\"CI Workflow\",\"headSha\":%q,\"status\":\"completed\",\"conclusion\":\"success\"}]\n", candidate)
		return
	}
	wantEndpoint, err := remoteci.GitHubAttemptJobsEndpoint(repository, 33463253256, 1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake gh: endpoint:", err)
		os.Exit(3)
	}
	if len(args) < 5 || args[0] != "api" || !hasValue(args, wantEndpoint) {
		fmt.Fprintf(os.Stderr, "fake gh: exact attempt jobs endpoint missing from argv: %q\n", args)
		os.Exit(3)
	}

	count := 0
	if data, err := os.ReadFile(statePath); err == nil {
		count, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	count++
	if err := os.WriteFile(statePath, []byte(strconv.Itoa(count)), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "fake gh: persist poll count:", err)
		os.Exit(4)
	}
	status, conclusion := "in_progress", ""
	if count > 1 {
		status, conclusion = "completed", "success"
	}
	fmt.Printf("{\"total_count\":1,\"jobs\":[{\"run_id\":33463253256,\"run_attempt\":1,\"name\":%q,\"head_sha\":%q,\"status\":%q,\"conclusion\":%q}]}\n", checkName, candidate, status, conclusion)
}

func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func hasValue(args []string, value string) bool {
	for _, arg := range args {
		if arg == value {
			return true
		}
	}
	return false
}
