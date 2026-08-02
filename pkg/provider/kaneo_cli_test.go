package provider

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestKaneoCLI_ListTasksPaginates proves ListTasks walks pages: the server
// caps a page at 100 records, and a board past 100 cards must not lose its
// tail (unclaimed to-do cards past the cap would be invisible to pulse).
func TestKaneoCLI_ListTasksPaginates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH shell-stub test is POSIX-only")
	}

	dir := t.TempDir()
	// Fake kaneo: 100 tasks on page 1, 5 on page 2, empty after.
	stub := `#!/bin/sh
page=1
prev=""
for a in "$@"; do
  if [ "$prev" = "--page" ]; then page="$a"; fi
  prev="$a"
done
case "$page" in
  1) n=100; start=1 ;;
  2) n=5; start=101 ;;
  *) echo "[]"; exit 0 ;;
esac
printf "["
i=0
while [ $i -lt $n ]; do
  ref=$((start + i))
  [ $i -gt 0 ] && printf ","
  printf "{\"id\":\"id-%d\",\"ref\":\"FAC-%d\",\"title\":\"t\",\"status\":\"to-do\",\"priority\":\"low\",\"projectId\":\"p1\",\"labels\":[]}" "$ref" "$ref"
  i=$((i + 1))
done
printf "]"
`
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	kp := NewKaneoProvider("", "p1", true)
	tasks, err := kp.ListTasks(context.Background(), "p1", "")
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 105 {
		t.Fatalf("got %d tasks, want 105 (page 2 tail lost)", len(tasks))
	}
	want := fmt.Sprintf("FAC-%d", 105)
	if tasks[len(tasks)-1].Ref != want {
		t.Fatalf("last task = %s, want %s", tasks[len(tasks)-1].Ref, want)
	}
}
