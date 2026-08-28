// Command binparity audits the checked-in Chainseer executable provenance
// manifest. The source tree is deliberately external to Herdforge; only its
// repo-relative identity and dispositions are committed here.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

const defaultManifest = "docs/architecture/chainseer-bin-parity.json"

const (
	exitSourceUnavailable = 2
	exitParityMismatch    = 3
	exitManifestInvalid   = 4
)

var errParityMismatch = errors.New("parity mismatch")

var allowedDisposition = map[string]bool{
	"herdforge_command_replacement": true,
	"generic_capability":            true,
	"chainseer_product_exemption":   true,
	"deprecated_wrapper":            true,
}

type Manifest struct {
	Version               int           `json:"version"`
	Task                  string        `json:"task"`
	SourceRoot            string        `json:"source_root"`
	SourceRevision        string        `json:"source_revision"`
	SourceExecutableCount int           `json:"source_executable_count"`
	Entries               []Disposition `json:"entries"`
}

type Disposition struct {
	Path              string   `json:"path"`
	Disposition       string   `json:"disposition"`
	Replacement       string   `json:"replacement,omitempty"`
	Ticket            string   `json:"ticket,omitempty"`
	CapabilityTickets []string `json:"capability_tickets,omitempty"`
	Rationale         string   `json:"rationale"`
}

func main() {
	fs := flag.NewFlagSet("binparity", flag.ExitOnError)
	manifestPath := fs.String("manifest", defaultManifest, "repo-relative parity manifest")
	sourcePath := fs.String("source", os.Getenv("CHAINSEER_BIN"), "Chainseer bin directory")
	fs.Parse(os.Args[1:])
	if strings.TrimSpace(*sourcePath) == "" {
		resolved, err := defaultSourcePath(context.Background(), ".")
		if err != nil {
			reportSourceUnavailable(err)
		}
		*sourcePath = resolved
	}
	if err := sourceDirectoryAvailable(*sourcePath); err != nil {
		reportSourceUnavailable(err)
	}
	m, err := readManifest(*manifestPath)
	if err == nil {
		err = validateManifest(m)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "binparity: MANIFEST_INVALID: %v\n", err)
		os.Exit(exitManifestInvalid)
	}
	if err := auditSource(*sourcePath, m); err != nil {
		if errors.Is(err, errParityMismatch) {
			fmt.Fprintf(os.Stderr, "binparity: PARITY_MISMATCH: %v\n", err)
			os.Exit(exitParityMismatch)
		}
		reportSourceUnavailable(err)
	}
	fmt.Printf("binparity: PASS: %d executable dispositions\n", len(m.Entries))
}

func defaultSourcePath(ctx context.Context, startDir string) (string, error) {
	projectRoot, _, err := gitroot.ProjectRoot(ctx, startDir)
	if err != nil {
		return "", fmt.Errorf("resolve Herdforge git root: %w", err)
	}
	return filepath.Join(filepath.Dir(projectRoot), "chainseer", "bin"), nil
}

func sourceDirectoryAvailable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("Chainseer source at %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("Chainseer source at %q is not a directory", path)
	}
	return nil
}

func reportSourceUnavailable(err error) {
	if os.Getenv("CHAINSEER_PARITY_OPTIONAL") == "1" {
		fmt.Printf("binparity: SKIP_SOURCE_UNAVAILABLE: %v\n", err)
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "binparity: SOURCE_UNAVAILABLE: %v (set CHAINSEER_BIN for an authorized source)\n", err)
	os.Exit(exitSourceUnavailable)
}

func readManifest(path string) (Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, fmt.Errorf("decode %s: %w", path, err)
	}
	return m, nil
}

func validateManifest(m Manifest) error {
	if m.Version != 1 || m.Task != "FAC-309" || m.SourceRoot != "chainseer/bin" {
		return errors.New("manifest header is invalid")
	}
	if m.SourceExecutableCount != len(m.Entries) {
		return fmt.Errorf("source_executable_count=%d but entries=%d", m.SourceExecutableCount, len(m.Entries))
	}
	seen := make(map[string]bool, len(m.Entries))
	for i, e := range m.Entries {
		if e.Path == "" || filepath.IsAbs(e.Path) || !strings.HasPrefix(e.Path, "bin/") || strings.Contains(e.Path, "..") {
			return fmt.Errorf("entry %d has invalid repo-relative path %q", i, e.Path)
		}
		if seen[e.Path] {
			return fmt.Errorf("duplicate entry %q", e.Path)
		}
		seen[e.Path] = true
		if !allowedDisposition[e.Disposition] || strings.TrimSpace(e.Rationale) == "" {
			return fmt.Errorf("entry %q has incomplete disposition", e.Path)
		}
		switch e.Disposition {
		case "herdforge_command_replacement":
			if e.Replacement == "" || !strings.HasPrefix(e.Replacement, "herd ") || !validTicket(e.Ticket) {
				return fmt.Errorf("replacement %q needs exact herd command and FAC ticket", e.Path)
			}
		case "generic_capability":
			if len(e.CapabilityTickets) == 0 {
				return fmt.Errorf("generic capability %q needs a capability or follow-up ticket", e.Path)
			}
			for _, t := range e.CapabilityTickets {
				if !validTicket(t) {
					return fmt.Errorf("generic capability %q has invalid ticket %q", e.Path, t)
				}
			}
		}
	}
	return nil
}

func validTicket(s string) bool {
	return len(s) > 4 && strings.HasPrefix(s, "FAC-") && s[4:] != "" && strings.Trim(s[4:], "0123456789") == ""
}

func auditSource(root string, m Manifest) error {
	files := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && info.Mode()&0111 != 0 {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, "bin/"+filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scan source %q: %w", root, err)
	}
	sort.Strings(files)
	manifest := make([]string, 0, len(m.Entries))
	for _, e := range m.Entries {
		manifest = append(manifest, e.Path)
	}
	sort.Strings(manifest)
	if len(files) != len(manifest) {
		return fmt.Errorf("%w: source has %d executables but manifest has %d", errParityMismatch, len(files), len(manifest))
	}
	for i := range files {
		if files[i] != manifest[i] {
			return fmt.Errorf("%w at %q vs %q", errParityMismatch, files[i], manifest[i])
		}
	}
	return nil
}
