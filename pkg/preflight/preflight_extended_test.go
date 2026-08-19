package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckWorktreeBoundary_SkipsGitDir(t *testing.T) {
	tmpDir := t.TempDir()
	// Create .git directory — should be skipped entirely
	os.MkdirAll(filepath.Join(tmpDir, ".git"), 0755)
	os.WriteFile(filepath.Join(tmpDir, ".git", "config"), []byte("path: /Users/evil"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected no leak detection inside .git, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsNodeModules(t *testing.T) {
	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, "node_modules"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "node_modules", "pkg"), []byte("path: /Users/secret"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected no leak detection inside node_modules, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsVendor(t *testing.T) {
	tmpDir := t.TempDir()
	os.MkdirAll(filepath.Join(tmpDir, "vendor"), 0755)
	os.WriteFile(filepath.Join(tmpDir, "vendor", "lib"), []byte("path: /Users/secret"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected no leak detection inside vendor, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsGeneratedHerdBootstrap(t *testing.T) {
	tmpDir := t.TempDir()
	bootstrap := filepath.Join(tmpDir, ".herd", "bootstrap", "cache", "module.go")
	if err := os.MkdirAll(filepath.Dir(bootstrap), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrap, []byte("// generated cache: /Users/toolchain\npackage cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckWorktreeBoundary(tmpDir); err != nil {
		t.Fatalf("expected generated .herd/bootstrap cache to be skipped, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_IgnoresNonCheckedExts(t *testing.T) {
	tmpDir := t.TempDir()
	// .png files with absolute paths should be ignored
	os.WriteFile(filepath.Join(tmpDir, "image.png"), []byte("/Users/something"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected no leak detection for non-checked extension, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_ReadError(t *testing.T) {
	tmpDir := t.TempDir()
	// Create a dir that looks like a file extension — walk error testing
	os.MkdirAll(filepath.Join(tmpDir, "subdir"), 0755)
	os.Chmod(filepath.Join(tmpDir, "subdir"), 0000)
	defer os.Chmod(filepath.Join(tmpDir, "subdir"), 0755)

	// Put a .go file in it
	goFile := filepath.Join(tmpDir, "subdir", "test.go")
	os.WriteFile(goFile, []byte("package x"), 0644)
	os.Chmod(goFile, 0000)
	defer os.Chmod(goFile, 0644)

	// This should not panic — even if walk can't read a file inside
	err := CheckWorktreeBoundary(tmpDir)
	_ = err // walk may or may not return an error depending on OS; we just verify no panic
}

func TestCheckWorktreeBoundary_WalkError(t *testing.T) {
	err := CheckWorktreeBoundary("/nonexistent-path-xyzzy")
	if err == nil {
		t.Fatal("expected walk error for nonexistent path")
	}
}

func TestCheckWorktreeBoundary_DetectLeak(t *testing.T) {
	tmpDir := t.TempDir()
	goFile := filepath.Join(tmpDir, "leak.go")
	os.WriteFile(goFile, []byte("// path: /Users/evil/secret\npackage x\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err == nil {
		t.Fatal("expected leak detection for /Users/ path in .go file")
	}
}

func TestCheckWorktreeBoundary_DetectHomeLeak(t *testing.T) {
	tmpDir := t.TempDir()
	yamlFile := filepath.Join(tmpDir, "config.yaml")
	os.WriteFile(yamlFile, []byte("path: /home/ec2-user/secret\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err == nil {
		t.Fatal("expected leak detection for /home/ path in .yaml file")
	}
}

func TestCheckWorktreeBoundary_URLPathSegmentsAreNotLeaks(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{
			name:    "URL home segment",
			content: "docs: https://docs.example.org/x/home/guide\n",
			wantErr: false,
		},
		{
			name:    "URL Users segment",
			content: "docs: https://docs.example.org/Users/guide\n",
			wantErr: false,
		},
		{
			name:    "bare domain home segment",
			content: "see docs.m0.org/home/resources/addresses/\n",
			wantErr: false,
		},
		{
			name:    "bare home path",
			content: "path: /home/ec2-user/secret\n",
			wantErr: true,
		},
		{
			name:    "bare Users path",
			content: "path: /Users/kampe/secret\n",
			wantErr: true,
		},
		{name: "embedded Users path segment", content: "citation: vendor/Users/kampe/guide\n", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(tmpDir, "config.yaml"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}

			err := CheckWorktreeBoundary(tmpDir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckWorktreeBoundary() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCheckWorktreeBoundaryChanged_ScopesToChangedFiles(t *testing.T) {
	tmpDir := t.TempDir()
	runGitDivergenceTest(t, tmpDir, "init")
	runGitDivergenceTest(t, tmpDir, "config", "user.email", "test@example.com")
	runGitDivergenceTest(t, tmpDir, "config", "user.name", "Preflight Test")
	writeDivergenceTestFile(t, tmpDir, "base")
	if err := os.WriteFile(filepath.Join(tmpDir, "unrelated.yaml"), []byte("path: /Users/other/secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitDivergenceTest(t, tmpDir, "add", ".")
	runGitDivergenceTest(t, tmpDir, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(tmpDir, "unrelated.yaml"), []byte("path: /Users/other/secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "touched.yaml"), []byte("path: /Users/lane/secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckWorktreeBoundaryChanged(tmpDir, nil); err == nil {
		t.Fatal("changed-file scan accepted touched leak")
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "touched.yaml"), []byte("path: ./safe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckWorktreeBoundaryChanged(tmpDir, nil); err != nil {
		t.Fatalf("unrelated leak failed scan: %v", err)
	}
}

func TestIsURLPathSegment_BareDomain(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{
			name: "bare domain",
			line: "see docs.m0.org/home/resources/addresses/",
			want: true,
		},
		{
			name: "scheme URL",
			line: "see https://docs.example.org/home/resources/",
			want: true,
		},
		{
			name: "host path",
			line: "path: /home/ec2-user/secret",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index := strings.Index(tt.line, "/home/")
			if index < 0 {
				t.Fatalf("test input does not contain marker: %q", tt.line)
			}
			if got := isURLPathSegment(tt.line, index); got != tt.want {
				t.Fatalf("isURLPathSegment() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCheckWorktreeBoundary_AllowsRepoDeclaredFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "docs", "runbook.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("checkout: /Users/operator/repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := CheckWorktreeBoundaryWithAllowlist(tmpDir, []string{"docs/runbook.md"}); err != nil {
		t.Fatalf("declared relative allowlist should admit the file: %v", err)
	}
}

func TestCheckWorktreeBoundary_AllowlistCannotEscapeRoot(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "docs", "runbook.md")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("checkout: /Users/operator/repo\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := CheckWorktreeBoundaryWithAllowlist(tmpDir, []string{"../**"}); err == nil {
		t.Fatal("parent traversal allowlist must not suppress a leak")
	}
}

func TestCheckWorktreeBoundary_SkipsPreflightTest(t *testing.T) {
	tmpDir := t.TempDir()
	// File named preflight_something_test.go should be skipped even with /Users/ content
	testFile := filepath.Join(tmpDir, "preflight_helper_test.go")
	os.WriteFile(testFile, []byte("// path: /Users/test\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected preflight test file to be skipped, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsAGENTSMD(t *testing.T) {
	tmpDir := t.TempDir()
	agentFile := filepath.Join(tmpDir, "AGENTS.md")
	os.WriteFile(agentFile, []byte("path: /Users/something\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected AGENTS.md to be skipped, got: %v", err)
	}
}

func TestCheckWorktreeBoundary_SkipsPreflightSource(t *testing.T) {
	tmpDir := t.TempDir()
	srcFile := filepath.Join(tmpDir, "preflight.go")
	os.WriteFile(srcFile, []byte("path: /Users/something\n"), 0644)

	err := CheckWorktreeBoundary(tmpDir)
	if err != nil {
		t.Fatalf("expected preflight.go to be skipped, got: %v", err)
	}
}
