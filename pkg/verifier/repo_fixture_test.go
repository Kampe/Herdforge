package verifier

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type cachedRepoFixture struct {
	dir  string
	head string
}

type repoFixtureCache struct {
	mu       sync.Mutex
	root     string
	fixtures map[string]cachedRepoFixture
}

var testRepoFixtures *repoFixtureCache

func TestMain(m *testing.M) {
	if err := fac151TestMainAdmission(); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	root, err := os.MkdirTemp("", "herdforge-verifier-fixtures-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create verifier fixture root: %v\n", err)
		os.Exit(1)
	}
	testRepoFixtures = &repoFixtureCache{
		root:     root,
		fixtures: make(map[string]cachedRepoFixture),
	}

	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintf(os.Stderr, "remove verifier fixture root: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

// copyCachedRepo builds each immutable Git fixture once per package process,
// then byte-copies it into a distinct t.TempDir for every test invocation.
// The copied .git directory, index, refs, and worktree are never shared, so a
// mutation or commit in one test cannot affect any later -count iteration.
func copyCachedRepo(t *testing.T, key string, build func(string) (string, error)) (string, string) {
	t.Helper()
	if testRepoFixtures == nil {
		t.Fatal("verifier repository fixture cache is not initialized")
	}

	fixture, err := testRepoFixtures.get(key, build)
	if err != nil {
		t.Fatalf("build repository fixture %q: %v", key, err)
	}
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS(fixture.dir)); err != nil {
		t.Fatalf("copy repository fixture %q: %v", key, err)
	}
	return dir, fixture.head
}

func (c *repoFixtureCache) get(key string, build func(string) (string, error)) (cachedRepoFixture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if fixture, ok := c.fixtures[key]; ok {
		return fixture, nil
	}

	dir, err := os.MkdirTemp(c.root, "repo-")
	if err != nil {
		return cachedRepoFixture{}, err
	}
	head, err := build(dir)
	if err != nil {
		cleanupErr := os.RemoveAll(dir)
		if cleanupErr != nil {
			return cachedRepoFixture{}, fmt.Errorf("%w (cleanup: %v)", err, cleanupErr)
		}
		return cachedRepoFixture{}, err
	}
	fixture := cachedRepoFixture{dir: dir, head: head}
	c.fixtures[key] = fixture
	return fixture, nil
}

func buildCompletionFixture(subjects []string) func(string) (string, error) {
	return func(dir string) (string, error) {
		if err := initializeFixtureRepo(dir); err != nil {
			return "", err
		}
		if err := fixtureGit(dir, "commit", "--allow-empty", "-q", "-m", "base"); err != nil {
			return "", err
		}
		base, err := fixtureGitOutput(dir, "rev-parse", "HEAD")
		if err != nil {
			return "", err
		}
		if err := fixtureGit(dir, "update-ref", "refs/remotes/origin/main", base); err != nil {
			return "", err
		}
		for _, subject := range subjects {
			if err := fixtureGit(dir, "commit", "--allow-empty", "-q", "-m", subject); err != nil {
				return "", err
			}
		}
		return fixtureGitOutput(dir, "rev-parse", "HEAD")
	}
}

func buildVerificationFixture(dir string) (string, error) {
	if err := initializeFixtureRepo(dir); err != nil {
		return "", err
	}
	files := []struct {
		name       string
		contents   string
		executable bool
	}{
		{name: "candidate.txt", contents: "original\n"},
		{name: "check.sh", contents: "#!/bin/sh\n[ \"$(cat candidate.txt)\" = \"original\" ]\n", executable: true},
		{name: "always-fail.sh", contents: "#!/bin/sh\nexit 7\n", executable: true},
		{name: "env-check.sh", contents: "#!/bin/sh\n[ -z \"${VERIFIER_AMBIENT_SECRET:-}\" ]\n", executable: true},
		{name: "dirty-check.sh", contents: "#!/bin/sh\ntouch post-run-dirty.txt\n", executable: true},
	}
	for _, file := range files {
		mode := fs.FileMode(0o644)
		if file.executable {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(dir, file.name), []byte(file.contents), mode); err != nil {
			return "", err
		}
	}
	return commitFixture(dir)
}

func buildRestorationFailureFixture(dir string) (string, error) {
	if err := initializeFixtureRepo(dir); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("original\n"), 0o644); err != nil {
		return "", err
	}
	check := "#!/bin/sh\ncount=$(cat \"$1\" 2>/dev/null || echo 0)\ncount=$((count + 1))\nprintf '%s\\n' \"$count\" > \"$1\"\nif [ \"$(cat candidate.txt)\" = \"mutant\" ]; then exit 1; fi\n[ \"$count\" -lt 3 ]\n"
	if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte(check), 0o755); err != nil {
		return "", err
	}
	return commitFixture(dir)
}

func buildMutationFixture(waits bool) func(string) (string, error) {
	return func(dir string) (string, error) {
		if err := initializeFixtureRepo(dir); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("original\n"), 0o644); err != nil {
			return "", err
		}
		wait := ""
		if waits {
			wait = "\nif [ \"$(cat candidate.txt)\" = \"mutant\" ]; then sleep 3; fi\n"
		}
		check := "#!/bin/sh\n" + wait + "[ \"$(cat candidate.txt)\" != \"mutant\" ]\n"
		if err := os.WriteFile(filepath.Join(dir, "check.sh"), []byte(check), 0o755); err != nil {
			return "", err
		}
		return commitFixture(dir)
	}
}

func initializeFixtureRepo(dir string) error {
	commands := [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.invalid"},
		{"config", "user.name", "verifier-test"},
		{"config", "commit.gpgsign", "false"},
	}
	for _, args := range commands {
		if err := fixtureGit(dir, args...); err != nil {
			return err
		}
	}
	return nil
}

func commitFixture(dir string) (string, error) {
	if err := fixtureGit(dir, "add", "."); err != nil {
		return "", err
	}
	if err := fixtureGit(dir, "commit", "-q", "-m", "candidate"); err != nil {
		return "", err
	}
	return fixtureGitOutput(dir, "rev-parse", "HEAD")
}

func fixtureGit(dir string, args ...string) error {
	_, err := runGit(dir, args...)
	return err
}

func fixtureGitOutput(dir string, args ...string) (string, error) {
	output, err := runGit(dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func fixturePassingReceipt(candidate string) Receipt {
	output := []byte("fixture verification output")
	result := &Result{
		Passed:       true,
		Outcome:      OutcomePASS,
		Output:       string(output),
		OutputDigest: digestBytes(output),
		ExitCode:     0,
	}
	return makeReceipt(VerificationRequest{
		TaskRef:           "FAC-122",
		LeaseGeneration:   "lease-7",
		CandidateSHA:      candidate,
		BaseSHA:           candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		Artifacts:         []string{"candidate.txt"},
	}, []string{"./check.sh"}, result, OutcomePASS)
}

func TestCachedRepoFixturesAreIndependent(t *testing.T) {
	first, firstHead := verificationRepo(t)
	if err := os.WriteFile(filepath.Join(first, "candidate.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second, secondHead := verificationRepo(t)
	if first == second {
		t.Fatal("cached repository copies must have distinct worktree paths")
	}
	if firstHead != secondHead {
		t.Fatalf("cached repository copies disagree on HEAD: %s != %s", firstHead, secondHead)
	}
	data, err := os.ReadFile(filepath.Join(second, "candidate.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "original\n" {
		t.Fatalf("first copy contaminated later fixture: %q", data)
	}
	if _, dirty, err := candidateState(second); err != nil {
		t.Fatal(err)
	} else if len(dirty) != 0 {
		t.Fatalf("later fixture copy is dirty: %v", dirty)
	}
}
