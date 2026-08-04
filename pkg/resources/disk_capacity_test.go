package resources

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

type diskFake struct {
	values map[string]Capacity
	err    error
	calls  []string
}

func (f *diskFake) StatFS(path string) (Capacity, error) {
	f.calls = append(f.calls, path)
	if f.err != nil {
		return Capacity{}, f.err
	}
	return f.values[path], nil
}
func diskHealthy(id string) Capacity {
	return Capacity{FilesystemID: id, TotalBytes: 1000, FreeBytes: 500, TotalInodes: 100, FreeInodes: 80}
}
func diskPolicy() DiskPolicy {
	return DiskPolicy{ReserveBytes: 100, ReservePercent: 20, ReserveInodes: 10}
}

func TestEvaluateDiskCapacityThresholdsAndInodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		c      Capacity
		r      DiskRequest
		state  DiskState
		reason string
	}{
		{"bytes", Capacity{FilesystemID: "fs", TotalBytes: 1000, FreeBytes: 150, TotalInodes: 100, FreeInodes: 80}, DiskRequest{Path: ".", RequiredBytes: 100}, DiskBlocked, DiskReasonBelowThreshold},
		{"percent", Capacity{FilesystemID: "fs", TotalBytes: 1000, FreeBytes: 150, TotalInodes: 100, FreeInodes: 80}, DiskRequest{Path: "."}, DiskBlocked, DiskReasonBelowThreshold},
		{"inodes", Capacity{FilesystemID: "fs", TotalBytes: 1000, FreeBytes: 500, TotalInodes: 100, FreeInodes: 9}, DiskRequest{Path: "."}, DiskBlocked, DiskReasonInodeExhaustion},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateDiskCapacity(&diskFake{values: map[string]Capacity{".": tc.c}}, tc.r, diskPolicy())
			if got.State != tc.state || got.Evidence.Reason != tc.reason {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestEvaluateDiskCapacityHysteresisRecoveryAndTempVolume(t *testing.T) {
	p := diskPolicy()
	p.RecoveryBytes, p.RecoveryPercent, p.RecoveryInodes = 600, 60, 60
	f := &diskFake{values: map[string]Capacity{"repo": diskHealthy("repo"), "tmp": {FilesystemID: "tmp", TotalBytes: 1000, FreeBytes: 150, TotalInodes: 100, FreeInodes: 80}}}
	got := EvaluateDiskCapacity(f, DiskRequest{Path: "repo", TempPath: "tmp", PreviouslyBlocked: true}, p)
	if got.State != DiskBlocked || got.Evidence.Reason != DiskReasonHysteresis {
		t.Fatalf("hysteresis got %+v", got)
	}
	f.values["repo"] = Capacity{FilesystemID: "repo", TotalBytes: 1000, FreeBytes: 700, TotalInodes: 100, FreeInodes: 80}
	got = EvaluateDiskCapacity(f, DiskRequest{Path: "repo", TempPath: "tmp"}, p)
	if got.State != DiskBlocked || got.Evidence.Reason != DiskReasonTempVolumeDivergence {
		t.Fatalf("temp divergence got %+v", got)
	}
	f.values["tmp"] = Capacity{FilesystemID: "tmp", TotalBytes: 1000, FreeBytes: 700, TotalInodes: 100, FreeInodes: 80}
	got = EvaluateDiskCapacity(f, DiskRequest{Path: "repo", TempPath: "tmp"}, p)
	if !got.Allowed || got.State != DiskReady {
		t.Fatalf("recovery got %+v", got)
	}
}

func TestEvaluateDiskCapacityFailsClosedAndEvidenceBounded(t *testing.T) {
	for name, b := range map[string]StatFSBackend{"nil": nil, "error": &diskFake{err: errors.New("probe")}, "invalid": &diskFake{values: map[string]Capacity{".": {TotalBytes: 0, TotalInodes: 1, FreeInodes: 1}}}} {
		t.Run(name, func(t *testing.T) {
			got := EvaluateDiskCapacity(b, DiskRequest{Path: ".", Operation: "bad op secret/volume"}, diskPolicy())
			if got.Allowed || got.State != DiskBlocked {
				t.Fatalf("not fail closed: %+v", got)
			}
			want := DiskReasonUnavailable
			if name == "invalid" {
				want = DiskReasonInvalid
			}
			if got.Evidence.Reason != want {
				t.Fatalf("reason = %q, want %q", got.Evidence.Reason, want)
			}
		})
	}
	f := &diskFake{values: map[string]Capacity{".": diskHealthy("secret/volume")}}
	got := EvaluateDiskCapacity(f, DiskRequest{Path: ".", Operation: "bad op secret/volume"}, diskPolicy())
	if got.Evidence.FilesystemID != "opaque:f82a67b36a2b" || got.Evidence.Operation != "unknown" || len(got.Evidence.Operation) > 32 {
		t.Fatalf("unsafe evidence: %+v", got.Evidence)
	}
}

func TestEvaluateDiskCapacityDenialProbesRootOnly(t *testing.T) {
	f := &diskFake{values: map[string]Capacity{"repo": {FilesystemID: "repo", TotalBytes: 1000, FreeBytes: 1, TotalInodes: 100, FreeInodes: 80}, "tmp": diskHealthy("tmp")}}
	got := EvaluateDiskCapacity(f, DiskRequest{Path: "repo", TempPath: "tmp", Operation: "build"}, diskPolicy())
	if got.Allowed || got.Evidence.Reason != DiskReasonBelowThreshold {
		t.Fatalf("decision = %+v", got)
	}
	if len(f.calls) != 1 || f.calls[0] != "repo" {
		t.Fatalf("probes = %v, want root only", f.calls)
	}
	if got.Evidence.Operation != "build" {
		t.Fatalf("operation evidence = %q", got.Evidence.Operation)
	}
	if got := EvaluateDiskCapacity(&diskFake{err: errors.New("probe")}, DiskRequest{Path: "repo", TempPath: "tmp", Operation: "build"}, diskPolicy()); got.Evidence.Reason != DiskReasonUnavailable {
		t.Fatalf("probe error = %+v", got)
	}
}

func TestEvaluateDiskCapacityAdditionalFailureCarriesExactOpaqueEvidence(t *testing.T) {
	additional := Capacity{FilesystemID: "target/secret", TotalBytes: 1000, FreeBytes: 50, TotalInodes: 100, FreeInodes: 7}
	f := &diskFake{values: map[string]Capacity{"repo": diskHealthy("repo"), "target": additional}}
	got := EvaluateDiskCapacity(f, DiskRequest{Path: "repo", AdditionalPaths: []string{"target"}, Operation: "worktree_create"}, diskPolicy())
	if got.Allowed || got.Evidence.Reason != DiskReasonAdditionalBelow {
		t.Fatalf("decision = %+v", got)
	}
	if got.Evidence.FilesystemID != "repo" || got.Evidence.FailedFilesystemID != safeDiskIdentity("target/secret") {
		t.Fatalf("filesystem evidence = %+v", got.Evidence)
	}
	if got.Evidence.FailedFreeBytes == nil || *got.Evidence.FailedFreeBytes != 50 || got.Evidence.FailedFreePercent == nil || *got.Evidence.FailedFreePercent != 5 || got.Evidence.FailedFreeInodes == nil || *got.Evidence.FailedFreeInodes != 7 {
		t.Fatalf("failed volume metrics = %+v", got.Evidence)
	}
}

func TestAdditionalVolumeReasonCodesAreStable(t *testing.T) {
	root := diskHealthy("repo")
	for _, tc := range []struct {
		name   string
		path   string
		want   string
		result Capacity
		err    error
	}{
		{name: "unavailable", path: "missing", want: DiskReasonAdditionalUnavailable, err: errors.New("probe")},
		{name: "invalid", path: "invalid", want: DiskReasonAdditionalInvalid, result: Capacity{FilesystemID: "invalid", TotalBytes: 0, TotalInodes: 100}},
		{name: "below", path: "low", want: DiskReasonAdditionalBelow, result: Capacity{FilesystemID: "low", TotalBytes: 1000, FreeBytes: 0, TotalInodes: 100, FreeInodes: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := StatFSFunc(func(path string) (Capacity, error) {
				if path == "repo" {
					return root, nil
				}
				return tc.result, tc.err
			})
			got := EvaluateDiskCapacity(backend, DiskRequest{Path: "repo", AdditionalPaths: []string{tc.path}}, diskPolicy())
			if got.Evidence.Reason != tc.want {
				t.Fatalf("reason = %q, want %q", got.Evidence.Reason, tc.want)
			}
		})
	}
}

func TestAdditionalVolumeEvidenceJSONPreservesZeroObservations(t *testing.T) {
	backend := &diskFake{values: map[string]Capacity{
		"repo":   diskHealthy("repo"),
		"target": {FilesystemID: "target/secret", TotalBytes: 1000, FreeBytes: 0, TotalInodes: 100, FreeInodes: 0},
	}}
	request := DiskRequest{Path: "repo", AdditionalPaths: []string{"target"}, RequiredBytes: 7, RequiredInodes: 3, Operation: "worktree_create"}
	policy := diskPolicy()
	got := EvaluateDiskCapacity(backend, request, policy)
	if got.Allowed || got.Evidence.Reason != DiskReasonAdditionalBelow {
		t.Fatalf("decision = %+v", got)
	}
	data, err := json.Marshal(got.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	if strings.Contains(encoded, "target/secret") || strings.Contains(encoded, "target") {
		t.Fatalf("evidence leaked a path: %s", encoded)
	}
	var decoded DiskEvidence
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.FailedFilesystemID != safeDiskIdentity("target/secret") || decoded.Reason != DiskReasonAdditionalBelow {
		t.Fatalf("decoded identity/reason = %+v", decoded)
	}
	if decoded.RequiredBytes != 7 || decoded.RequiredInodes != 3 || decoded.ReserveBytes != policy.ReserveBytes || decoded.ReserveInodes != policy.ReserveInodes {
		t.Fatalf("decoded requirements/thresholds = %+v", decoded)
	}
	if decoded.FailedFreeBytes == nil || *decoded.FailedFreeBytes != 0 || decoded.FailedFreePercent == nil || *decoded.FailedFreePercent != 0 || decoded.FailedFreeInodes == nil || *decoded.FailedFreeInodes != 0 {
		t.Fatalf("zero observations were not preserved: %+v JSON=%s", decoded, encoded)
	}
}

func TestDiskEvidenceJSONOmitsUnobservedVolumeMetrics(t *testing.T) {
	backend := &diskFake{values: map[string]Capacity{"repo": diskHealthy("repo")}}
	evidence := EvaluateDiskCapacity(backend, DiskRequest{Path: "repo"}, diskPolicy()).Evidence
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	for _, field := range []string{"temp_free_bytes", "temp_free_percent", "temp_free_inodes", "failed_free_bytes", "failed_free_percent", "failed_free_inodes"} {
		if strings.Contains(encoded, field) {
			t.Fatalf("unobserved field %q serialized: %s", field, encoded)
		}
	}
	var decoded DiskEvidence
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TempFreeBytes != nil || decoded.TempFreePercent != nil || decoded.TempFreeInodes != nil || decoded.FailedFreeBytes != nil || decoded.FailedFreePercent != nil || decoded.FailedFreeInodes != nil {
		t.Fatalf("unobserved metrics became present after unmarshal: %+v", decoded)
	}
}

func TestDiskEvidenceJSONOmitsAdditionalErrorMetrics(t *testing.T) {
	backend := StatFSFunc(func(path string) (Capacity, error) {
		if path == "repo" {
			return diskHealthy("repo"), nil
		}
		return Capacity{}, errors.New("additional probe failed")
	})
	evidence := EvaluateDiskCapacity(backend, DiskRequest{Path: "repo", AdditionalPaths: []string{"target"}}, diskPolicy()).Evidence
	if evidence.Reason != DiskReasonAdditionalUnavailable {
		t.Fatalf("reason = %q", evidence.Reason)
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "failed_free_") {
		t.Fatalf("probe-error metrics serialized without observation: %s", data)
	}
}

func TestDiskEvidenceJSONPreservesObservedTempZeros(t *testing.T) {
	backend := &diskFake{values: map[string]Capacity{
		"repo": diskHealthy("repo"),
		"tmp":  {FilesystemID: "tmp/secret", TotalBytes: 1000, FreeBytes: 0, TotalInodes: 100, FreeInodes: 0},
	}}
	evidence := EvaluateDiskCapacity(backend, DiskRequest{Path: "repo", TempPath: "tmp"}, diskPolicy()).Evidence
	if evidence.Reason != DiskReasonTempVolumeDivergence || evidence.TempFreeBytes == nil || evidence.TempFreePercent == nil || evidence.TempFreeInodes == nil {
		t.Fatalf("observed temp zeros missing: %+v", evidence)
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	var decoded DiskEvidence
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.TempFreeBytes == nil || *decoded.TempFreeBytes != 0 || decoded.TempFreePercent == nil || *decoded.TempFreePercent != 0 || decoded.TempFreeInodes == nil || *decoded.TempFreeInodes != 0 {
		t.Fatalf("observed temp zeros lost after unmarshal: %+v JSON=%s", decoded, data)
	}
	if decoded.FailedFreeBytes != nil || decoded.FailedFreePercent != nil || decoded.FailedFreeInodes != nil {
		t.Fatalf("unobserved additional metrics became present: %+v", decoded)
	}
}

func TestStatfsConversionRejectsOverflow(t *testing.T) {
	for name, tc := range map[string][7]uint64{
		"zero block size": {1, 1, 0, 1, 1, 1, 1},
		"total overflow":  {1, 1, 2, math.MaxUint64, 1, 1, 1},
		"free overflow":   {1, 1, 2, 1, math.MaxUint64, 1, 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := capacityFromStatfs("fs", "1", tc[3], tc[4], tc[2], tc[5], tc[6]); err == nil {
				t.Fatal("expected conversion failure")
			}
		})
	}
	left, err := capacityFromStatfs("a", "1", 2, 1, 4, 10, 5)
	right, err2 := capacityFromStatfs("b", "1", 2, 1, 4, 10, 5)
	typeVariant, err3 := capacityFromStatfs("a", "2", 2, 1, 4, 10, 5)
	if err != nil || err2 != nil || err3 != nil || left.TotalBytes != 8 || left.FreeBytes != 4 || left.FilesystemID == right.FilesystemID || left.FilesystemID == typeVariant.FilesystemID {
		t.Fatalf("conversion = %+v/%+v, errors %v/%v", left, right, err, err2)
	}
}

func TestEvaluateDiskCapacityRejectsInvalidPolicy(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), -1, 101} {
		p := diskPolicy()
		p.ReservePercent = value
		got := EvaluateDiskCapacity(&diskFake{values: map[string]Capacity{".": diskHealthy("fs")}}, DiskRequest{Path: "."}, p)
		if got.Allowed || got.Evidence.Reason != DiskReasonInvalidPolicy {
			t.Fatalf("policy %v = %+v", value, got)
		}
	}
}

func TestCapacityGateHysteresisIsBoundToScope(t *testing.T) {
	p := DiskPolicy{ReserveBytes: 600, ReservePercent: 1, RecoveryBytes: 800, RecoveryPercent: 1, ReserveInodes: 1, RecoveryInodes: 1}
	f := &diskFake{values: map[string]Capacity{
		"same":  {FilesystemID: "same", TotalBytes: 1000, FreeBytes: 500, TotalInodes: 100, FreeInodes: 90},
		"other": {FilesystemID: "other", TotalBytes: 1000, FreeBytes: 700, TotalInodes: 100, FreeInodes: 90},
	}}
	g := NewCapacityGate(f, p)
	first := g.Admit(DiskRequest{Path: "same"})
	if first.Allowed || first.Evidence.Reason != DiskReasonBelowThreshold {
		t.Fatalf("first denial = %+v", first)
	}
	second := g.Admit(DiskRequest{Path: "same"})
	if second.Allowed || second.Evidence.Reason != DiskReasonHysteresis {
		t.Fatalf("same-scope recovery denial = %+v", second)
	}
	other := g.Admit(DiskRequest{Path: "other"})
	if !other.Allowed {
		t.Fatalf("different healthy scope inherited hysteresis: %+v", other)
	}
	if first.Evidence.ScopeID == other.Evidence.ScopeID {
		t.Fatalf("derived scope identities aliased: %q", first.Evidence.ScopeID)
	}
	if strings.Contains(other.Evidence.ScopeID, "/") || len(other.Evidence.ScopeID) > 32 {
		t.Fatalf("unsafe scope evidence = %q", other.Evidence.ScopeID)
	}
}

func TestCapacityGateConcurrentScopesDoNotBleed(t *testing.T) {
	p := DiskPolicy{ReserveBytes: 400, ReservePercent: 1, RecoveryBytes: 800, RecoveryPercent: 1, ReserveInodes: 1, RecoveryInodes: 1}
	f := &diskFake{values: map[string]Capacity{
		"a": {FilesystemID: "a", TotalBytes: 1000, FreeBytes: 100, TotalInodes: 100, FreeInodes: 90},
		"b": {FilesystemID: "b", TotalBytes: 1000, FreeBytes: 500, TotalInodes: 100, FreeInodes: 90},
	}}
	g := NewCapacityGate(f, p)
	first := g.Admit(DiskRequest{Path: "a"})
	if first.Allowed {
		t.Fatal("volume A must establish blocked state")
	}
	type result struct {
		path string
		got  DiskDecision
	}
	results := make(chan result, 2)
	go func() { results <- result{"a", g.Admit(DiskRequest{Path: "a"})} }()
	go func() { results <- result{"b", g.Admit(DiskRequest{Path: "b"})} }()
	var b DiskDecision
	for i := 0; i < 2; i++ {
		got := <-results
		if got.path == "b" {
			b = got.got
		}
	}
	if !b.Allowed {
		t.Fatalf("boundary-valued healthy scope inherited recovery hysteresis: %+v", b)
	}
	if first.Evidence.ScopeID == b.Evidence.ScopeID {
		t.Fatalf("concurrent derived scope identities aliased: %q", b.Evidence.ScopeID)
	}
}

func TestAggregateDiskRequirementIsNonzeroAndOverflowSafe(t *testing.T) {
	got, err := AggregateDiskRequirement(DefaultMergeRequirement(), DefaultWorktreeCreateRequirement())
	if err != nil || got.Bytes == 0 || got.Inodes == 0 {
		t.Fatalf("aggregate = %+v, err=%v", got, err)
	}
	if _, err := AggregateDiskRequirement(DiskRequirement{Bytes: math.MaxUint64}, DiskRequirement{Bytes: 1, Inodes: 1}); err == nil {
		t.Fatal("expected byte requirement overflow")
	}
	if _, err := AggregateDiskRequirement(DiskRequirement{Inodes: math.MaxUint64}, DiskRequirement{Bytes: 1, Inodes: 1}); err == nil {
		t.Fatal("expected inode requirement overflow")
	}
}

func TestDiskEvidenceNextActionIsStableAndPathFree(t *testing.T) {
	blocked := EvaluateDiskCapacity(&diskFake{values: map[string]Capacity{".": diskHealthy("fs")}}, DiskRequest{Operation: "merge_gate", Path: ".", RequiredBytes: 600}, diskPolicy())
	if blocked.Allowed || blocked.Evidence.NextAction != DiskActionRecoverSpace {
		t.Fatalf("pressure action = %+v", blocked)
	}
	invalid := EvaluateDiskCapacity(nil, DiskRequest{Operation: "merge_gate", Path: "/secret/path"}, DiskPolicy{ReservePercent: 101})
	if invalid.Allowed || invalid.Evidence.NextAction != DiskActionFixPolicy || strings.Contains(invalid.Evidence.NextAction, "/") {
		t.Fatalf("invalid-policy action = %+v", invalid)
	}
}
