package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/keys-i/blackbeard/src/internal/domain"
	"github.com/keys-i/blackbeard/src/internal/index"
	"github.com/keys-i/blackbeard/src/internal/provider"
)

const appTestHash = "0123456789abcdef0123456789abcdef01234567"

type syncFunc func(context.Context) (provider.CatalogSnapshot, error)

func (f syncFunc) Sync(ctx context.Context) (provider.CatalogSnapshot, error) { return f(ctx) }

func TestProvidersSyncReconcilesAggregateAndOfflineSearchDoesNotSync(t *testing.T) {
	t.Parallel()

	cacheRoot := t.TempDir()
	archive := domain.Record{Provider: "internet_archive", SourceID: "archive", Title: "Public archive film"}
	oldAcademic := domain.Record{Provider: academicProvider, SourceID: "old-academic", Title: "Obsolete academic record"}
	seedCatalogue(t, cacheRoot, []domain.Record{archive, oldAcademic})
	size := int64(42)
	academic := domain.Record{
		Provider: "academic_torrents", SourceID: appTestHash, Title: "Climate dataset",
		InfoHashV1: appTestHash, SizeBytes: &size,
	}
	constructs := 0
	deps := testCatalogueDeps(cacheRoot, func(path string) (provider.CatalogSyncer, error) {
		constructs++
		want := filepath.Join(cacheRoot, "blackbeard", "providers", academicProvider)
		if path != want || !filepath.IsAbs(path) {
			t.Fatalf("provider cache path = %q, want %q", path, want)
		}
		return syncFunc(func(ctx context.Context) (provider.CatalogSnapshot, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("sync context has no deadline")
			}
			return provider.CatalogSnapshot{Records: []domain.Record{academic}, NotModified: true}, nil
		}), nil
	})

	stdout, stderr, err := runCLI(context.Background(), deps, "--output", "json", "providers", "sync")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" {
		t.Fatalf("sync stdout=%q stderr=%q", stdout, stderr)
	}
	var syncEnvelope struct {
		SchemaVersion int        `json:"schema_version"`
		Type          string     `json:"type"`
		Data          syncOutput `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &syncEnvelope); err != nil {
		t.Fatal(err)
	}
	if syncEnvelope.SchemaVersion != 1 || syncEnvelope.Type != "provider_sync" || len(syncEnvelope.Data.Providers) != 1 || syncEnvelope.Data.Providers[0].Status != "unchanged" {
		t.Fatalf("sync envelope = %#v", syncEnvelope)
	}

	stdout, stderr, err = runCLI(context.Background(), deps, "--output", "json", "search", "--offline", "climate")
	if err != nil {
		t.Fatal(err)
	}
	if stderr != "" || constructs != 1 {
		t.Fatalf("offline search constructed provider: constructs=%d stderr=%q", constructs, stderr)
	}
	var envelope struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		Data          struct {
			Mode    string         `json:"mode"`
			Results []searchResult `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.SchemaVersion != 1 || envelope.Type != "search" || envelope.Data.Mode != "offline" || len(envelope.Data.Results) != 1 || envelope.Data.Results[0].Record.SourceID != appTestHash {
		t.Fatalf("search output = %#v", envelope)
	}

	stdout, _, err = runCLI(context.Background(), deps, "search", "--offline", "public archive")
	if err != nil || !strings.Contains(stdout, "Public archive film") {
		t.Fatalf("last-good other-provider record was lost: stdout=%q err=%v", stdout, err)
	}
	stdout, _, err = runCLI(context.Background(), deps, "search", "--offline", "obsolete")
	if err != nil || strings.Contains(stdout, oldAcademic.Title) {
		t.Fatalf("replaced academic record remains: stdout=%q err=%v", stdout, err)
	}

	stdout, stderr, err = runCLI(context.Background(), deps, "--output", "ndjson", "providers", "sync")
	if err != nil || stderr != "" {
		t.Fatalf("sync NDJSON stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	assertStreamTypes(t, stdout, "provider_sync", "done")
}

func TestOfflineSearchFormatsAndFlagPrecedence(t *testing.T) {
	t.Parallel()

	cacheRoot := t.TempDir()
	records := []domain.Record{
		{Provider: academicProvider, SourceID: "academic", Title: "Zulu dataset"},
		{Provider: "debian", SourceID: "debian", Title: "Alpha dataset"},
	}
	seedCatalogue(t, cacheRoot, records)
	deps := testCatalogueDeps(cacheRoot, func(string) (provider.CatalogSyncer, error) {
		t.Fatal("offline search constructed a provider")
		return nil, nil
	})

	stdout, stderr, err := runCLI(context.Background(), deps,
		"--output", "ndjson", "search", "--offline", "--provider", "academic", "--sort", "title-desc", "--limit", "1", "zulu from debian")
	if err != nil || stderr != "" {
		t.Fatalf("stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"type":"result"`) || !strings.Contains(lines[0], `"source_id":"academic"`) || !strings.Contains(lines[1], `"type":"done"`) {
		t.Fatalf("NDJSON = %q", stdout)
	}
	assertStreamTypes(t, stdout, "result", "done")

	stdout, stderr, err = runCLI(context.Background(), deps, "search", "--offline", "no-such-result")
	if err != nil || stderr != "" || !strings.HasPrefix(stdout, "RANK") {
		t.Fatalf("zero-result table stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	stdout, stderr, err = runCLI(context.Background(), deps, "--output", "json", "search", "--offline", "no-such-result")
	if err != nil || stderr != "" {
		t.Fatalf("zero-result JSON stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	var emptyEnvelope struct {
		SchemaVersion int    `json:"schema_version"`
		Type          string `json:"type"`
		Data          struct {
			Results []json.RawMessage `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &emptyEnvelope); err != nil || emptyEnvelope.SchemaVersion != 1 || emptyEnvelope.Type != "search" || emptyEnvelope.Data.Results == nil || len(emptyEnvelope.Data.Results) != 0 {
		t.Fatalf("zero-result JSON = %q, %v", stdout, err)
	}

	for _, args := range [][]string{
		{"search", "--offline", "--provider", "unknown", "dataset"},
		{"search", "--offline", "--provider", "academic sort:newest", "dataset"},
		{"search", "--offline", "--sort", "random", "dataset"},
		{"search", "--offline", "--sort", "newest provider:debian", "dataset"},
		{"search", "--offline", "--limit", "0", "dataset"},
		{"search", "--offline", "--explain", "dataset"},
	} {
		stdout, stderr, err = runCLI(context.Background(), deps, args...)
		if err == nil || ExitCode(err) != 2 || stdout != "" || stderr != "" {
			t.Fatalf("run(%q) stdout=%q stderr=%q err=%v code=%d", args, stdout, stderr, err, ExitCode(err))
		}
	}
	stdout, stderr, err = runCLI(context.Background(), deps, "search", "--help")
	if err != nil || stderr != "" || !strings.Contains(stdout, "blackbeard search [flags] <query>") || !strings.Contains(stdout, "Put flags before the query") {
		t.Fatalf("search help stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
}

func TestSyncFailureAndCancellationPreserveLiveIndex(t *testing.T) {
	t.Parallel()

	cacheRoot := t.TempDir()
	old := domain.Record{Provider: "debian", SourceID: "stable", Title: "Stable Debian image"}
	seedCatalogue(t, cacheRoot, []domain.Record{old})
	conflict := []domain.Record{
		{Provider: academicProvider, SourceID: "same", Title: "First", InfoHashV1: strings.Repeat("1", 40)},
		{Provider: academicProvider, SourceID: "same", Title: "Second", InfoHashV1: strings.Repeat("2", 40)},
	}
	var deps catalogueDeps
	for name, records := range map[string][]domain.Record{
		"empty":    {},
		"foreign":  {{Provider: "debian", SourceID: "foreign", Title: "Foreign"}},
		"conflict": conflict,
	} {
		t.Run(name, func(t *testing.T) {
			deps = testCatalogueDeps(cacheRoot, func(string) (provider.CatalogSyncer, error) {
				return syncFunc(func(context.Context) (provider.CatalogSnapshot, error) {
					return provider.CatalogSnapshot{Records: records}, nil
				}), nil
			})
			stdout, stderr, err := runCLI(context.Background(), deps, "providers", "sync")
			if err == nil || stdout != "" || stderr != "" {
				t.Fatalf("failed rebuild stdout=%q stderr=%q err=%v", stdout, stderr, err)
			}
		})
	}

	searchDeps := testCatalogueDeps(cacheRoot, func(string) (provider.CatalogSyncer, error) {
		return nil, errors.New("must not be called")
	})
	stdout, _, err := runCLI(context.Background(), searchDeps, "search", "--offline", "stable debian")
	if err != nil || !strings.Contains(stdout, old.Title) {
		t.Fatalf("previous index unavailable: stdout=%q err=%v", stdout, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stdout, stderr, err := runCLI(ctx, deps, "providers", "sync")
	if err == nil || ExitCode(err) != 130 || stdout != "" || stderr != "" {
		t.Fatalf("cancelled sync stdout=%q stderr=%q err=%v code=%d", stdout, stderr, err, ExitCode(err))
	}
}

func TestOfflineSearchRejectsMissingAndRelativeCache(t *testing.T) {
	t.Parallel()

	for _, root := range []string{t.TempDir(), "relative-cache"} {
		deps := testCatalogueDeps(root, func(string) (provider.CatalogSyncer, error) {
			return nil, errors.New("must not be called")
		})
		stdout, stderr, err := runCLI(context.Background(), deps, "search", "--offline", "dataset")
		if err == nil || stdout != "" || stderr != "" {
			t.Fatalf("root=%q stdout=%q stderr=%q err=%v", root, stdout, stderr, err)
		}
		if filepath.IsAbs(root) && !strings.Contains(err.Error(), "providers sync") {
			t.Fatalf("missing-index error = %v", err)
		}
	}
}

func TestFailedFirstSyncDoesNotInitializeCatalogue(t *testing.T) {
	t.Parallel()

	for name, syncer := range map[string]provider.CatalogSyncer{
		"provider": syncFunc(func(context.Context) (provider.CatalogSnapshot, error) {
			return provider.CatalogSnapshot{}, errors.New("offline")
		}),
		"rebuild": syncFunc(func(context.Context) (provider.CatalogSnapshot, error) {
			return provider.CatalogSnapshot{Records: []domain.Record{
				{Provider: academicProvider, SourceID: "same", Title: "First", InfoHashV1: strings.Repeat("1", 40)},
				{Provider: academicProvider, SourceID: "same", Title: "Second", InfoHashV1: strings.Repeat("2", 40)},
			}}, nil
		}),
	} {
		t.Run(name, func(t *testing.T) {
			cacheRoot := t.TempDir()
			deps := testCatalogueDeps(cacheRoot, func(string) (provider.CatalogSyncer, error) { return syncer, nil })
			if stdout, stderr, err := runCLI(context.Background(), deps, "providers", "sync"); err == nil || stdout != "" || stderr != "" {
				t.Fatalf("first sync stdout=%q stderr=%q err=%v", stdout, stderr, err)
			}
			stdout, stderr, err := runCLI(context.Background(), deps, "search", "--offline", "anything")
			if err == nil || stdout != "" || stderr != "" || !strings.Contains(err.Error(), "providers sync") {
				t.Fatalf("offline search stdout=%q stderr=%q err=%v", stdout, stderr, err)
			}
			paths, err := resolveCataloguePaths(deps)
			if err != nil {
				t.Fatal(err)
			}
			for _, candidate := range []string{paths.index, paths.index + ".previous"} {
				if _, err := os.Lstat(candidate); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("incomplete generation %q remains: %v", candidate, err)
				}
			}
		})
	}
}

func TestSyncRejectsSymlinkedApplicationCache(t *testing.T) {
	t.Parallel()

	cacheRoot := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(cacheRoot, "blackbeard")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	constructed := false
	deps := testCatalogueDeps(cacheRoot, func(string) (provider.CatalogSyncer, error) {
		constructed = true
		return nil, errors.New("must not be called")
	})
	stdout, stderr, err := runCLI(context.Background(), deps, "providers", "sync")
	if err == nil || stdout != "" || stderr != "" || constructed {
		t.Fatalf("sync stdout=%q stderr=%q constructed=%v err=%v", stdout, stderr, constructed, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("outside cache entries=%v err=%v", entries, err)
	}
}

func TestCommandsRejectLinkedCatalogueGeneration(t *testing.T) {
	t.Parallel()

	for _, generation := range []struct{ name, suffix string }{{"live", ""}, {"previous", ".previous"}} {
		t.Run(generation.name, func(t *testing.T) {
			cacheRoot := t.TempDir()
			deps := testCatalogueDeps(cacheRoot, func(string) (provider.CatalogSyncer, error) {
				t.Fatal("sync constructed provider for unsafe catalogue generation")
				return nil, nil
			})
			paths, err := resolveCataloguePaths(deps)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(paths.index), 0o700); err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			candidate := paths.index + generation.suffix
			if err := os.Symlink(outside, candidate); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}

			for _, args := range [][]string{{"providers", "sync"}, {"search", "--offline", "dataset"}} {
				stdout, stderr, err := runCLI(context.Background(), deps, args...)
				if err == nil || stdout != "" || stderr != "" {
					t.Fatalf("run(%q) stdout=%q stderr=%q err=%v", args, stdout, stderr, err)
				}
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "unchanged" {
				t.Fatalf("outside sentinel = %q, %v", data, err)
			}
			if info, err := os.Lstat(candidate); err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("unsafe generation was mutated: %v, %v", info, err)
			}
		})
	}
}

func BenchmarkSearchOfflineCLI(b *testing.B) {
	cacheRoot := b.TempDir()
	records := make([]domain.Record, 100)
	for i := range records {
		records[i] = domain.Record{Provider: academicProvider, SourceID: "record-" + strconv.Itoa(i), Title: "Climate dataset"}
	}
	seedCatalogue(b, cacheRoot, records)
	deps := testCatalogueDeps(cacheRoot, func(string) (provider.CatalogSyncer, error) { return nil, errors.New("unused") })
	b.ReportAllocs()
	for b.Loop() {
		if err := run(context.Background(), []string{"--output", "json", "search", "--offline", "climate"}, strings.NewReader(""), io.Discard, io.Discard, "bench", deps); err != nil {
			b.Fatal(err)
		}
	}
}

func testCatalogueDeps(root string, newAcademic func(string) (provider.CatalogSyncer, error)) catalogueDeps {
	return catalogueDeps{
		userCacheDir: func() (string, error) { return root, nil },
		newAcademic:  newAcademic,
	}
}

func seedCatalogue(tb testing.TB, cacheRoot string, records []domain.Record) {
	tb.Helper()
	paths, err := resolveCataloguePaths(testCatalogueDeps(cacheRoot, nil))
	if err != nil {
		tb.Fatal(err)
	}
	store, err := index.Open(paths.index)
	if err != nil {
		tb.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), records); err != nil {
		_ = store.Close()
		tb.Fatal(err)
	}
	if err := store.Close(); err != nil {
		tb.Fatal(err)
	}
}

func runCLI(ctx context.Context, deps catalogueDeps, args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := run(ctx, args, strings.NewReader(""), &stdout, &stderr, "test", deps)
	return stdout.String(), stderr.String(), err
}

func assertStreamTypes(t *testing.T, stream string, types ...string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(stream), "\n")
	if len(lines) != len(types) {
		t.Fatalf("NDJSON lines = %q", stream)
	}
	for i, line := range lines {
		var record struct {
			SchemaVersion int    `json:"schema_version"`
			Type          string `json:"type"`
			Sequence      uint64 `json:"sequence"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil || record.SchemaVersion != 1 || record.Type != types[i] || record.Sequence != uint64(i+1) {
			t.Fatalf("NDJSON record %d = %q, %#v, %v", i+1, line, record, err)
		}
	}
}
