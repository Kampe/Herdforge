package classify

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// golden vectors: docs R0, bounded R1, feature/API R2, auth/secrets/destructive/core R3.
func TestClassify_GoldenVectors(t *testing.T) {
	sha := "abc123deadbeef"
	tests := []struct {
		name string
		in   Input
		want Tier
		// ruleMust contain at least one of these rule IDs in evidence
		ruleMust []string
		// ruleMustNot must not appear
		ruleMustNot []string
	}{
		{
			name: "docs-only R0",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"README.md", "docs/architecture/guide.md", "CHANGELOG.md"},
			},
			want:     TierR0,
			ruleMust: []string{"r0.docs"},
		},
		{
			name: "bounded tests-only R1",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"pkg/mail/mail_test.go", "pkg/config/testdata/sample.yaml"},
			},
			want:     TierR1,
			ruleMust: []string{"r1.tests"},
		},
		{
			name: "bounded tooling config R1",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{".editorconfig", ".gitignore", ".prettierrc"},
			},
			want:     TierR1,
			ruleMust: []string{"r1.config_bounded"},
		},
		{
			name: "feature production source R2",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"pkg/mail/mail.go", "pkg/config/config.go"},
			},
			want:     TierR2,
			ruleMust: []string{"r2.production_source"},
		},
		{
			name: "public API symbol R2",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"docs/note.md"},
				Symbols:      []string{"Classify", "Result"},
			},
			want:     TierR2,
			ruleMust: []string{"r2.public_api"},
		},
		{
			name: "dependency manifest R2",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"go.mod", "go.sum"},
			},
			want:     TierR2,
			ruleMust: []string{"r2.dependency_manifest"},
		},
		{
			name: "workflow config R2",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{".herd/herd.yaml", "Makefile"},
			},
			want:     TierR2,
			ruleMust: []string{"r2.workflow_config"},
		},
		{
			name: "auth secrets R3",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"pkg/auth/jwt.go", "internal/secret/manager.go"},
			},
			want:     TierR3,
			ruleMust: []string{"r3.auth_secrets"},
		},
		{
			name: "payment sensitive R3",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"pkg/payment/stripe.go"},
			},
			want:     TierR3,
			ruleMust: []string{"r3.auth_secrets"},
		},
		{
			name: "destructive ops R3",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"scripts/destroy_cluster.sh", "pkg/gc/force_delete.go"},
			},
			want:     TierR3,
			ruleMust: []string{"r3.destructive"},
		},
		{
			name: "core orchestration R3",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"pkg/daemon/forge.go", "pkg/dispatch/launch.go"},
			},
			want:     TierR3,
			ruleMust: []string{"r3.core_orchestration"},
		},
		{
			name: "infrastructure R3",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{"Dockerfile", "deploy/k8s/deployment.yaml", "terraform/main.tf"},
			},
			want:     TierR3,
			ruleMust: []string{"r3.infrastructure"},
		},
		{
			name: "CI workflows R3",
			in: Input{
				CandidateSHA: sha,
				Paths:        []string{".github/workflows/ci.yml"},
			},
			want:     TierR3,
			ruleMust: []string{"r3.ci_workflows"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.in)
			if got.Tier != tt.want {
				t.Fatalf("tier = %s, want %s; rules=%v reasons=%v", got.Tier, tt.want, ruleIDs(got), got.Reasons)
			}
			if got.ClassifierVersion != ClassifierVersion {
				t.Errorf("ClassifierVersion = %q, want %q", got.ClassifierVersion, ClassifierVersion)
			}
			if got.CandidateSHA != sha {
				t.Errorf("CandidateSHA = %q, want %q", got.CandidateSHA, sha)
			}
			if len(got.RequiredGates) == 0 {
				t.Error("RequiredGates empty")
			}
			ids := ruleIDs(got)
			for _, must := range tt.ruleMust {
				if !contains(ids, must) {
					t.Errorf("missing rule %q in %v", must, ids)
				}
			}
			for _, ban := range tt.ruleMustNot {
				if contains(ids, ban) {
					t.Errorf("unexpected rule %q in %v", ban, ids)
				}
			}
			// Mutation non-vacuity: if we forced a lower tier, golden would fail.
			if got.Tier.rank() < tt.want.rank() {
				t.Fatalf("non-vacuity: classifier under-ranked")
			}
		})
	}
}

func TestClassify_MixedDiffsSelectHighestTier(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  Tier
	}{
		{
			name:  "docs + tests = R1",
			paths: []string{"README.md", "pkg/mail/mail_test.go"},
			want:  TierR1,
		},
		{
			name:  "docs + feature = R2",
			paths: []string{"docs/guide.md", "pkg/mail/mail.go"},
			want:  TierR2,
		},
		{
			name:  "feature + auth = R3",
			paths: []string{"pkg/mail/mail.go", "pkg/auth/token.go"},
			want:  TierR3,
		},
		{
			name:  "tests + production = R2 not docs",
			paths: []string{"pkg/mail/mail_test.go", "pkg/mail/mail.go"},
			want:  TierR2,
		},
		{
			name:  "docs + go.mod = R2",
			paths: []string{"README.md", "go.mod"},
			want:  TierR2,
		},
		{
			name:  "all tiers present = R3",
			paths: []string{"README.md", "pkg/x/x_test.go", "pkg/x/x.go", "pkg/daemon/d.go"},
			want:  TierR3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(Input{CandidateSHA: "s1", Paths: tt.paths})
			if got.Tier != tt.want {
				t.Fatalf("tier = %s, want %s; rules=%v", got.Tier, tt.want, ruleIDs(got))
			}
		})
	}
}

func TestClassify_CannotMisclassifyAsDocsOnly(t *testing.T) {
	// Renames, generated, tests+prod, deps, config must never land on pure R0 docs-only.
	cases := []struct {
		name string
		in   Input
	}{
		{
			name: "rename production",
			in: Input{
				CandidateSHA: "s",
				Changes: []FileChange{
					{Kind: ChangeRename, OldPath: "pkg/mail/old.go", Path: "pkg/mail/new.go"},
				},
			},
		},
		{
			name: "generated only",
			in: Input{
				CandidateSHA: "s",
				Paths:        []string{"pkg/api/v1/api.pb.go", "generated/types.go"},
			},
		},
		{
			name: "tests with production",
			in: Input{
				CandidateSHA: "s",
				Paths:        []string{"pkg/foo/foo_test.go", "pkg/foo/foo.go"},
			},
		},
		{
			name: "dependency change",
			in: Input{
				CandidateSHA: "s",
				Paths:        []string{"package.json", "package-lock.json"},
			},
		},
		{
			name: "config change herd.yaml",
			in: Input{
				CandidateSHA: "s",
				Paths:        []string{".herd/herd.yaml"},
			},
		},
		{
			name: "bounded config",
			in: Input{
				CandidateSHA: "s",
				Paths:        []string{".golangci.yml"},
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.in)
			if got.Tier == TierR0 {
				t.Fatalf("misclassified as R0 docs-only; rules=%v reasons=%v", ruleIDs(got), got.Reasons)
			}
			// Must have non-docs evidence
			onlyDocs := true
			for _, r := range got.Rules {
				if r.ID != "r0.docs" && r.ID != "r0.mechanical" {
					onlyDocs = false
					break
				}
			}
			if onlyDocs {
				t.Fatalf("only docs/mechanical rules fired: %v", ruleIDs(got))
			}
		})
	}
}

func TestClassify_UnknownPathsConservative(t *testing.T) {
	got := Classify(Input{
		CandidateSHA: "s",
		Paths:        []string{"weird/blob.dat", "mystery.bin"},
	})
	if got.Tier.rank() < TierR2.rank() {
		t.Fatalf("unknown paths must fail upward to >=R2, got %s", got.Tier)
	}
	if !contains(ruleIDs(got), "unknown.path_conservative") {
		t.Fatalf("expected unknown.path_conservative, got %v", ruleIDs(got))
	}
	if len(got.Reasons) == 0 {
		t.Fatal("expected reason evidence")
	}
}

func TestClassify_EmptyScopeFailsUpward(t *testing.T) {
	got := Classify(Input{CandidateSHA: "s"})
	if got.Tier != TierR2 {
		t.Fatalf("empty scope tier = %s, want R2", got.Tier)
	}
	if !contains(ruleIDs(got), "unknown.conservative") {
		t.Fatalf("rules = %v", ruleIDs(got))
	}
}

func TestClassify_OrderIndependence(t *testing.T) {
	a := []string{"pkg/auth/a.go", "README.md", "pkg/mail/m.go", "go.mod"}
	b := []string{"go.mod", "pkg/mail/m.go", "README.md", "pkg/auth/a.go"}
	ra := Classify(Input{CandidateSHA: "same", PatchID: "p1", Paths: a, Symbols: []string{"Zed", "Alpha"}})
	rb := Classify(Input{CandidateSHA: "same", PatchID: "p1", Paths: b, Symbols: []string{"Alpha", "Zed"}})

	if ra.Tier != rb.Tier {
		t.Fatalf("tier order-dependent: %s vs %s", ra.Tier, rb.Tier)
	}
	if !reflect.DeepEqual(ra.ChangedPaths, rb.ChangedPaths) {
		t.Fatalf("paths not normalized: %v vs %v", ra.ChangedPaths, rb.ChangedPaths)
	}
	if !reflect.DeepEqual(ruleIDs(ra), ruleIDs(rb)) {
		t.Fatalf("rule IDs order-dependent: %v vs %v", ruleIDs(ra), ruleIDs(rb))
	}
	if !reflect.DeepEqual(ra.ChangedSymbols, rb.ChangedSymbols) {
		t.Fatalf("symbols not sorted: %v vs %v", ra.ChangedSymbols, rb.ChangedSymbols)
	}
	ja, _ := json.Marshal(ra)
	jb, _ := json.Marshal(rb)
	if string(ja) != string(jb) {
		t.Fatalf("JSON not stable across input order\nA=%s\nB=%s", ja, jb)
	}
}

func TestClassify_SHABindingAndInvalidation(t *testing.T) {
	r := Classify(Input{
		CandidateSHA: "sha-aaa",
		PatchID:      "patch-1",
		Paths:        []string{"pkg/mail/mail.go"},
	})
	if !r.ValidFor("sha-aaa", "patch-1") {
		t.Fatal("expected valid for exact SHA+patch")
	}
	if r.ValidFor("sha-bbb", "patch-1") {
		t.Fatal("must invalidate on SHA change")
	}
	if r.ValidFor("sha-aaa", "patch-2") {
		t.Fatal("must invalidate on patch ID change")
	}
	if r.ValidFor("", "patch-1") {
		t.Fatal("empty SHA must not validate")
	}
	// Empty result SHA is never valid.
	empty := Result{CandidateSHA: "", ClassifierVersion: ClassifierVersion}
	if empty.ValidFor("sha-aaa", "") {
		t.Fatal("empty bound SHA must not validate")
	}
}

func TestClassify_PolicyExtensions(t *testing.T) {
	pol := Policy{
		ExtraR3Substrings: []string{"ledger"},
		ExtraR2Substrings: []string{"experimental"},
	}
	got := Classify(Input{
		CandidateSHA: "s",
		Paths:        []string{"docs/ledger-notes.md"},
		Policy:       &pol,
	})
	// docs alone would be R0, policy elevates to R3
	if got.Tier != TierR3 {
		t.Fatalf("policy R3 elevates docs path, got %s rules=%v", got.Tier, ruleIDs(got))
	}
	if !contains(ruleIDs(got), "policy.extra_r3") {
		t.Fatalf("missing policy.extra_r3 in %v", ruleIDs(got))
	}
}

func TestClassify_GatesByTier(t *testing.T) {
	if !contains(GatesFor(TierR0), "mechanical_review_optional") {
		t.Error("R0 gates")
	}
	if !contains(GatesFor(TierR1), "different_family_review") {
		t.Error("R1 gates")
	}
	if !contains(GatesFor(TierR2), "integration_rerun") {
		t.Error("R2 gates")
	}
	r3 := GatesFor(TierR3)
	if !contains(r3, "security_capable_review") || !contains(r3, "high_risk_explicit_gates") {
		t.Errorf("R3 gates = %v", r3)
	}
}

func TestClassify_MachineReadableJSON(t *testing.T) {
	r := Classify(Input{
		CandidateSHA: "deadbeef",
		PatchID:      "pid-9",
		Paths:        []string{"pkg/auth/x.go", "README.md"},
	})
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tier", "rules", "changed_paths", "required_gates", "classifier_version", "candidate_sha", "reasons"} {
		if _, ok := m[key]; !ok {
			t.Errorf("missing JSON key %q in %s", key, b)
		}
	}
	if m["tier"] != "R3" {
		t.Errorf("tier json = %v", m["tier"])
	}
	if m["classifier_version"] != ClassifierVersion {
		t.Errorf("version = %v", m["classifier_version"])
	}
}

// TestClassify_MutationWouldFail ensures golden expectations are non-vacuous:
// forcing docs-only paths to be scored as production would change the R0 vector.
func TestClassify_MutationWouldFail(t *testing.T) {
	docs := Classify(Input{CandidateSHA: "s", Paths: []string{"README.md"}})
	prod := Classify(Input{CandidateSHA: "s", Paths: []string{"pkg/foo/foo.go"}})
	if docs.Tier == prod.Tier {
		t.Fatal("docs and production must classify differently; test would be vacuous if equal")
	}
	if docs.Tier != TierR0 || prod.Tier != TierR2 {
		t.Fatalf("expected R0 vs R2, got %s vs %s", docs.Tier, prod.Tier)
	}
	// If someone mutated Max to always return R0, mixed auth would fail here.
	mixed := Classify(Input{CandidateSHA: "s", Paths: []string{"README.md", "pkg/auth/a.go"}})
	if mixed.Tier != TierR3 {
		t.Fatalf("mutation guard: mixed auth must be R3, got %s", mixed.Tier)
	}
}

func TestMax(t *testing.T) {
	if Max(TierR0, TierR2) != TierR2 {
		t.Fatal("Max R0,R2")
	}
	if Max(TierR3, TierR1) != TierR3 {
		t.Fatal("Max R3,R1")
	}
	if Max(TierR2, TierR2) != TierR2 {
		t.Fatal("Max equal")
	}
}

func TestNormalizePaths_StripDotSlashAndSlashes(t *testing.T) {
	got := Classify(Input{
		CandidateSHA: "s",
		Paths:        []string{"./pkg/mail/mail.go", `pkg\mail\other.go`},
	})
	for _, p := range got.ChangedPaths {
		if strings.HasPrefix(p, "./") {
			t.Errorf("path not normalized: %q", p)
		}
		if strings.Contains(p, "\\") {
			t.Errorf("backslash remains: %q", p)
		}
	}
	if got.Tier != TierR2 {
		t.Fatalf("tier = %s", got.Tier)
	}
}

func ruleIDs(r Result) []string {
	out := make([]string, len(r.Rules))
	for i, rm := range r.Rules {
		out[i] = rm.ID
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
