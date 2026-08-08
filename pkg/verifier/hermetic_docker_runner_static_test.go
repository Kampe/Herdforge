package verifier

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	fac198PrimaryContainerID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	fac198ForeignContainerID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func fac198Zip(t *testing.T, entries ...[2]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	for _, entry := range entries {
		file, err := writer.Create(entry[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func TestFAC198GoZipHashOrdersRecordsByFilename(t *testing.T) {
	content := fac198Zip(t, [2]string{"z.txt", "b"}, [2]string{"a.txt", "a"})
	got, err := goZipHash(content)
	if err != nil {
		t.Fatalf("goZipHash: %v", err)
	}
	const want = "h1:CeGSs7jJBacfGLbdHt50nJPP66Qy8cXHKz0bXS7f4GY="
	if got != want {
		t.Fatalf("goZipHash = %q, want %q", got, want)
	}
}

func TestFAC198GoZipHashRejectsDuplicateAndUnsafeEntries(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
	}{
		{name: "duplicate", content: fac198Zip(t, [2]string{"same.txt", "one"}, [2]string{"same.txt", "two"})},
		{name: "unsafe", content: fac198Zip(t, [2]string{"../escape.txt", "bad"})},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := goZipHash(test.content); err == nil {
				t.Fatal("goZipHash accepted invalid ZIP entries")
			}
		})
	}
}

func TestFAC151HermeticPrivateKeyZeroing(t *testing.T) {
	key := ed25519.PrivateKey(bytes.Repeat([]byte{0xa5}, ed25519.PrivateKeySize))
	zeroPrivateKey(key)
	if !bytes.Equal(key, make([]byte, ed25519.PrivateKeySize)) {
		t.Fatal("private-key zeroing control did not clear every byte")
	}
}

func TestFAC151HermeticBoundedBufferFailsClosed(t *testing.T) {
	var output bytes.Buffer
	bounded := &boundedBuffer{Buffer: &output, Limit: 3, label: "fake stdout"}
	if _, err := bounded.Write([]byte("1234")); err == nil {
		t.Fatal("overflowing bounded capture was silently truncated")
	}
	if output.Len() != 0 {
		t.Fatalf("overflowing write retained partial output: %d bytes", output.Len())
	}
	if _, err := bounded.Write([]byte("123")); err != nil {
		t.Fatalf("bounded write at the exact limit failed: %v", err)
	}
}

func TestFAC151HermeticTarPayloadIsValidAndBounded(t *testing.T) {
	payload, err := boundedTarPayload([]tarPayload{{name: "receipt.json", data: []byte(`{"ok":true}`)}}, maxHermeticReceiptTarBytes)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(payload))
	header, err := reader.Next()
	if err != nil || header.Name != "receipt.json" || header.Size != int64(len(`{"ok":true}`)) || header.Mode&0o777 != 0o444 || header.Uid != 0 || header.Gid != 0 {
		t.Fatalf("receipt tar header = %#v, err=%v", header, err)
	}
	if got, err := io.ReadAll(reader); err != nil || string(got) != `{"ok":true}` {
		t.Fatalf("receipt tar payload = %q, err=%v", got, err)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("tar payload has unexpected trailing member: %v", err)
	}
	if int64(len(payload)) > maxHermeticReceiptTarBytes {
		t.Fatalf("tar payload exceeded bound: %d", len(payload))
	}
	if err := validateTarPayload(payload, []tarPayload{{name: "receipt.json", data: []byte(`{"ok":true}`), mode: 0o444}}); err != nil {
		t.Fatalf("exact receipt payload contract rejected valid tar: %v", err)
	}
	if err := validateTarPayload(payload, []tarPayload{{name: "receipt.json", data: []byte(`{"wrong":true}`), mode: 0o444}}); err == nil {
		t.Fatal("receipt payload content mutation was accepted")
	}
	if err := validateTarPayload(payload, []tarPayload{{name: "receipt.json", data: []byte(`{"ok":true}`), mode: 0o444}, {name: "extra", data: []byte("x"), mode: 0o444}}); err == nil {
		t.Fatal("unexpected receipt payload member was accepted")
	}
	if _, err := boundedTarPayload([]tarPayload{{name: "../escape", data: []byte("x")}}, maxHermeticReceiptTarBytes); err == nil {
		t.Fatal("unsafe tar member path was accepted")
	}
}

func TestFAC151HermeticCacheTarMembersAreWorldReadable(t *testing.T) {
	payload, err := verifiedGoCacheTar(verifiedGoCache{
		Zip: []byte("zip"), ZipHash: []byte("hash"), Mod: []byte("mod"), Info: []byte("info"),
	})
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(payload))
	for index := 0; index < 4; index++ {
		header, err := reader.Next()
		if err != nil {
			t.Fatal(err)
		}
		if header.Mode&0o777 != 0o444 || header.Uid != 0 || header.Gid != 0 {
			t.Fatalf("cache member %q metadata mode=%o uid=%d gid=%d, want 0444/0/0", header.Name, header.Mode&0o777, header.Uid, header.Gid)
		}
	}
}

func TestFAC151HermeticSymlinkCanonicality(t *testing.T) {
	for _, target := range []string{"/tmp/escape", "../../escape"} {
		t.Run("archive-"+strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			var buffer bytes.Buffer
			writer := tar.NewWriter(&buffer)
			if err := writer.WriteHeader(&tar.Header{Name: "nested/link", Typeflag: tar.TypeSymlink, Linkname: target, Uid: 0, Gid: 0, Mode: 0o777}); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := sourceManifestDigestFromArchive(buffer.Bytes()); err == nil {
				t.Fatalf("unsafe archive symlink target %q was accepted", target)
			}
		})
		t.Run("filesystem-"+strings.ReplaceAll(target, "/", "_"), func(t *testing.T) {
			member := newFAC198SymlinkInfo()
			if !fac198OwnerFixtureSupported() {
				readMembers := filesystemReadbackReader(func(string) ([]filesystemManifestMember, error) {
					return []filesystemManifestMember{{name: "nested/link", info: member, target: target}}, nil
				})
				if _, err := sourceManifestDigestFromFilesystemWithReader("/fake-root", readMembers); err == nil {
					t.Fatal("non-Unix owner gate did not fail closed")
				}
				return
			}
			readMembers := filesystemReadbackReader(func(string) ([]filesystemManifestMember, error) {
				return []filesystemManifestMember{{name: "nested/link", info: member, target: target}}, nil
			})
			if _, err := sourceManifestDigestFromFilesystemWithReader("/fake-root", readMembers); err == nil {
				t.Fatalf("unsafe filesystem symlink target %q was accepted", target)
			}
		})
	}
	t.Run("filesystem-contained-relative", func(t *testing.T) {
		if !fac198OwnerFixtureSupported() {
			return
		}
		member := newFAC198SymlinkInfo()
		readMembers := filesystemReadbackReader(func(string) ([]filesystemManifestMember, error) {
			return []filesystemManifestMember{{name: "nested/link", info: member, target: "target"}}, nil
		})
		if _, err := sourceManifestDigestFromFilesystemWithReader("/fake-root", readMembers); err != nil {
			t.Fatalf("contained relative filesystem symlink was rejected: %v", err)
		}
	})
}

func fac198ArchiveWithHeaders(t *testing.T, headers ...tar.Header) []byte {
	t.Helper()
	var archive bytes.Buffer
	for _, header := range headers {
		if header.Typeflag == tar.TypeXGlobalHeader {
			fac198WriteRawGlobalHeader(t, &archive, header)
			continue
		}
		var member bytes.Buffer
		writer := tar.NewWriter(&member)
		if err := writer.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := writer.Write(bytes.Repeat([]byte{'x'}, int(header.Size))); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		archive.Write(member.Bytes()[:len(member.Bytes())-1024])
	}
	archive.Write(make([]byte, 1024))
	return archive.Bytes()
}

func fac198WriteRawGlobalHeader(t *testing.T, archive *bytes.Buffer, header tar.Header) {
	t.Helper()
	var encoded bytes.Buffer
	writer := tar.NewWriter(&encoded)
	if err := writer.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, PAXRecords: header.PAXRecords}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(nil), encoded.Bytes()[:len(encoded.Bytes())-1024]...)
	copy(raw[0:100], []byte(header.Name))
	copy(raw[100:108], []byte(fmt.Sprintf("%07o\x00", header.Mode)))
	copy(raw[108:116], []byte(fmt.Sprintf("%07o\x00", header.Uid)))
	copy(raw[116:124], []byte(fmt.Sprintf("%07o\x00", header.Gid)))
	copy(raw[157:257], []byte(header.Linkname))
	raw[156] = header.Typeflag
	for index := 148; index < 156; index++ {
		raw[index] = ' '
	}
	var checksum int64
	for _, value := range raw[:512] {
		checksum += int64(value)
	}
	copy(raw[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
	archive.Write(raw)
}

func fac198GitArchiveMetadata() tar.Header {
	return tar.Header{
		Name:       "pax_global_header",
		Typeflag:   tar.TypeXGlobalHeader,
		Uid:        0,
		Gid:        0,
		PAXRecords: map[string]string{"comment": strings.Repeat("a", 40)},
		Format:     tar.FormatPAX,
	}
}

func TestFAC198ArchiveGlobalMetadataPolicy(t *testing.T) {
	regular := tar.Header{Name: "pkg/verifier/member.go", Mode: 0o644, Uid: 0, Gid: 0, Size: 1}
	withMetadata := fac198ArchiveWithHeaders(t, fac198GitArchiveMetadata(), regular)
	withoutMetadata := fac198ArchiveWithHeaders(t, regular)
	reader := tar.NewReader(bytes.NewReader(withMetadata))
	header, err := reader.Next()
	if err != nil {
		t.Fatalf("read Git global metadata fixture: %v", err)
	}
	if header.Name != "pax_global_header" || header.Typeflag != tar.TypeXGlobalHeader || header.Uid != 0 || header.Gid != 0 || header.Mode != 0 || header.Size != 0 || !reflect.DeepEqual(header.PAXRecords, map[string]string{"comment": strings.Repeat("a", 40)}) {
		t.Fatalf("global metadata fixture shape = %#v", header)
	}
	withDigest, err := sourceManifestDigestFromArchive(withMetadata)
	if err != nil {
		t.Fatalf("git archive metadata was rejected: %v", err)
	}
	withoutDigest, err := sourceManifestDigestFromArchive(withoutMetadata)
	if err != nil {
		t.Fatalf("ordinary archive was rejected: %v", err)
	}
	if withDigest != withoutDigest {
		t.Fatalf("metadata changed manifest digest: with=%q without=%q", withDigest, withoutDigest)
	}

	tests := []struct {
		name    string
		archive func() []byte
	}{
		{name: "duplicate metadata", archive: func() []byte {
			return fac198ArchiveWithHeaders(t, fac198GitArchiveMetadata(), fac198GitArchiveMetadata(), regular)
		}},
		{name: "unexpected type", archive: func() []byte {
			return fac198ArchiveWithHeaders(t, tar.Header{Name: "foreign", Typeflag: tar.TypeFifo, Uid: 0, Gid: 0})
		}},
		{name: "empty metadata payload", archive: func() []byte {
			header := fac198GitArchiveMetadata()
			header.PAXRecords = map[string]string{}
			return fac198ArchiveWithHeaders(t, header)
		}},
		{name: "oversized metadata payload", archive: func() []byte {
			header := fac198GitArchiveMetadata()
			header.PAXRecords["comment"] = strings.Repeat("a", 41)
			return fac198ArchiveWithHeaders(t, header)
		}},
		{name: "ambiguous metadata", archive: func() []byte {
			header := fac198GitArchiveMetadata()
			header.PAXRecords["path"] = "pkg/verifier/member.go"
			return fac198ArchiveWithHeaders(t, header)
		}},
		{name: "unsafe metadata name", archive: func() []byte {
			header := fac198GitArchiveMetadata()
			header.Name = "../pax_global_header"
			return fac198ArchiveWithHeaders(t, header)
		}},
		{name: "metadata link", archive: func() []byte {
			header := fac198GitArchiveMetadata()
			header.Linkname = "pkg/verifier/member.go"
			return fac198ArchiveWithHeaders(t, header)
		}},
		{name: "wrong metadata ownership", archive: func() []byte {
			header := fac198GitArchiveMetadata()
			header.Uid = 1
			return fac198ArchiveWithHeaders(t, header)
		}},
		{name: "wrong metadata mode", archive: func() []byte {
			header := fac198GitArchiveMetadata()
			header.Mode = 0o644
			return fac198ArchiveWithHeaders(t, header)
		}},
		{name: "manifest shadow", archive: func() []byte {
			shadow := tar.Header{Name: "pax_global_header", Mode: 0o644, Uid: 0, Gid: 0, Size: 1}
			return fac198ArchiveWithHeaders(t, shadow)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := sourceManifestDigestFromArchive(test.archive()); err == nil {
				t.Fatal("invalid global metadata was accepted")
			}
		})
	}
}

func TestFAC198GitArchiveProducerPinsTarUmaskAndStrictReadback(t *testing.T) {
	root := "/repo"
	candidateSHA := strings.Repeat("a", 40)
	wantArgs := []string{"-c", "tar.umask=0022", "-C", root, "archive", "--format=tar", candidateSHA}
	if got := fixedGitArchiveArgs(root, candidateSHA); !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("git archive argv = %#v, want %#v", got, wantArgs)
	}
	for _, mode := range []int64{0o644, 0o755} {
		archive := fac198ArchiveWithHeaders(t, tar.Header{Name: "pkg/verifier/member.go", Mode: mode, Uid: 0, Gid: 0, Size: 1})
		if _, err := sourceManifestDigestFromArchive(archive); err != nil {
			t.Fatalf("canonical Git regular mode %o rejected: %v", mode, err)
		}
	}
	for _, mode := range []int64{0o664, 0o666, 0o777} {
		archive := fac198ArchiveWithHeaders(t, tar.Header{Name: "pkg/verifier/member.go", Mode: mode, Uid: 0, Gid: 0, Size: 1})
		if _, err := sourceManifestDigestFromArchive(archive); err == nil {
			t.Fatalf("noncanonical Git regular mode %o was accepted", mode)
		}
	}
}

func TestSourceManifestDigestFromFilesystemMembersSkipsDirectory(t *testing.T) {
	if !fac198OwnerFixtureSupported() {
		t.Skip("filesystem owner fixture is fail-closed on non-Unix platforms")
	}
	root := t.TempDir()
	content := []byte("later member")
	if err := os.WriteFile(filepath.Join(root, "later.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	readMembers := filesystemReadbackReader(func(string) ([]filesystemManifestMember, error) {
		return []filesystemManifestMember{
			{name: "nested", info: newFAC198DirectoryInfo()},
			{name: "later.txt", info: newFAC198RegularInfo(int64(len(content)))},
		}, nil
	})
	got, err := sourceManifestDigestFromFilesystemWithReader(root, readMembers)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	want, err := sourceManifestDigestFromRecords([]sourceManifestRecord{{
		name: "later.txt", kind: 'f', mode: 0, size: int64(len(content)), digest: hex.EncodeToString(sum[:]),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("digest=%q, want later regular member contribution %q", got, want)
	}
}

func TestFAC151HermeticRuntimeArgvBindsCompileArtifactAndPolicy(t *testing.T) {
	want := []string{
		"/usr/bin/env",
		"TMPDIR=" + hermeticGoFixtureTmpDir,
		"GOTMPDIR=" + hermeticGoFixtureTmpDir,
		"HOME=" + hermeticGoFixtureTmpDir,
		hermeticContainerEnv + "=1",
		hermeticRunPath + "/verifier.test",
		"-test.run", fixedFAC151Regex(),
		"-test.count=" + hermeticTestCount,
		"-test.timeout=" + hermeticTestTimeout,
	}
	got := fixedFAC151Argv()
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("runtime argv = %#v, want %#v", got, want)
	}
	if !containsString(got, hermeticRunPath+"/verifier.test") {
		t.Fatalf("runtime argv missing compile output path: %#v", got)
	}
}

func TestFAC151HermeticStaticControlFlowContracts(t *testing.T) {
	_, runnerPath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	runnerSource, err := os.ReadFile(filepath.Join(filepath.Dir(runnerPath), "hermetic_docker_runner.go"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(runnerSource)
	contracts := map[string]string{
		"key zeroing is deferred immediately after generation":        "defer zeroPrivateKey(privateKey)",
		"receipt is copied as a tar payload":                          "boundedTarPayload([]tarPayload{{name: path.Base(hermeticReceiptPath)",
		"receipt copy targets replay directory":                       "r.docker.Copy(ctx, id, hermeticReplayPath, payload)",
		"source sealing does not recurse into root-owned descendants": "[]string{\"/bin/chmod\", \"a-w\", hermeticSourcePath}",
	}
	for name, fragment := range contracts {
		if !strings.Contains(source, fragment) {
			t.Errorf("missing static contract %s: %q", name, fragment)
		}
	}
	if strings.Contains(source, "io.LimitWriter") {
		t.Fatal("hermetic Docker runner still uses silently truncating LimitWriter")
	}

	_, otherPath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	otherSource, err := os.ReadFile(filepath.Join(filepath.Dir(otherPath), "fac151_compiled_admission_other_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(otherSource), "func fac151TestMainAdmission() error { return compiledFAC151Admission() }") {
		t.Fatal("non-Linux tagged TestMain path does not fail closed through compiled admission")
	}
}

type fac198DockerFake struct {
	removed          bool
	removeAttempted  bool
	removeID         string
	postInspect      int
	inspectCalls     int
	foreignAt        int
	execCalls        int
	emptyPreStart    bool
	emptyPostStart   bool
	partialPostStart bool
	createCalls      int
	pullCalls        int
	imageInspectErr  []error
	callOrder        []string
	pullErr          error
	executionErr     error
	removeErr        error
	removeErrButGone bool
	blockStart       chan struct{}
	startHook        func(containerID string) error
	mountInfoHook    func() error
	copies           []struct {
		destination string
		payload     []byte
	}
	inspection        dockerInspection
	mountInfo         []byte
	verifierExecCalls int
	mountInfoProbes   int
}

func (f *fac198DockerFake) Create(context.Context) (string, error) {
	f.createCalls++
	f.callOrder = append(f.callOrder, "create")
	return fac198PrimaryContainerID, nil
}
func (f *fac198DockerFake) InspectImage(context.Context) error {
	f.callOrder = append(f.callOrder, "image inspect")
	if len(f.imageInspectErr) == 0 {
		return nil
	}
	err := f.imageInspectErr[0]
	f.imageInspectErr = f.imageInspectErr[1:]
	return err
}
func (f *fac198DockerFake) Pull(context.Context) error {
	f.pullCalls++
	f.callOrder = append(f.callOrder, "pull --platform "+hermeticDockerPlatform+" "+hermeticDockerImage)
	return f.pullErr
}
func (f *fac198DockerFake) Inspect(_ context.Context, id string) (dockerInspection, error) {
	f.callOrder = append(f.callOrder, "inspect")
	if f.removeAttempted {
		f.postInspect++
		if f.removed {
			return dockerInspection{}, &dockerCommandError{Operation: "inspect", ExitCode: 1, Stderr: "Error response from daemon: No such container: " + id}
		}
		return f.inspection, nil
	}
	if f.removed {
		return dockerInspection{}, &dockerCommandError{Operation: "inspect", ExitCode: 1, Stderr: "Error response from daemon: No such container: " + id}
	}
	f.inspectCalls++
	if id != fac198PrimaryContainerID {
		return dockerInspection{}, errors.New("unexpected container identity")
	}
	if f.inspectCalls == f.foreignAt {
		foreign := f.inspection
		foreign.ID = fac198ForeignContainerID
		return foreign, nil
	}
	inspection := f.inspection
	if f.inspectCalls == 1 && f.emptyPreStart {
		inspection.Mounts = nil
	}
	if f.inspectCalls == 2 && f.emptyPostStart {
		inspection.Mounts = nil
	}
	if f.inspectCalls == 2 && f.partialPostStart {
		inspection.Mounts = inspection.Mounts[:1]
	}
	return inspection, nil
}
func (f *fac198DockerFake) Start(ctx context.Context, _ string) error {
	f.callOrder = append(f.callOrder, "start")
	if f.startHook != nil {
		return f.startHook(fac198PrimaryContainerID)
	}
	if f.blockStart != nil {
		close(f.blockStart)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (f *fac198DockerFake) Copy(_ context.Context, _ string, destination string, payload []byte) error {
	f.callOrder = append(f.callOrder, "copy "+destination)
	f.copies = append(f.copies, struct {
		destination string
		payload     []byte
	}{destination, append([]byte(nil), payload...)})
	return nil
}
func (f *fac198DockerFake) Exec(_ context.Context, _ string, argv []string, _ []byte) (dockerResult, error) {
	f.execCalls++
	if len(argv) == 2 && argv[0] == "/bin/cat" && argv[1] == "/proc/self/mountinfo" { //hermetic:allow-argv-position test-fake dispatcher: routes by program+arg, not a launch contract
		if f.mountInfoHook != nil {
			if err := f.mountInfoHook(); err != nil {
				return dockerResult{}, err
			}
		}
		f.mountInfoProbes++
		f.callOrder = append(f.callOrder, "mountinfo")
		return dockerResult{Output: append([]byte(nil), f.mountInfo...)}, nil
	}
	if len(argv) > 0 {
		f.callOrder = append(f.callOrder, "exec "+argv[0])
	}
	if len(argv) == 3 && argv[0] == "/bin/sh" {
		return dockerResult{Output: []byte("101 202 65532 65532")}, nil
	}
	// Runtime invocation is env + binary + -test.*; compile is `go test -c -o …/verifier.test`
	// and must not be treated as the verifier run (would short-circuit before the test binary).
	if isHermeticVerifierInvocation(argv) {
		f.verifierExecCalls++
		if f.executionErr != nil {
			return dockerResult{Output: []byte("stdout\nstderr"), ExitCode: 7}, f.executionErr
		}
		return dockerResult{Output: []byte("stdout\nstderr"), ExitCode: 7}, errors.New("exit status 7")
	}
	return dockerResult{}, nil
}

func isHermeticVerifierInvocation(argv []string) bool {
	for i, arg := range argv {
		if arg == hermeticRunPath+"/verifier.test" {
			// Binary as argv[0], or after env assignments before -test flags.
			if i == 0 {
				return true
			}
			for _, later := range argv[i+1:] {
				if strings.HasPrefix(later, "-test.") {
					return true
				}
			}
		}
	}
	return false
}
func (f *fac198DockerFake) Remove(_ context.Context, id string) error {
	f.removeID = id
	f.removeAttempted = true
	if f.removeErr != nil {
		// removeErrButGone models the race EnsureCleanup exists for: the
		// remove command reports an error, but the container is genuinely
		// gone (e.g. an out-of-band removal won the race). An independent
		// absence check is the only trustworthy signal here.
		if f.removeErrButGone {
			f.removed = true
		}
		return f.removeErr
	}
	f.removed = true
	return nil
}

func newFAC198FakeRunner(t *testing.T, fake *fac198DockerFake) *hermeticDockerRunner {
	t.Helper()
	var source bytes.Buffer
	writer := tar.NewWriter(&source)
	if err := writer.WriteHeader(&tar.Header{Name: "pkg/verifier/fake.go", Mode: 0o644, Uid: 0, Gid: 0, Size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	sourceDigest, err := sourceManifestDigestFromArchive(source.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	policy := fixedHermeticDockerPolicy()
	fake.inspection.ID = fac198PrimaryContainerID
	fake.inspection.Config.Image = policy.image()
	fake.inspection.Config.User = hermeticContainerUser
	fake.inspection.HostConfig.NetworkMode = "none"
	fake.inspection.HostConfig.ReadonlyRootfs = true
	fake.inspection.HostConfig.PidsLimit = hermeticPIDLimit
	fake.inspection.HostConfig.Memory = hermeticMemoryBytes
	fake.inspection.HostConfig.CapDrop = []string{"ALL"}
	fake.inspection.HostConfig.SecurityOpt = []string{"seccomp=unconfined"}
	fake.inspection.HostConfig.Tmpfs = map[string]string{
		hermeticBuildPath: "rw,noexec,nosuid,nodev,size=512m", hermeticRunPath: "rw,exec,nosuid,nodev,size=256m", hermeticReplayPath: "rw,noexec,nosuid,nodev,size=64m",
	}
	for _, destination := range []string{hermeticBuildPath, hermeticRunPath, hermeticReplayPath} {
		fake.inspection.Mounts = append(fake.inspection.Mounts, struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}{Type: "tmpfs", Destination: destination, RW: true})
	}
	fake.mountInfo = fac198ValidMountInfo()
	runner, err := newFAC151DockerRunner("/repo", strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	runner.docker = fake
	runner.storePath = filepath.Join(t.TempDir(), "container-lifecycle.db")
	runner.allowlist = func(string) error { return nil }
	runner.hostCache = func() (verifiedGoCache, error) {
		return verifiedGoCache{Zip: []byte("zip"), ZipHash: []byte("hash"), Mod: []byte("mod"), Info: []byte("info")}, nil
	}
	runner.archive = func(context.Context, string, string) ([]byte, string, error) {
		return source.Bytes(), sourceDigest, nil
	}
	return runner
}

func fac198ValidMountInfo() []byte {
	return []byte("36 25 0:29 / /tmp/build rw,relatime - tmpfs tmpfs rw,nosuid,nodev,noexec,size=524288k\n37 25 0:30 / /tmp/replay rw,relatime - tmpfs tmpfs rw,nosuid,nodev,noexec,size=65536k\n38 25 0:31 / /tmp/run rw,relatime - tmpfs tmpfs rw,nosuid,nodev,size=262144k\n")
}

func fac198ValidInspection(t *testing.T) dockerInspection {
	t.Helper()
	fake := &fac198DockerFake{}
	newFAC198FakeRunner(t, fake)
	return fake.inspection
}

func TestFAC198DockerInspectionMountShapes(t *testing.T) {
	tests := []struct {
		name  string
		mount func([]struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}
		wantErr bool
	}{
		{name: "empty pre-start", mount: func([]struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} {
			return nil
		}, wantErr: false},
		{name: "exact full realization", mount: func(m []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} {
			return m
		}, wantErr: false},
		{name: "partial realization", mount: func(m []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} {
			return m[:2]
		}, wantErr: true},
		{name: "duplicate destination", mount: func(m []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} {
			m[1] = m[0]
			return m
		}, wantErr: true},
		{name: "missing destination", mount: func(m []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} {
			m[2] = m[0]
			return m
		}, wantErr: true},
		{name: "foreign type", mount: func(m []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} {
			m[0].Type = "bind"
			return m
		}, wantErr: true},
		{name: "foreign destination", mount: func(m []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} {
			m[0].Destination = "/foreign"
			return m
		}, wantErr: true},
		{name: "read-only realization", mount: func(m []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}) []struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		} {
			m[0].RW = false
			return m
		}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inspection := fac198ValidInspection(t)
			inspection.Mounts = test.mount(inspection.Mounts)
			stage := dockerInspectionPostStart
			if test.name == "empty pre-start" {
				stage = dockerInspectionPreStart
			}
			err := inspection.validate(fixedHermeticDockerPolicy(), fac198PrimaryContainerID, stage)
			if (err != nil) != test.wantErr {
				t.Fatalf("mount validation error=%v, wantErr=%t", err, test.wantErr)
			}
		})
	}
}

func TestFAC198DockerMountShapeDiagnostic(t *testing.T) {
	t.Run("pre-start and post-start stages", func(t *testing.T) {
		pre := fac198ValidInspection(t)
		pre.Mounts = pre.Mounts[:1]
		preErr := pre.validate(fixedHermeticDockerPolicy(), fac198PrimaryContainerID, dockerInspectionPreStart)
		if preErr == nil || !strings.Contains(preErr.Error(), "stage=pre_start") || !strings.Contains(preErr.Error(), "mounts_count=1") {
			t.Fatalf("pre-start diagnostic = %v", preErr)
		}
		post := fac198ValidInspection(t)
		post.Mounts = post.Mounts[:1]
		postErr := post.validate(fixedHermeticDockerPolicy(), fac198PrimaryContainerID, dockerInspectionPostStart)
		if postErr == nil || !strings.Contains(postErr.Error(), "stage=post_start") || !strings.Contains(postErr.Error(), "mounts_count=1") {
			t.Fatalf("post-start diagnostic = %v", postErr)
		}
	})
	t.Run("stable sorted bounded metadata", func(t *testing.T) {
		inspection := fac198ValidInspection(t)
		mounts := append([]struct {
			Type        string `json:"Type"`
			Destination string `json:"Destination"`
			RW          bool   `json:"RW"`
		}{}, inspection.Mounts...)
		mounts[0], mounts[2] = mounts[2], mounts[0]
		foreign := mounts[0]
		foreign.Type = strings.Repeat("foreign-type-", 12)
		foreign.Destination = "/private/host/path/that-must-not-leak"
		mounts = append(mounts, foreign)
		inspection.Mounts = mounts
		first := inspection.validate(fixedHermeticDockerPolicy(), fac198PrimaryContainerID, dockerInspectionPostStart)
		second := inspection.validate(fixedHermeticDockerPolicy(), fac198PrimaryContainerID, dockerInspectionPostStart)
		if first == nil || second == nil || first.Error() != second.Error() {
			t.Fatalf("unstable diagnostic first=%v second=%v", first, second)
		}
		diagnostic := first.Error()
		if !strings.Contains(diagnostic, "host_tmpfs_count=3") || !strings.Contains(diagnostic, "mounts_count=4") || !strings.Contains(diagnostic, "<non-fixed>") {
			t.Fatalf("diagnostic metadata = %q", diagnostic)
		}
		for _, entry := range []string{
			hermeticBuildPath + "=rw,noexec,nosuid,nodev,size=512m",
			hermeticReplayPath + "=rw,noexec,nosuid,nodev,size=64m",
			hermeticRunPath + "=rw,exec,nosuid,nodev,size=256m",
			"(tmpfs," + hermeticBuildPath + ",true)",
			"(tmpfs," + hermeticReplayPath + ",true)",
			"(tmpfs," + hermeticRunPath + ",true)",
		} {
			if !strings.Contains(diagnostic, entry) {
				t.Fatalf("diagnostic missing %q: %q", entry, diagnostic)
			}
		}
		if strings.Contains(diagnostic, "/private/host/path") || len(diagnostic) > maxDockerMountDiagnosticBytes {
			t.Fatalf("diagnostic leaked or exceeded bound: len=%d value=%q", len(diagnostic), diagnostic)
		}
		if strings.Index(diagnostic, hermeticBuildPath) > strings.Index(diagnostic, hermeticReplayPath) || strings.Index(diagnostic, hermeticReplayPath) > strings.Index(diagnostic, hermeticRunPath) {
			t.Fatalf("fixed destinations are not sorted: %q", diagnostic)
		}
	})
	t.Run("explicit bound", func(t *testing.T) {
		inspection := fac198ValidInspection(t)
		for index := 0; index < 128; index++ {
			mount := inspection.Mounts[0]
			mount.Type = strings.Repeat("foreign-type-", 100)
			mount.Destination = "/arbitrary/host/path/" + strconv.Itoa(index)
			inspection.Mounts = append(inspection.Mounts, mount)
		}
		err := inspection.validate(fixedHermeticDockerPolicy(), fac198PrimaryContainerID, dockerInspectionPostStart)
		if err == nil || len(err.Error()) > maxDockerMountDiagnosticBytes || strings.Contains(err.Error(), "/arbitrary/host/path") {
			t.Fatalf("bounded diagnostic = %v", err)
		}
	})
}

func TestFAC198RunnerStageBoundMountRealization(t *testing.T) {
	t.Run("empty pre-start then full post-start", func(t *testing.T) {
		fake := &fac198DockerFake{emptyPreStart: true}
		runner := newFAC198FakeRunner(t, fake)
		_, runErr := runner.Run(context.Background())
		if runErr == nil || fake.inspectCalls != 2 || len(fake.copies) == 0 || fake.execCalls == 0 {
			t.Fatalf("stage transition result err=%v inspections=%d copies=%d execs=%d", runErr, fake.inspectCalls, len(fake.copies), fake.execCalls)
		}
	})
	t.Run("empty post-start reaches downstream after runtime proof", func(t *testing.T) {
		fake := &fac198DockerFake{emptyPreStart: true, emptyPostStart: true}
		runner := newFAC198FakeRunner(t, fake)
		_, runErr := runner.Run(context.Background())
		if runErr == nil || fake.inspectCalls != 2 || fake.mountInfoProbes != 1 {
			t.Fatalf("empty post-start result err=%v inspections=%d probes=%d", runErr, fake.inspectCalls, fake.mountInfoProbes)
		}
		if len(fake.copies) == 0 || fake.verifierExecCalls != 1 {
			t.Fatalf("empty post-start did not reach downstream copies=%d verifier_execs=%d", len(fake.copies), fake.verifierExecCalls)
		}
	})
	t.Run("partial post-start fails before runtime probe", func(t *testing.T) {
		fake := &fac198DockerFake{partialPostStart: true}
		runner := newFAC198FakeRunner(t, fake)
		_, runErr := runner.Run(context.Background())
		if runErr == nil || fake.mountInfoProbes != 0 || len(fake.copies) != 0 || fake.verifierExecCalls != 0 {
			t.Fatalf("partial post-start boundary err=%v probes=%d copies=%d verifier_execs=%d", runErr, fake.mountInfoProbes, len(fake.copies), fake.verifierExecCalls)
		}
	})
}

func TestFAC198MountInfoFieldEscapes(t *testing.T) {
	for _, test := range []struct {
		name, input, want string
	}{
		{name: "space", input: `a\040b`, want: "a b"},
		{name: "tab", input: `a\011b`, want: "a\tb"},
		{name: "newline", input: `a\012b`, want: "a\nb"},
		{name: "backslash", input: `a\134b`, want: `a\b`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := decodeMountInfoField(test.input)
			if err != nil || got != test.want {
				t.Fatalf("decode(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
		})
	}
	for _, input := range []string{`a\013b`, `a\04`, `a\`, `a\999b`} {
		if _, err := decodeMountInfoField(input); err == nil {
			t.Fatalf("decode(%q) unexpectedly accepted invalid escape", input)
		}
	}
}

func TestFAC198RunnerRequiresRuntimeMountProofBeforeAuthorityUse(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func([]byte) []byte
		wantCode string
	}{
		{name: "missing", mutate: func(info []byte) []byte {
			return []byte("36 25 0:29 / /tmp/build rw,relatime - tmpfs tmpfs rw,nosuid,nodev,noexec\n37 25 0:30 / /tmp/replay rw,relatime - tmpfs tmpfs rw,nosuid,nodev,noexec\n")
		}},
		{name: "foreign", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("/tmp/run"), []byte("/tmp/foreign"), 1)
		}},
		{name: "duplicate", mutate: func(info []byte) []byte {
			return append(append([]byte(nil), info...), []byte("39 25 0:32 / /tmp/run rw,relatime - tmpfs tmpfs rw,nosuid,nodev,size=262144k\n")...)
		}},
		{name: "wrong flags", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("rw,nosuid,nodev,noexec"), []byte("ro,nosuid,nodev,noexec"), 1)
		}},
		{name: "wrong fstype", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("- tmpfs tmpfs"), []byte("- ext4 ext4"), 1)
		}},
		{name: "nested mount", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("/tmp/run"), []byte("/tmp/run/nested"), 1)
		}},
		{name: "contradictory options", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("rw,nosuid,nodev,noexec"), []byte("rw,ro,nosuid,nodev,noexec"), 1)
		}},
		{name: "exec mount option with noexec super option", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("rw,relatime"), []byte("rw,exec,relatime"), 1)
		}},
		{name: "noexec mount option with exec super option", mutate: func(info []byte) []byte {
			info = bytes.Replace(info, []byte("rw,relatime"), []byte("rw,noexec,relatime"), 1)
			return bytes.Replace(info, []byte("rw,nosuid,nodev,noexec"), []byte("rw,nosuid,nodev,exec"), 1)
		}},
		{name: "repeated internal space", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("rw,relatime"), []byte("rw  relatime"), 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "repeated leading space", mutate: func(info []byte) []byte {
			return append([]byte(" "), info...)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "repeated trailing space", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("\n"), []byte(" \n"), 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "empty mount option", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("rw,relatime"), []byte("rw,,relatime"), 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "duplicate noexec option", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("rw,nosuid,nodev,noexec"), []byte("rw,nosuid,nodev,noexec,noexec"), 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "duplicate rw option", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("rw,relatime"), []byte("rw,rw,relatime"), 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "wrong size", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("size=524288k"), []byte("size=1m"), 1)
		}, wantCode: "runtime_mountinfo_wrong:/tmp/build"},
		{name: "missing size", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte(",size=524288k"), nil, 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "duplicate size", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("size=524288k"), []byte("size=524288k,size=524288k"), 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "malformed size", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("size=524288k"), []byte("size=bogus"), 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "overflow size", mutate: func(info []byte) []byte {
			return bytes.Replace(info, []byte("size=524288k"), []byte("size=9223372036854775807g"), 1)
		}, wantCode: "runtime_mountinfo_malformed"},
		{name: "missing final newline", mutate: func(info []byte) []byte {
			return bytes.TrimSuffix(info, []byte("\n"))
		}},
		{name: "over bound", mutate: func(info []byte) []byte {
			return append(append([]byte(nil), info...), bytes.Repeat([]byte("x"), maxHermeticMountInfoBytes)...)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fac198DockerFake{}
			runner := newFAC198FakeRunner(t, fake)
			fake.mountInfo = test.mutate(fake.mountInfo)
			if _, err := runner.Run(context.Background()); err == nil {
				t.Fatal("runtime mount proof unexpectedly accepted")
			} else if test.wantCode != "" && !strings.Contains(err.Error(), test.wantCode) {
				t.Fatalf("runtime proof error=%q, want code %q", err, test.wantCode)
			}
			if len(fake.copies) != 0 || fake.verifierExecCalls != 0 {
				t.Fatalf("runtime proof reached authority use copies=%d verifier_execs=%d", len(fake.copies), fake.verifierExecCalls)
			}
			if fake.mountInfoProbes != 1 {
				t.Fatalf("runtime mount probes=%d, want exactly one", fake.mountInfoProbes)
			}
		})
	}
}

func TestFAC198RunnerAcceptsEscapedUnrelatedMountInfo(t *testing.T) {
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)
	unrelated := []byte("40 25 0:33 /source\\040tab\\011line\\012slash\\134 /unrelated\\040mount rw,relatime - tmpfs source\\040tab\\011line\\012slash\\134 rw,nosuid,nodev\n")
	fake.mountInfo = append(unrelated, fake.mountInfo...)
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("expected fixed downstream fake failure")
	}
	if fake.mountInfoProbes != 1 || len(fake.copies) == 0 || fake.verifierExecCalls != 1 {
		t.Fatalf("escaped unrelated mount did not reach downstream probes=%d copies=%d verifier_execs=%d", fake.mountInfoProbes, len(fake.copies), fake.verifierExecCalls)
	}
}

func TestFAC198RunnerAcceptsExactRuntimeMountProof(t *testing.T) {
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("expected fixed fake verifier failure after runtime proof")
	}
	if len(fake.copies) == 0 || fake.verifierExecCalls != 1 || fake.mountInfoProbes != 1 {
		t.Fatalf("exact runtime proof did not reach next stage copies=%d verifier_execs=%d probes=%d", len(fake.copies), fake.verifierExecCalls, fake.mountInfoProbes)
	}
	probeIndex, secondInspectIndex, copyIndex := -1, -1, -1
	inspectSeen := 0
	for index, call := range fake.callOrder {
		switch {
		case call == "inspect":
			inspectSeen++
			if inspectSeen == 2 {
				secondInspectIndex = index
			}
		case call == "mountinfo":
			probeIndex = index
		case strings.HasPrefix(call, "copy ") && copyIndex == -1:
			copyIndex = index
		}
	}
	if probeIndex <= secondInspectIndex || copyIndex <= probeIndex {
		t.Fatalf("probe ordering=%v", fake.callOrder)
	}
}

func fac198ImageAbsentError() error {
	return &dockerImageAbsentError{Reference: hermeticDockerImage}
}

func TestFAC198PinnedImageProvisioningCallOrder(t *testing.T) {
	fake := &fac198DockerFake{imageInspectErr: []error{fac198ImageAbsentError(), nil}}
	runner := newFAC198FakeRunner(t, fake)
	_, _ = runner.Run(context.Background())
	want := []string{"image inspect", "pull --platform " + hermeticDockerPlatform + " " + hermeticDockerImage, "image inspect", "create"}
	if len(fake.callOrder) < len(want) || !reflect.DeepEqual(fake.callOrder[:len(want)], want) {
		t.Fatalf("call order = %#v, want prefix %#v", fake.callOrder, want)
	}
	if fake.pullCalls != 1 || fake.createCalls != 1 {
		t.Fatalf("pulls=%d creates=%d, want one each", fake.pullCalls, fake.createCalls)
	}
}

func TestFAC198PinnedImageProvisioningFailuresBlockCreate(t *testing.T) {
	tests := []struct {
		name       string
		inspectErr error
		pullErr    error
	}{
		{name: "wrong post-pull digest", inspectErr: errors.New("image config digest mismatch")},
		{name: "wrong post-pull platform", inspectErr: errors.New("image platform mismatch")},
		{name: "wrong post-pull config", inspectErr: errors.New("image config mismatch")},
		{name: "wrong post-pull toolchain", inspectErr: errors.New("image toolchain mismatch")},
		{name: "non-absence inspect error", inspectErr: errors.New("daemon unavailable")},
		{name: "pull error", pullErr: errors.New("auth denied")},
		{name: "post-pull absence", inspectErr: fac198ImageAbsentError()},
		{name: "post-pull inspect error", inspectErr: errors.New("inspect failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fac198DockerFake{pullErr: test.pullErr}
			if test.name == "non-absence inspect error" {
				fake.imageInspectErr = []error{test.inspectErr}
			} else if test.name == "pull error" {
				fake.imageInspectErr = []error{fac198ImageAbsentError()}
			} else {
				fake.imageInspectErr = []error{fac198ImageAbsentError(), test.inspectErr}
			}
			if err := ensureHermeticDockerImage(context.Background(), fake); err == nil {
				t.Fatal("image provisioning unexpectedly succeeded")
			}
			if fake.createCalls != 0 {
				t.Fatalf("create calls=%d, want zero", fake.createCalls)
			}
			if test.name == "non-absence inspect error" && fake.pullCalls != 0 {
				t.Fatalf("pull calls=%d, want zero", fake.pullCalls)
			}
		})
	}
}

func TestFAC198PinnedImageCLIArgumentsAndIdentity(t *testing.T) {
	valid := dockerImageInspection{ID: hermeticDockerConfigDigest, Architecture: fixedHermeticDockerPolicy().architecture(), OS: "linux"}
	valid.Config.Env = []string{"GOLANG_VERSION=" + hermeticGoVersion, "GOTOOLCHAIN=" + hermeticGoToolchain}
	validOutput, err := json.Marshal([]dockerImageInspection{valid})
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	inspectCalls := 0
	cli := fixedDockerCLI{command: func(_ context.Context, _ []byte, args []string) (dockerResult, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "image":
			inspectCalls++
			if inspectCalls == 1 {
				return dockerResult{ExitCode: 1, Stderr: []byte("Error response from daemon: No such image: " + hermeticDockerImage)}, &dockerCommandError{Operation: "image inspect", ExitCode: 1, Stderr: "Error response from daemon: No such image: " + hermeticDockerImage}
			}
			return dockerResult{Output: validOutput}, nil
		case "pull":
			return dockerResult{}, nil
		default:
			return dockerResult{}, errors.New("unexpected Docker operation")
		}
	}}
	if err := ensureHermeticDockerImage(context.Background(), cli); err != nil {
		t.Fatalf("ensureHermeticDockerImage: %v", err)
	}
	want := [][]string{
		{"image", "inspect", hermeticDockerImage},
		{"pull", "--platform", hermeticDockerPlatform, hermeticDockerImage},
		{"image", "inspect", hermeticDockerImage},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("Docker calls = %#v, want %#v", calls, want)
	}
}

func TestFAC198PinnedImageIDRepresentations(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want bool
	}{
		{name: "config digest", id: hermeticDockerConfigDigest, want: true},
		{name: "reference digest", id: pinnedDockerReferenceDigest(hermeticDockerImage), want: true},
		{name: "foreign ID", id: "sha256:" + strings.Repeat("f", 64), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			image := dockerImageInspection{ID: test.id, Architecture: fixedHermeticDockerPolicy().architecture(), OS: "linux"}
			image.Config.Env = []string{"GOLANG_VERSION=" + hermeticGoVersion, "GOTOOLCHAIN=" + hermeticGoToolchain}
			output, err := json.Marshal([]dockerImageInspection{image})
			if err != nil {
				t.Fatal(err)
			}
			cli := fixedDockerCLI{command: func(_ context.Context, _ []byte, args []string) (dockerResult, error) {
				if !reflect.DeepEqual(args, []string{"image", "inspect", hermeticDockerImage}) {
					return dockerResult{}, errors.New("unexpected image inspect argv")
				}
				return dockerResult{Output: output}, nil
			}}
			gotErr := cli.InspectImage(context.Background())
			if (gotErr == nil) != test.want {
				t.Fatalf("InspectImage error=%v, want success=%t", gotErr, test.want)
			}
		})
	}
}

func TestFAC198PinnedImagePostPullMetadataBlocksProvisioning(t *testing.T) {
	tests := []struct {
		name string
		make func() dockerImageInspection
	}{
		{name: "config digest", make: func() dockerImageInspection {
			image := dockerImageInspection{ID: "sha256:" + strings.Repeat("0", 64), Architecture: fixedHermeticDockerPolicy().architecture(), OS: "linux"}
			image.Config.Env = []string{"GOLANG_VERSION=" + hermeticGoVersion, "GOTOOLCHAIN=" + hermeticGoToolchain}
			return image
		}},
		{name: "platform", make: func() dockerImageInspection {
			// Wrong architecture relative to the host pin must reject.
			wrong := "amd64"
			if fixedHermeticDockerPolicy().architecture() == "amd64" {
				wrong = "arm64"
			}
			image := dockerImageInspection{ID: hermeticDockerConfigDigest, Architecture: wrong, OS: "linux"}
			image.Config.Env = []string{"GOLANG_VERSION=" + hermeticGoVersion, "GOTOOLCHAIN=" + hermeticGoToolchain}
			return image
		}},
		{name: "Go version", make: func() dockerImageInspection {
			image := dockerImageInspection{ID: hermeticDockerConfigDigest, Architecture: fixedHermeticDockerPolicy().architecture(), OS: "linux"}
			image.Config.Env = []string{"GOLANG_VERSION=1.24.0", "GOTOOLCHAIN=" + hermeticGoToolchain}
			return image
		}},
		{name: "Go toolchain", make: func() dockerImageInspection {
			image := dockerImageInspection{ID: hermeticDockerConfigDigest, Architecture: fixedHermeticDockerPolicy().architecture(), OS: "linux"}
			image.Config.Env = []string{"GOLANG_VERSION=" + hermeticGoVersion, "GOTOOLCHAIN=auto"}
			return image
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			postPull, err := json.Marshal([]dockerImageInspection{test.make()})
			if err != nil {
				t.Fatal(err)
			}
			inspectCalls := 0
			pullCalls := 0
			cli := fixedDockerCLI{command: func(_ context.Context, _ []byte, args []string) (dockerResult, error) {
				switch args[0] {
				case "image":
					inspectCalls++
					if inspectCalls == 1 {
						stderr := "Error response from daemon: No such image: " + hermeticDockerImage
						return dockerResult{ExitCode: 1, Stderr: []byte(stderr)}, &dockerCommandError{Operation: "image inspect", ExitCode: 1, Stderr: stderr}
					}
					return dockerResult{Output: postPull}, nil
				case "pull":
					pullCalls++
					return dockerResult{}, nil
				default:
					return dockerResult{}, errors.New("unexpected Docker operation")
				}
			}}
			if err := ensureHermeticDockerImage(context.Background(), cli); err == nil {
				t.Fatal("invalid post-pull metadata unexpectedly passed")
			}
			if inspectCalls != 2 || pullCalls != 1 {
				t.Fatalf("inspect calls=%d pull calls=%d, want 2 and 1", inspectCalls, pullCalls)
			}
		})
	}
}

func TestFAC151HermeticRunnerFailurePreservesEvidenceAndTeardown(t *testing.T) {
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)
	result, runErr := runner.Run(context.Background())
	if runErr == nil || result.ExitCode != 7 {
		t.Fatalf("failure evidence result=%+v err=%v", result, runErr)
	}
	sum := sha256.Sum256([]byte("stdout\nstderr"))
	if result.OutputDigest != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("output digest=%q, want complete fake output digest", result.OutputDigest)
	}
	if !result.Removed || fake.removeID != fac198PrimaryContainerID || fake.postInspect != 1 {
		t.Fatalf("teardown evidence removed=%t id=%q post_inspect=%d", result.Removed, fake.removeID, fake.postInspect)
	}
	if len(fake.copies) != 3 {
		t.Fatalf("copy calls=%d, want source/cache/receipt", len(fake.copies))
	}
}

func TestFAC198SourceTransferUsesBuildRootAndPreservesDigest(t *testing.T) {
	if !fac198OwnerFixtureSupported() {
		t.Skip("filesystem owner fixture is fail-closed on non-Unix platforms")
	}
	archive := fac198ArchiveWithHeaders(t, tar.Header{Name: "pkg/verifier/member.go", Mode: 0o644, Uid: 0, Gid: 0, Size: 1})
	digest, err := sourceManifestDigestFromArchive(archive)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := rootCandidateArchiveForDocker(archive)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(transport))
	var names []string
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		names = append(names, header.Name)
	}
	if !reflect.DeepEqual(names, []string{"source/", "source/pkg/verifier/member.go"}) {
		t.Fatalf("transport members=%v", names)
	}
	transportMembers, err := fac198ReadRootedTransport(transport)
	if err != nil {
		t.Fatal(err)
	}
	transportDigest, err := fac198DigestFromTransportMembers(transportMembers)
	if err != nil || transportDigest != digest {
		t.Fatalf("transport digest=%q err=%v, archive digest=%q", transportDigest, err, digest)
	}
	root := t.TempDir()
	filesystemMembers := make([]filesystemManifestMember, 0, len(transportMembers))
	seenDirectories := make(map[string]bool)
	for _, member := range transportMembers {
		if member.directory {
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(member.name)), 0o755); err != nil {
				t.Fatal(err)
			}
			filesystemMembers = append(filesystemMembers, filesystemManifestMember{name: member.name, info: newFAC198DirectoryInfo()})
			seenDirectories[member.name] = true
			continue
		}
		parent := path.Dir(member.name)
		for parent != "." && !seenDirectories[parent] {
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(parent)), 0o755); err != nil {
				t.Fatal(err)
			}
			filesystemMembers = append(filesystemMembers, filesystemManifestMember{name: parent, info: newFAC198DirectoryInfo()})
			seenDirectories[parent] = true
			parent = path.Dir(parent)
		}
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(member.name)), member.data, 0o644); err != nil {
			t.Fatal(err)
		}
		filesystemMembers = append(filesystemMembers, filesystemManifestMember{name: member.name, info: newFAC198RegularInfo(int64(len(member.data)))})
	}
	readMembers := filesystemReadbackReader(func(string) ([]filesystemManifestMember, error) {
		return filesystemMembers, nil
	})
	filesystemDigest, err := sourceManifestDigestFromFilesystemWithReader(root, readMembers)
	if err != nil || filesystemDigest != digest {
		t.Fatalf("filesystem digest=%q err=%v, archive digest=%q", filesystemDigest, err, digest)
	}
	fake := &fac198DockerFake{}
	runner := newFAC198FakeRunner(t, fake)
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("expected downstream fake verifier failure")
	}
	if len(fake.copies) == 0 || fake.copies[0].destination != hermeticBuildPath {
		t.Fatalf("source copy destination=%q, want %q", fake.copies[0].destination, hermeticBuildPath)
	}
	probeIndex, sourceCopyIndex, testIndex := -1, -1, -1
	for index, call := range fake.callOrder {
		if call == "exec /bin/test" {
			testIndex = index
		}
		if strings.HasPrefix(call, "copy ") && sourceCopyIndex == -1 {
			sourceCopyIndex = index
		}
		if call == "mountinfo" {
			probeIndex = index
		}
	}
	if probeIndex < 0 || testIndex < 0 || sourceCopyIndex <= testIndex || testIndex <= probeIndex {
		t.Fatalf("source transfer call order=%v", fake.callOrder)
	}
}

type fac198TransportMember struct {
	name      string
	directory bool
	data      []byte
	target    string
	mode      int64
}

func fac198ReadRootedTransport(payload []byte) ([]fac198TransportMember, error) {
	reader := tar.NewReader(bytes.NewReader(payload))
	members := make([]fac198TransportMember, 0)
	root, err := reader.Next()
	if err != nil || root.Name != hermeticSourceTransportRoot+"/" || root.Typeflag != tar.TypeDir {
		return nil, errors.New("transport root is not exact")
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || !strings.HasPrefix(header.Name, hermeticSourceTransportRoot+"/") {
			return nil, errors.New("transport member escaped root")
		}
		name := strings.TrimPrefix(header.Name, hermeticSourceTransportRoot+"/")
		if name == "" {
			return nil, errors.New("transport member has empty name")
		}
		name = strings.TrimSuffix(name, "/")
		clean, err := validateManifestName(name)
		if err != nil || clean != name {
			return nil, errors.New("transport member name is unsafe")
		}
		member := fac198TransportMember{name: name, directory: header.Typeflag == tar.TypeDir, target: header.Linkname, mode: int64(header.Mode & 0o111)}
		if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
			member.data, err = io.ReadAll(reader)
			if err != nil {
				return nil, err
			}
		}
		members = append(members, member)
	}
	return members, nil
}

func fac198DigestFromTransportMembers(members []fac198TransportMember) (string, error) {
	records := make([]sourceManifestRecord, 0, len(members))
	for _, member := range members {
		if member.directory {
			continue
		}
		var kind byte
		var size int64
		var digest string
		switch {
		case member.data != nil:
			sum := sha256.Sum256(member.data)
			kind, size, digest = 'f', int64(len(member.data)), hex.EncodeToString(sum[:])
		default:
			sum := sha256.Sum256([]byte(member.target))
			kind, size, digest = 'l', int64(len(member.target)), hex.EncodeToString(sum[:])
		}
		records = append(records, sourceManifestRecord{name: member.name, kind: kind, mode: member.mode, size: size, digest: digest})
	}
	return sourceManifestDigestFromRecords(records)
}

func TestFAC198SourceTransferRejectsDuplicateTransportRoot(t *testing.T) {
	archive := fac198ArchiveWithHeaders(t, tar.Header{Name: "source/member.go", Mode: 0o644, Uid: 0, Gid: 0, Size: 1})
	if _, err := rootCandidateArchiveForDocker(archive); err == nil {
		t.Fatal("pre-rooted source archive unexpectedly accepted")
	}
}

func TestFAC198SourceTransferRewritesLongPAXFieldsAndBoundsOutput(t *testing.T) {
	longName := "pkg/" + strings.Repeat("a", 300) + ".go"
	longTarget := strings.Repeat("target", 300)
	archive := fac198ArchiveWithHeaders(t,
		tar.Header{Name: longName, Mode: 0o644, Uid: 0, Gid: 0, Size: 1},
		tar.Header{Name: "pkg/link", Typeflag: tar.TypeSymlink, Mode: 0o777, Uid: 0, Gid: 0, Linkname: longTarget},
	)
	transport, err := rootCandidateArchiveForDocker(archive)
	if err != nil {
		t.Fatal(err)
	}
	members, err := fac198ReadRootedTransport(transport)
	if err != nil {
		t.Fatal(err)
	}
	if members[0].name != longName || members[1].name != "pkg/link" || members[1].target != longTarget {
		t.Fatalf("rewritten members=%+v", members)
	}
	var oversized bytes.Buffer
	writer := tar.NewWriter(&oversized)
	for index := 0; index <= maxHermeticSourceTransportMembers; index++ {
		if err := writer.WriteHeader(&tar.Header{Name: fmt.Sprintf("member/%05d", index), Mode: 0o644, Uid: 0, Gid: 0}); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rootCandidateArchiveForDocker(oversized.Bytes()); err == nil {
		t.Fatal("oversized transport archive unexpectedly accepted")
	}
}

func TestFAC151HermeticRunnerRejectsForeignInspectionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		call int
	}{
		{name: "pre-start", call: 1},
		{name: "post-start", call: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fac198DockerFake{foreignAt: tc.call}
			runner := newFAC198FakeRunner(t, fake)
			result, runErr := runner.Run(context.Background())
			if runErr == nil || result.Removed != true || fake.removeID != fac198PrimaryContainerID {
				t.Fatalf("foreign identity result=%+v err=%v removal=%q", result, runErr, fake.removeID)
			}
			if len(fake.copies) != 0 || fake.execCalls != 0 {
				t.Fatalf("foreign identity reached copy/compile/execute: copies=%d exec=%d", len(fake.copies), fake.execCalls)
			}
			if result.ContainerID != fac198PrimaryContainerID {
				t.Fatalf("result bound foreign identity: %q", result.ContainerID)
			}
			if fake.postInspect != 1 {
				t.Fatalf("teardown post-inspection count=%d, want 1", fake.postInspect)
			}
		})
	}
}

func TestFAC151HermeticRunnerJoinsPrimaryAndTeardownFailures(t *testing.T) {
	primaryErr := errors.New("execution primary failure")
	removeErr := errors.New("container removal failure")
	fake := &fac198DockerFake{executionErr: primaryErr, removeErr: removeErr}
	runner := newFAC198FakeRunner(t, fake)
	result, runErr := runner.Run(context.Background())
	if runErr == nil || !errors.Is(runErr, primaryErr) || !errors.Is(runErr, removeErr) {
		t.Fatalf("joined failure=%v, want primary and teardown causes", runErr)
	}
	if result.Removed {
		t.Fatal("failed removal was reported as removed")
	}
	if fake.removeID != fac198PrimaryContainerID || fake.postInspect != 1 {
		t.Fatalf("teardown evidence id=%q post_inspect=%d", fake.removeID, fake.postInspect)
	}
}

func TestFAC198DockerCommandBoundary(t *testing.T) {
	id := strings.Repeat("a", 64)
	exactStderr := "Error response from daemon: No such container: " + id
	process := func(stderr string, exitCode int) dockerCommandFunc {
		return func(context.Context, []byte, []string) (dockerResult, error) {
			result := dockerResult{ExitCode: exitCode, Stderr: []byte(stderr)}
			return result, errors.New("docker command failed")
		}
	}
	_, err := runDockerWithProcess(context.Background(), nil, []string{"inspect", id}, process(exactStderr, 1))
	var commandErr *dockerCommandError
	if !errors.As(err, &commandErr) || commandErr.Operation != "inspect" || commandErr.ExitCode != 1 || commandErr.Stderr != exactStderr {
		t.Fatalf("typed command error=%+v, err=%v", commandErr, err)
	}
	if !isDockerContainerAbsent(err, id) {
		t.Fatal("exact requested-container absence was not classified")
	}

	for name, stderr := range map[string]string{
		"wrong-id":           "Error response from daemon: No such container: " + strings.Repeat("b", 64),
		"daemon-unavailable": "Error response from daemon: Cannot connect to the Docker daemon",
		"generic-not-found":  "resource not found",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := runDockerWithProcess(context.Background(), nil, []string{"inspect", id}, process(stderr, 1))
			if isDockerContainerAbsent(err, id) {
				t.Fatalf("near-miss stderr was classified absent: %q", stderr)
			}
		})
	}

	inspectJSON := []byte(`[{"Id":"` + id + `","Config":{"Image":"image","User":"user"}}]`)
	inspectable := fixedDockerCLI{command: func(_ context.Context, _ []byte, args []string) (dockerResult, error) {
		if args[0] == "inspect" {
			return dockerResult{Output: inspectJSON, Stdout: inspectJSON}, nil
		}
		return dockerResult{}, nil
	}}
	if err := inspectable.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectable.Inspect(context.Background(), id); err != nil {
		t.Fatalf("success-still-inspectable should remain a hard inspect result, got %v", err)
	}
	if isDockerContainerAbsent(nil, id) {
		t.Fatal("successful inspect was classified absent")
	}

	for _, malformed := range []string{"", strings.Repeat("a", 64), strings.Repeat("a", 64) + "\r\n", " " + strings.Repeat("a", 64) + "\n", strings.Repeat("a", 64) + " \n", strings.Repeat("a", 64) + "\n\n", strings.Repeat("a", 64) + "\n" + strings.Repeat("a", 64) + "\n", strings.Repeat("A", 64) + "\n", "sha256:" + strings.Repeat("a", 64) + "\n", "prefix" + strings.Repeat("a", 64) + "\n", strings.Repeat("a", 64) + "suffix\n"} {
		t.Run("create-"+fmt.Sprintf("%d", len(malformed)), func(t *testing.T) {
			cli := fixedDockerCLI{command: func(context.Context, []byte, []string) (dockerResult, error) {
				return dockerResult{Stdout: []byte(malformed)}, nil
			}}
			if _, err := cli.Create(context.Background()); err == nil {
				t.Fatalf("malformed Docker ID accepted: %q", malformed)
			}
		})
	}
	valid := fixedDockerCLI{command: func(context.Context, []byte, []string) (dockerResult, error) {
		return dockerResult{Stdout: []byte(id + "\n")}, nil
	}}
	if got, err := valid.Create(context.Background()); err != nil || got != id {
		t.Fatalf("valid Docker ID result=%q err=%v", got, err)
	}
}

func TestFAC151HermeticRunnerTimeoutUsesIndependentTeardown(t *testing.T) {
	fake := &fac198DockerFake{blockStart: make(chan struct{})}
	runner := newFAC198FakeRunner(t, fake)
	runner.operationTimeout = time.Millisecond
	result, runErr := runner.Run(context.Background())
	if runErr == nil || !errors.Is(runErr, context.DeadlineExceeded) {
		t.Fatalf("blocked operation error=%v, want deadline", runErr)
	}
	if !result.Removed || fake.removeID != fac198PrimaryContainerID || fake.postInspect != 1 {
		t.Fatalf("independent teardown evidence removed=%t id=%q inspections=%d", result.Removed, fake.removeID, fake.postInspect)
	}
}

func isValidSHADigest(s string) bool {
	if !strings.HasPrefix(s, "sha256:") {
		return false
	}
	hexPart := strings.TrimPrefix(s, "sha256:")
	if len(hexPart) != 64 || strings.ToLower(hexPart) != hexPart {
		return false
	}
	_, err := hex.DecodeString(hexPart)
	return err == nil
}

// TestFAC216HermeticImagePinsAreSinglePlatformManifests validates every entry
// in hermeticImagePins at test time. Each pin's MediaType must be a
// single-platform manifest (not a multi-arch index), ConfigDigest must be a
// valid sha256 distinct from the Image manifest digest, and every field must be
// populated. This guards against the FAC-151 regression where the amd64 pin
// pointed at a multi-arch INDEX digest with ConfigDigest set to the manifest
// digest (one level off).
func TestFAC216HermeticImagePinsAreSinglePlatformManifests(t *testing.T) {
	for arch, pin := range hermeticImagePins {
		t.Run(arch, func(t *testing.T) {
			if !isImageManifestMediaType(pin.MediaType) {
				t.Fatalf("MediaType %q is not a recognized single-platform manifest type", pin.MediaType)
			}
			if isImageIndexMediaType(pin.MediaType) {
				t.Fatalf("MediaType %q is a multi-arch index type, not a manifest", pin.MediaType)
			}
			if pin.Platform == "" || pin.Architecture == "" {
				t.Fatalf("pin for %s has an empty Platform or Architecture", arch)
			}
			refDigest := pinnedDockerReferenceDigest(pin.Image)
			if refDigest == "" {
				t.Fatalf("Image %q is not a valid pinned reference (golang@sha256:...)", pin.Image)
			}
			if !isValidSHADigest(pin.ConfigDigest) {
				t.Fatalf("ConfigDigest %q is not a valid sha256 digest", pin.ConfigDigest)
			}
			if pin.ConfigDigest == refDigest {
				t.Fatalf("ConfigDigest equals the Image manifest digest %q — a config blob is always distinct from its manifest", pin.ConfigDigest)
			}
		})
	}
}

// TestFAC216ImageMediaTypeGuardRejectsIndexes verifies the mediaType guard
// functions correctly classify manifest and index types. This is the
// regression-proving half of the pin guard — a test that cannot fail is a
// finding, not coverage.
func TestFAC216ImageMediaTypeGuardRejectsIndexes(t *testing.T) {
	manifestTypes := []string{mediaTypeDockerManifest, mediaTypeOCIManifest}
	for _, mt := range manifestTypes {
		if !isImageManifestMediaType(mt) {
			t.Fatalf("isImageManifestMediaType rejected valid manifest type %q", mt)
		}
		if isImageIndexMediaType(mt) {
			t.Fatalf("isImageIndexMediaType accepted manifest type %q as an index", mt)
		}
	}
	indexTypes := []string{mediaTypeDockerManifestList, mediaTypeOCIImageIndex}
	for _, mt := range indexTypes {
		if isImageManifestMediaType(mt) {
			t.Fatalf("isImageManifestMediaType accepted index type %q as a manifest", mt)
		}
		if !isImageIndexMediaType(mt) {
			t.Fatalf("isImageIndexMediaType rejected valid index type %q", mt)
		}
	}
	for _, mt := range []string{"", "application/vnd.other", "text/plain"} {
		if isImageManifestMediaType(mt) {
			t.Fatalf("isImageManifestMediaType accepted unknown type %q", mt)
		}
		if isImageIndexMediaType(mt) {
			t.Fatalf("isImageIndexMediaType accepted unknown type %q", mt)
		}
	}
}
