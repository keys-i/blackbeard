package index

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/keys-i/blackbeard/src/internal/domain"
	productquery "github.com/keys-i/blackbeard/src/internal/query"
)

func TestRebuildSearchesFixtureWithWeightedStableRanking(t *testing.T) {
	t.Parallel()

	records := fixtureRecords(t)
	store := newTestStore(t, records)
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "relevance.json"))
	if err != nil {
		t.Fatal(err)
	}
	var judgements []struct {
		Query             string   `json:"query"`
		ExpectedSourceIDs []string `json:"expected_source_ids"`
	}
	if err := json.Unmarshal(data, &judgements); err != nil {
		t.Fatal(err)
	}
	for _, judgement := range judgements {
		ast, err := productquery.Parse(judgement.Query)
		if err != nil {
			t.Fatalf("Parse(%q): %v", judgement.Query, err)
		}
		hits, err := store.Search(context.Background(), ast, DefaultLimit)
		if err != nil {
			t.Fatalf("Search(%q): %v", judgement.Query, err)
		}
		got := sourceIDs(hits)
		if !reflect.DeepEqual(got, judgement.ExpectedSourceIDs) {
			t.Fatalf("Search(%q) source IDs = %v, want %v", judgement.Query, got, judgement.ExpectedSourceIDs)
		}
	}

	ast, _ := productquery.Parse("climate")
	first, err := store.Search(context.Background(), ast, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Search(context.Background(), ast, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated search changed order:\n%#v\n%#v", first, second)
	}
}

func TestExactFiltersKeepUnknownDistinctFromZero(t *testing.T) {
	t.Parallel()

	date2024 := date("2024-06-01")
	date2022 := date("2022-06-01")
	oneGiB := int64(1 << 30)
	twoGiB := int64(2 << 30)
	store := newTestStore(t, []domain.Record{
		record("academic_torrents", "match", "Climate dataset", withSize(oneGiB), withDate(date2024), withKinds("dataset")),
		record("academic_torrents", "too-large", "Climate dataset large", withSize(twoGiB), withDate(date2024), withKinds("dataset")),
		record("academic_torrents", "too-old", "Climate dataset old", withSize(oneGiB), withDate(date2022), withKinds("dataset")),
		record("academic_torrents", "unknown", "Climate dataset unknown", withKinds("dataset")),
		record("internet_archive", "wrong-provider", "Climate dataset archive", withSize(oneGiB), withDate(date2024), withKinds("dataset")),
	})
	ast, err := productquery.Parse("climate dataset after 2023 under 2 GiB academic sources only")
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(context.Background(), ast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(hits); !reflect.DeepEqual(got, []string{"match"}) {
		t.Fatalf("source IDs = %v", got)
	}
}

func TestTransitiveInfohashAndProviderScopedFallbackDedupe(t *testing.T) {
	t.Parallel()

	v1 := strings.Repeat("1", 40)
	v2 := strings.Repeat("a", 64)
	store := newTestStore(t, []domain.Record{
		record("internet_archive", "bridge", "Payload bridge", withHashes(v1, v2)),
		record("academic_torrents", "v1", "Payload v1", withHashes(v1, "")),
		record("debian", "v2", "Payload v2", withHashes("", v2)),
		record("internet_archive", "no-hash", "Payload fallback"),
		record("internet_archive", "no-hash", "Payload fallback richer", withDescription("extra metadata")),
		record("academic_torrents", "no-hash", "Payload other provider"),
	})
	ast, _ := productquery.Parse("payload")
	hits, err := store.Search(context.Background(), ast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("deduplicated hit count = %d, want 3: %#v", len(hits), hits)
	}
	if hits[0].ID == "" || hits[1].ID == "" || hits[2].ID == "" {
		t.Fatalf("empty stable IDs: %#v", hits)
	}
	wantGroup := digest(strings.Join([]string{"v1:" + v1, "v2:" + v2}, "\x00"))
	foundGroup := false
	for _, hit := range hits {
		if len(hit.Sources) == 3 {
			foundGroup = hit.ID == wantGroup
		}
	}
	if !foundGroup {
		t.Fatalf("transitive group ID/provenance missing: %#v", hits)
	}
	if count, err := store.index.DocCount(); err != nil || count != 5 {
		t.Fatalf("document count = %d, %v; want one per group/provider (5)", count, err)
	}

	academic, _ := productquery.Parse("payload from academic")
	hits, err = store.Search(context.Background(), academic, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(hits); !slices.Equal(got, []string{"v1", "no-hash"}) {
		t.Fatalf("provider-filtered source IDs = %v", got)
	}
	if hits[0].Record.Provider != "academic_torrents" || len(hits[0].Sources) != 3 {
		t.Fatalf("filtered representative/provenance = %#v", hits[0])
	}
}

func TestRecordsReturnsEachProviderSourceOnce(t *testing.T) {
	t.Parallel()

	shared := strings.Repeat("7", 40)
	want := []domain.Record{
		record("academic_torrents", "academic", "Shared academic", withHashes(shared, "")),
		record("debian", "debian", "Debian image"),
		record("internet_archive", "archive", "Shared archive", withHashes(shared, "")),
	}
	store := newTestStore(t, want)
	got, err := store.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Records() = %#v, want %#v", got, want)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Records(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Records() error = %v", err)
	}
}

func TestRecordsPaginatesAndObservesMidReadCancellation(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, benchmarkRecords(batchSize+89))
	records, err := store.Records(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != batchSize+89 {
		t.Fatalf("Records() count = %d, want %d", len(records), batchSize+89)
	}

	ctx := newStepCancelContext(context.Background(), 1)
	if _, err := store.Records(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-read cancellation error = %v", err)
	}
}

func TestUnicodePhrasesStayWithinOneSourceField(t *testing.T) {
	t.Parallel()

	v1 := strings.Repeat("2", 40)
	store := newTestStore(t, []domain.Record{
		record("internet_archive", "unicode", "Die Straße SEE"),
		record("internet_archive", "first", "alpha", withHashes(v1, "")),
		record("internet_archive", "second", "beta", withHashes(v1, "")),
		record("debian", "structured", "unrelated", func(record *domain.Record) { record.Categories = []string{"public", "domain"} }),
	})
	for _, query := range []string{"Straße", `"Straße SEE"`} {
		ast, _ := productquery.Parse(query)
		hits, err := store.Search(context.Background(), ast, 10)
		if err != nil || !slices.Equal(sourceIDs(hits), []string{"unicode"}) {
			t.Fatalf("Search(%q) = %#v, %v", query, hits, err)
		}
		if hits[0].Record.Title != "Die Straße SEE" {
			t.Fatalf("display title changed: %q", hits[0].Record.Title)
		}
	}
	for _, query := range []string{`"alpha beta"`, `"public domain"`} {
		ast, _ := productquery.Parse(query)
		if hits, err := store.Search(context.Background(), ast, 10); err != nil || len(hits) != 0 {
			t.Fatalf("Search(%q) = %#v, %v", query, hits, err)
		}
	}
}

func TestProviderTextIsolationAndMergedHardFilters(t *testing.T) {
	t.Parallel()

	shared := strings.Repeat("3", 40)
	conflict := strings.Repeat("4", 40)
	oneGiB, twoGiB := int64(1<<30), int64(2<<30)
	store := newTestStore(t, []domain.Record{
		record("internet_archive", "nebula", "nebula payload", withHashes(shared, ""), withSize(oneGiB)),
		record("academic_torrents", "glacier", "glacier", withHashes(shared, ""), withKinds("dataset")),
		record("internet_archive", "conflict-a", "conflicted", withHashes(conflict, ""), withSize(oneGiB)),
		record("academic_torrents", "conflict-b", "conflicted", withHashes(conflict, ""), withSize(twoGiB)),
	})
	ast, _ := productquery.Parse("nebula glacier")
	if hits, err := store.Search(context.Background(), ast, 10); err != nil || len(hits) != 0 {
		t.Fatalf("cross-provider lexical match = %#v, %v", hits, err)
	}
	ast, _ = productquery.Parse("nebula kind:dataset under 2 GiB")
	if hits, err := store.Search(context.Background(), ast, 10); err != nil || len(hits) != 1 {
		t.Fatalf("merged hard-filter metadata = %#v, %v", hits, err)
	}
	ast, _ = productquery.Parse("nebula kind:dataset under 2 GiB from archive")
	if hits, err := store.Search(context.Background(), ast, 10); err != nil || len(hits) != 0 {
		t.Fatalf("provider-scoped hard-filter metadata = %#v, %v", hits, err)
	}
	ast, _ = productquery.Parse("conflicted under 3 GiB")
	if hits, err := store.Search(context.Background(), ast, 10); err != nil || len(hits) != 0 {
		t.Fatalf("conflicting exact size must remain unknown: %#v, %v", hits, err)
	}
}

func TestRequiredAnalyzedTermUsesAllTokens(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, []domain.Record{
		record("debian", "partial", "machine image"),
		record("debian", "complete", "machine learning image"),
	})
	ast, _ := productquery.Parse("machine-learning")
	hits, err := store.Search(context.Background(), ast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(hits); !slices.Equal(got, []string{"complete"}) {
		t.Fatalf("hyphenated required term matched %v", got)
	}
}

func TestProviderFilterRanksOnlyEligibleProvenance(t *testing.T) {
	t.Parallel()

	first := strings.Repeat("8", 40)
	second := strings.Repeat("9", 40)
	store := newTestStore(t, []domain.Record{
		record("academic_torrents", "sparse", "beacon", withHashes(first, "")),
		record("internet_archive", "excluded-rich", "beacon", withHashes(first, ""), withDescription("rich excluded mirror"), withSize(1), withKinds("dataset")),
		record("academic_torrents", "eligible-rich", "beacon", withHashes(second, ""), withDescription("eligible metadata")),
	})
	ast, _ := productquery.Parse("beacon from academic")
	hits, err := store.Search(context.Background(), ast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(hits); !slices.Equal(got, []string{"eligible-rich", "sparse"}) {
		t.Fatalf("provider-scoped ranking = %v", got)
	}
}

func TestHitCollectorKeepsOnlyBestLimitedGroups(t *testing.T) {
	t.Parallel()

	collector := newHitCollector(3, nil)
	for i := range 10 {
		id := strconv.Itoa(i)
		collector.add(Hit{ID: id, Score: float64(i), Record: domain.Record{SourceID: id}})
	}
	results := collector.results()
	if len(results) != 3 {
		t.Fatalf("collector length = %d", len(results))
	}
	if got := sourceIDs(results); !slices.Equal(got, []string{"9", "8", "7"}) {
		t.Fatalf("collector results = %v", got)
	}
}

func TestDefaultLimitMatchesFullTiedOrdering(t *testing.T) {
	t.Parallel()

	records := make([]domain.Record, 64)
	for i := range records {
		records[i] = record("debian", strconv.Itoa(i), "same title")
	}
	store := newTestStore(t, records)
	ast, _ := productquery.Parse("same")
	full, err := store.Search(context.Background(), ast, len(records))
	if err != nil {
		t.Fatal(err)
	}
	limited, err := store.Search(context.Background(), ast, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != len(records) || len(limited) != 1 || limited[0].ID != full[0].ID {
		t.Fatalf("limited tied result = %#v; full first = %#v", limited, full)
	}
}

func TestDuplicateSourceRejectsInfohashConflictAndCapsPreparedBytes(t *testing.T) {
	t.Parallel()

	records := []domain.Record{
		record("debian", "same", "first", withHashes(strings.Repeat("a", 40), "")),
		record("debian", "same", "second", withHashes(strings.Repeat("b", 40), "")),
	}
	if _, err := prepare(context.Background(), records); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("prepare() conflict error = %v", err)
	}
	ambiguous := []domain.Record{
		record("debian", "same", "v1", withHashes(strings.Repeat("a", 40), "")),
		record("debian", "same", "v2", withHashes("", strings.Repeat("b", 64))),
	}
	if _, err := prepare(context.Background(), ambiguous); !errors.Is(err, ErrSourceConflict) {
		t.Fatalf("prepare() ambiguous hybrid error = %v", err)
	}
	if _, err := addPreparedBytes(maxPreparedBytes-1, 2); err == nil {
		t.Fatal("addPreparedBytes accepted an oversized index")
	}
	if _, err := groupEncodedSize(make([]sourceItem, maxGroupSources+1)); err == nil {
		t.Fatal("groupEncodedSize accepted too many sources")
	}
	if _, err := addGroupBytes(maxGroupBytes, 1); err == nil {
		t.Fatal("groupEncodedSize accepted oversized metadata")
	}
}

func TestRelevanceTiesUseCompletenessThenSourceCount(t *testing.T) {
	t.Parallel()

	multi := strings.Repeat("5", 40)
	store := newTestStore(t, []domain.Record{
		record("debian", "plain", "beacon"),
		record("debian", "rich", "beacon", withDescription("metadata"), withSize(1)),
		record("debian", "multi-a", "beacon", withHashes(multi, "")),
		record("debian", "multi-b", "beacon", withHashes(multi, "")),
	})
	ast, _ := productquery.Parse("beacon")
	hits, err := store.Search(context.Background(), ast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(hits); !slices.Equal(got, []string{"rich", "multi-a", "plain"}) {
		t.Fatalf("tie order = %v", got)
	}
}

func TestExplicitOrderingPlacesMissingMetadataLast(t *testing.T) {
	t.Parallel()

	old := date("2022-01-01")
	newest := date("2025-01-01")
	store := newTestStore(t, []domain.Record{
		record("internet_archive", "old", "Dataset old", withDate(old), withKinds("dataset")),
		record("internet_archive", "new", "Dataset new", withDate(newest), withKinds("dataset")),
		record("internet_archive", "unknown", "Dataset unknown", withKinds("dataset")),
	})
	ast, _ := productquery.Parse("kind:dataset newest first")
	hits, err := store.Search(context.Background(), ast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(hits); !slices.Equal(got, []string{"new", "old", "unknown"}) {
		t.Fatalf("newest order = %v", got)
	}

	ast, _ = productquery.Parse("kind:dataset oldest first")
	hits, err = store.Search(context.Background(), ast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(hits); !slices.Equal(got, []string{"old", "new", "unknown"}) {
		t.Fatalf("oldest order = %v", got)
	}
}

func TestExplicitOrderingChoosesMatchingGroupRepresentative(t *testing.T) {
	t.Parallel()

	shared := strings.Repeat("6", 40)
	old := date("2022-01-01")
	newest := date("2025-01-01")
	store := newTestStore(t, []domain.Record{
		record("internet_archive", "rich-old", "Dataset mirror", withHashes(shared, ""), withDate(old), withKinds("dataset"), withDescription("richer metadata")),
		record("academic_torrents", "new", "Dataset mirror", withHashes(shared, ""), withDate(newest), withKinds("dataset")),
	})
	ast, _ := productquery.Parse("kind:dataset newest first")
	hits, err := store.Search(context.Background(), ast, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Record.SourceID != "new" || len(hits[0].Sources) != 2 {
		t.Fatalf("ordered group representative = %#v", hits)
	}
}

func TestRelevanceOrderingKeepsMostCompleteRepresentative(t *testing.T) {
	t.Parallel()

	shared := strings.Repeat("7", 40)
	store := newTestStore(t, []domain.Record{
		record("academic_torrents", "plain", "Shared payload", withHashes(shared, "")),
		record("internet_archive", "rich", "Shared payload", withHashes(shared, ""), withDescription("metadata")),
	})
	plain, _ := productquery.Parse("shared")
	explicit, _ := productquery.Parse("shared sort:relevance")
	tied, _ := productquery.Parse("shared sort:title")
	for _, ast := range []productquery.AST{plain, explicit, tied} {
		hits, err := store.Search(context.Background(), ast, 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 1 || hits[0].Record.SourceID != "rich" {
			t.Fatalf("relevance representative = %#v", hits)
		}
	}
}

func TestRebuildRemovesStaleDeterministicBuild(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "catalogue.bleve")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	building := path + ".building"
	if err := os.MkdirAll(building, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(building, "stale"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), []domain.Record{record("debian", "fresh", "Fresh payload")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(building); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale build remains: %v", err)
	}
}

func TestFailedRebuildPreservesOldGenerationAndReopens(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "catalogue.bleve")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), []domain.Record{record("debian", "stable", "Stable payload")}); err != nil {
		t.Fatal(err)
	}
	bad := record("debian", "bad", "Bad payload")
	bad.InfoHashV1 = "not-a-hash"
	if err := store.Rebuild(context.Background(), []domain.Record{bad}); !errors.Is(err, domain.ErrInvalidRecord) {
		t.Fatalf("invalid rebuild error = %v", err)
	}
	ast, _ := productquery.Parse("stable")
	if hits, err := store.Search(context.Background(), ast, 10); err != nil || !slices.Equal(sourceIDs(hits), []string{"stable"}) {
		t.Fatalf("old generation after failure = %#v, %v", hits, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if hits, err := store.Search(context.Background(), ast, 10); err != nil || !slices.Equal(sourceIDs(hits), []string{"stable"}) {
		t.Fatalf("reopened generation = %#v, %v", hits, err)
	}
}

func TestOpenRecoversVerifiedPreviousGeneration(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "catalogue.bleve")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), []domain.Record{record("debian", "first", "first payload")}); err != nil {
		t.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), []domain.Record{record("debian", "second", "second payload")}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".previous"); err != nil {
		t.Fatalf("verified previous generation missing: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ast, _ := productquery.Parse("first")
	if hits, err := store.Search(context.Background(), ast, 10); err != nil || !slices.Equal(sourceIDs(hits), []string{"first"}) {
		t.Fatalf("recovered search = %#v, %v", hits, err)
	}
}

func TestCancellationAndQueryCaps(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, []domain.Record{record("debian", "stable", "Stable payload")})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ast, _ := productquery.Parse("stable")
	if _, err := store.Search(ctx, ast, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error = %v", err)
	}
	if err := store.Rebuild(ctx, fixtureRecords(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Rebuild() error = %v", err)
	}
	if _, err := store.Search(context.Background(), ast, productquery.MaxLimit+1); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("oversized limit error = %v", err)
	}
}

func TestSearchPagesUntilAFilteredMatch(t *testing.T) {
	t.Parallel()

	records := benchmarkRecords(searchPageSize + 20)
	for i := range records {
		size := int64(3 << 20)
		records[i].SizeBytes = &size
	}
	items, err := prepare(context.Background(), records)
	if err != nil {
		t.Fatal(err)
	}
	lastID := items[0].id
	lastSourceID := preparedSourceID(t, items[0])
	for _, item := range items[1:] {
		if item.id > lastID {
			lastID, lastSourceID = item.id, preparedSourceID(t, item)
		}
	}
	for i := range records {
		if records[i].SourceID == lastSourceID {
			size := int64(1 << 20)
			records[i].SizeBytes = &size
		}
	}
	store := newTestStore(t, records)
	ast, _ := productquery.Parse("climate under 2 MiB")
	hits, err := store.Search(context.Background(), ast, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceIDs(hits); !slices.Equal(got, []string{lastSourceID}) {
		t.Fatalf("paged source IDs = %v, want %q", got, lastSourceID)
	}
}

func preparedSourceID(t *testing.T, item prepared) string {
	t.Helper()
	var sources []domain.Record
	if err := json.Unmarshal([]byte(item.encoded), &sources); err != nil || len(sources) != 1 {
		t.Fatalf("prepared sources = %#v, %v", sources, err)
	}
	return sources[0].SourceID
}

func BenchmarkIndexRebuild(b *testing.B) {
	records := benchmarkRecords(1000)
	for b.Loop() {
		path := filepath.Join(b.TempDir(), "catalogue.bleve")
		store, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := store.Rebuild(context.Background(), records); err != nil {
			b.Fatal(err)
		}
		if err := store.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchWarm(b *testing.B) {
	store := benchmarkStore(b, benchmarkRecords(1000))
	ast, _ := productquery.Parse("climate dataset")
	b.ResetTimer()
	for b.Loop() {
		if _, err := store.Search(context.Background(), ast, 50); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSearchColdOpen(b *testing.B) {
	path := filepath.Join(b.TempDir(), "catalogue.bleve")
	store, err := Open(path)
	if err != nil {
		b.Fatal(err)
	}
	if err := store.Rebuild(context.Background(), benchmarkRecords(1000)); err != nil {
		b.Fatal(err)
	}
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
	ast, _ := productquery.Parse("climate dataset")
	b.ResetTimer()
	for b.Loop() {
		store, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := store.Search(context.Background(), ast, 50); err != nil {
			b.Fatal(err)
		}
		if err := store.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

type recordOption func(*domain.Record)

func record(provider, sourceID, title string, options ...recordOption) domain.Record {
	r := domain.Record{Provider: provider, SourceID: sourceID, Title: title}
	for _, option := range options {
		option(&r)
	}
	return r
}

func withSize(value int64) recordOption { return func(r *domain.Record) { r.SizeBytes = &value } }
func withDate(value time.Time) recordOption {
	return func(r *domain.Record) { r.PublishedAt = &value }
}
func withKinds(values ...string) recordOption {
	return func(r *domain.Record) { r.ContentKinds = values }
}
func withDescription(value string) recordOption {
	return func(r *domain.Record) { r.Description = value }
}
func withHashes(v1, v2 string) recordOption {
	return func(r *domain.Record) { r.InfoHashV1, r.InfoHashV2 = v1, v2 }
}

func date(value string) time.Time {
	parsed, err := time.Parse(time.DateOnly, value)
	if err != nil {
		panic(err)
	}
	return parsed
}

func fixtureRecords(t testing.TB) []domain.Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "catalogue.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records []domain.Record
	if err := json.Unmarshal(data, &records); err != nil {
		t.Fatal(err)
	}
	return records
}

func newTestStore(t *testing.T, records []domain.Record) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalogue.bleve"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Rebuild(context.Background(), records); err != nil {
		t.Fatal(err)
	}
	return store
}

func benchmarkStore(b *testing.B, records []domain.Record) *Store {
	b.Helper()
	store, err := Open(filepath.Join(b.TempDir(), "catalogue.bleve"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	if err := store.Rebuild(context.Background(), records); err != nil {
		b.Fatal(err)
	}
	return store
}

func benchmarkRecords(count int) []domain.Record {
	records := make([]domain.Record, count)
	for i := range records {
		size := int64(i+1) << 20
		records[i] = record("academic_torrents", "record-"+strconv.Itoa(i), "Climate dataset "+strconv.Itoa(i), withSize(size), withKinds("dataset"), withDescription("Generated weather observations for repeatable local benchmarks."))
	}
	return records
}

func sourceIDs(hits []Hit) []string {
	ids := make([]string, len(hits))
	for i, hit := range hits {
		ids[i] = hit.Record.SourceID
	}
	return ids
}

type stepCancelContext struct {
	context.Context
	done      chan struct{}
	remaining int
	mu        sync.Mutex
}

func newStepCancelContext(parent context.Context, remaining int) *stepCancelContext {
	return &stepCancelContext{Context: parent, done: make(chan struct{}), remaining: remaining}
}

func (c *stepCancelContext) Done() <-chan struct{} { return c.done }

func (c *stepCancelContext) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.remaining > 0 {
		c.remaining--
		return nil
	}
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return context.Canceled
}
