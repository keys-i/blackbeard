package academictorrents

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/keys-i/blackbeard/src/internal/domain"
	"github.com/keys-i/blackbeard/src/internal/provider"
)

const testHash = "0123456789abcdef0123456789abcdef01234567"

func TestParseCatalog(t *testing.T) {
	items, err := parseCatalog(context.Background(), strings.NewReader(catalogXML(itemXML("First", testHash, "42"))))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "First" || items[0].Category != "Dataset" || items[0].Size != 42 {
		t.Fatalf("unexpected items: %#v", items)
	}
}

func TestParseCatalogRejectsHostileInput(t *testing.T) {
	tests := map[string]string{
		"malformed":       `<rss><channel>`,
		"invalid utf8":    catalogXML(strings.Replace(itemXML("First", testHash, "42"), "First", string([]byte{0xff}), 1)),
		"uppercase hash":  catalogXML(itemXML("First", strings.ToUpper(testHash), "42")),
		"short hash":      catalogXML(itemXML("First", "abcd", "42")),
		"negative size":   catalogXML(itemXML("First", testHash, "-1")),
		"signed size":     catalogXML(itemXML("First", testHash, "+1")),
		"overflow size":   catalogXML(itemXML("First", testHash, "9223372036854775808")),
		"missing field":   catalogXML(strings.Replace(itemXML("First", testHash, "42"), "<category>Dataset</category>", "", 1)),
		"duplicate field": catalogXML(strings.Replace(itemXML("First", testHash, "42"), "<category>Dataset</category>", "<category>Dataset</category><category>Paper</category>", 1)),
		"unknown field":   catalogXML(strings.Replace(itemXML("First", testHash, "42"), "</item>", "<author>unknown</author></item>", 1)),
		"nested markup":   catalogXML(strings.Replace(itemXML("First", testHash, "42"), "First", "<b>First</b>", 1)),
		"doctype":         `<!DOCTYPE rss [<!ENTITY x "boom">]>` + catalogXML(itemXML("First", testHash, "42")),
		"trailing xml":    catalogXML(itemXML("First", testHash, "42")) + `<rss/>`,
		"empty root":      `<rss/>`,
		"empty channel":   `<rss><channel/></rss>`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := parseCatalog(context.Background(), strings.NewReader(input)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestParseCatalogDuplicateFirstWins(t *testing.T) {
	input := catalogXML(itemXML("First", testHash, "1") + itemXML("Second", testHash, "2"))
	items, err := parseCatalog(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "First" || items[0].Size != 1 {
		t.Fatalf("duplicate policy changed: %#v", items)
	}
}

func TestParseCatalogLimits(t *testing.T) {
	input := catalogXML(itemXML("First", testHash, "1"))
	if _, err := parseCatalogLimits(context.Background(), strings.NewReader(input), 32, maxRecords); !errors.Is(err, errCatalogTooLarge) {
		t.Fatalf("body cap error = %v", err)
	}

	secondHash := "1123456789abcdef0123456789abcdef01234567"
	input = catalogXML(itemXML("First", testHash, "1") + itemXML("Second", secondHash, "2"))
	if _, err := parseCatalogLimits(context.Background(), strings.NewReader(input), maxCatalogBytes, 1); !errors.Is(err, errTooManyRecords) {
		t.Fatalf("record cap error = %v", err)
	}

	longTitle := strings.Repeat("x", maxTitleBytes+1)
	if _, err := parseCatalog(context.Background(), strings.NewReader(catalogXML(itemXML(longTitle, testHash, "1")))); err == nil {
		t.Fatal("expected field cap rejection")
	}
}

func TestParseCatalogCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := parseCatalog(ctx, strings.NewReader(catalogXML(itemXML("First", testHash, "1"))))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestSyncPersistsNormalizedRecordsAndValidators(t *testing.T) {
	cache := &memoryCache{state: cacheState{SchemaVersion: cacheSchemaVersion, ETag: `"old"`, LastModified: "Wed, 21 Oct 2015 07:28:00 GMT"}}
	payload := []byte(catalogXML(itemXML("Open\nDataset", testHash, "42")))
	fetch := func(_ context.Context, path string, opts provider.FetchOptions) (provider.Response, error) {
		if path != "/database.xml" || opts.MaxBody != maxCatalogBytes || opts.ETag != `"old"` || opts.LastModified != "Wed, 21 Oct 2015 07:28:00 GMT" {
			t.Fatalf("unexpected fetch: path=%q opts=%+v", path, opts)
		}
		if err := opts.ValidateContentType("application/xml"); err != nil {
			t.Fatal(err)
		}
		return provider.Response{Body: payload, ETag: `"new"`, LastModified: "Thu, 22 Oct 2015 07:28:00 GMT"}, nil
	}
	catalog, err := newCatalog(fetch, cache, func(context.Context, []byte) (string, error) { return testHash, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := catalog.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.NotModified || len(snapshot.Records) != 1 || cache.saves != 1 {
		t.Fatalf("snapshot=%+v saves=%d", snapshot, cache.saves)
	}
	record := snapshot.Records[0]
	if record.Provider != "academic_torrents" || record.SourceID != testHash || record.Title != "Open Dataset" || record.InfoHashV1 != testHash {
		t.Fatalf("identity = %+v", record)
	}
	if record.DetailsURL != origin+"/details/"+testHash || record.TorrentURL != origin+"/download/"+testHash+".torrent" || record.MagnetURI != "magnet:?xt=urn:btih:"+testHash {
		t.Fatalf("derived sources = %+v", record)
	}
	if record.SizeBytes == nil || *record.SizeBytes != 42 || record.PublishedAt != nil || record.Seeders != nil || record.Leechers != nil {
		t.Fatalf("known/unknown metadata = %+v", record)
	}
	if len(record.Categories) != 1 || record.Categories[0] != "dataset" || cache.state.ETag != `"new"` {
		t.Fatalf("category/cache = %#v / %+v", record.Categories, cache.state)
	}
}

func TestSync304DoesNotRewriteCache(t *testing.T) {
	record, err := (catalogItem{Title: "Cached", Category: "Dataset", InfoHash: testHash, Size: 1}).record()
	if err != nil {
		t.Fatal(err)
	}
	cache := &memoryCache{state: cacheState{SchemaVersion: cacheSchemaVersion, ETag: `"same"`, Records: []domain.Record{record}}}
	fetch := func(_ context.Context, _ string, _ provider.FetchOptions) (provider.Response, error) {
		return provider.Response{NotModified: true}, nil
	}
	catalog, err := newCatalog(fetch, cache, func(context.Context, []byte) (string, error) { return testHash, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	snapshot, err := catalog.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.NotModified || len(snapshot.Records) != 1 || cache.saves != 0 {
		t.Fatalf("snapshot=%+v saves=%d", snapshot, cache.saves)
	}
}

func TestSyncDeadlineAfterCacheSaveIsNotReportedAsSuccess(t *testing.T) {
	fetch := func(context.Context, string, provider.FetchOptions) (provider.Response, error) {
		return provider.Response{Body: []byte(catalogXML(itemXML("Dataset", testHash, "42")))}, nil
	}
	catalog, err := newCatalog(fetch, blockingSaveCache{}, func(context.Context, []byte) (string, error) { return testHash, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := catalog.Sync(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Sync() error = %v", err)
	}
}

func TestNewSyncerDoesNotAdvertiseResolverCapability(t *testing.T) {
	syncer, err := NewSyncer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := syncer.(provider.Resolver); ok {
		t.Fatal("sync-only provider advertises resolver capability")
	}
}

func TestResolveVerifiesDownloadedMetainfo(t *testing.T) {
	body := []byte("torrent metainfo")
	fetches := 0
	fetch := func(_ context.Context, path string, opts provider.FetchOptions) (provider.Response, error) {
		fetches++
		if path != "/download/"+testHash+".torrent" || opts.MaxBody != maxMetainfoBytes {
			t.Fatalf("unexpected fetch: path=%q opts=%+v", path, opts)
		}
		if err := opts.ValidateContentType("application/x-bittorrent"); err != nil {
			t.Fatal(err)
		}
		return provider.Response{Body: body}, nil
	}
	cache := &memoryCache{}
	catalog, err := newCatalog(fetch, cache, func(_ context.Context, got []byte) (string, error) {
		if !bytes.Equal(got, body) {
			t.Fatalf("verifier got %q", got)
		}
		return testHash, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	record := domain.Record{Provider: "academic_torrents", SourceID: testHash, InfoHashV1: testHash}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	source, err := catalog.Resolve(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	if fetches != 1 || source.MagnetURI != "" || source.TorrentURL != origin+"/download/"+testHash+".torrent" || !bytes.Equal(source.Metainfo, body) {
		t.Fatalf("source=%+v fetches=%d", source, fetches)
	}
}

func TestResolveRejectsMismatchAndBounds(t *testing.T) {
	record := domain.Record{Provider: "academic_torrents", SourceID: testHash, InfoHashV1: testHash}
	tests := []struct {
		name   string
		body   []byte
		verify VerifyV1
	}{
		{"mismatch", []byte("torrent"), func(context.Context, []byte) (string, error) { return "1123456789abcdef0123456789abcdef01234567", nil }},
		{"oversize", make([]byte, maxMetainfoBytes+1), func(context.Context, []byte) (string, error) { return testHash, nil }},
		{"empty", nil, func(context.Context, []byte) (string, error) { return testHash, nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fetch := func(context.Context, string, provider.FetchOptions) (provider.Response, error) {
				return provider.Response{Body: test.body}, nil
			}
			catalog, err := newCatalog(fetch, &memoryCache{}, test.verify)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := catalog.Resolve(ctx, record); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestResolveRejectsUntrustedRecordBeforeFetch(t *testing.T) {
	called := false
	fetch := func(context.Context, string, provider.FetchOptions) (provider.Response, error) {
		called = true
		return provider.Response{}, nil
	}
	catalog, err := newCatalog(fetch, &memoryCache{}, func(context.Context, []byte) (string, error) { return testHash, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = catalog.Resolve(ctx, domain.Record{Provider: "academic_torrents", SourceID: "other", InfoHashV1: testHash, TorrentURL: "https://evil.example/x.torrent"})
	if err == nil || called {
		t.Fatalf("error=%v fetch called=%v", err, called)
	}
}

func TestResolveIgnoresProviderSuppliedTorrentURL(t *testing.T) {
	fetch := func(_ context.Context, path string, _ provider.FetchOptions) (provider.Response, error) {
		if path != "/download/"+testHash+".torrent" {
			t.Fatalf("fetch path = %q", path)
		}
		return provider.Response{Body: []byte("torrent")}, nil
	}
	catalog, err := newCatalog(fetch, &memoryCache{}, func(context.Context, []byte) (string, error) { return testHash, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	source, err := catalog.Resolve(ctx, domain.Record{
		Provider: "academic_torrents", SourceID: testHash, InfoHashV1: testHash,
		TorrentURL: "https://evil.example/payload.torrent",
	})
	if err != nil || source.TorrentURL != origin+"/download/"+testHash+".torrent" {
		t.Fatalf("source=%+v error=%v", source, err)
	}
}

func TestSyncOnlyCatalogCannotResolve(t *testing.T) {
	catalog, err := newSyncCatalog(func(context.Context, string, provider.FetchOptions) (provider.Response, error) {
		t.Fatal("resolver fetched without a verifier")
		return provider.Response{}, nil
	}, &memoryCache{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = catalog.Resolve(ctx, domain.Record{Provider: "academic_torrents", SourceID: testHash, InfoHashV1: testHash})
	if err == nil || !strings.Contains(err.Error(), "without a verifier") {
		t.Fatalf("Resolve() error = %v", err)
	}
}

func TestResolveCancelsMetainfoVerification(t *testing.T) {
	fetch := func(context.Context, string, provider.FetchOptions) (provider.Response, error) {
		return provider.Response{Body: []byte("torrent")}, nil
	}
	catalog, err := newCatalog(fetch, &memoryCache{}, func(ctx context.Context, _ []byte) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = catalog.Resolve(ctx, domain.Record{Provider: "academic_torrents", SourceID: testHash, InfoHashV1: testHash})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("verification cancellation error = %v", err)
	}
}

func TestDiskCacheRoundTripAndTrailingData(t *testing.T) {
	dir := t.TempDir()
	record, err := (catalogItem{Title: "Cached", InfoHash: testHash, Size: 9}).record()
	if err != nil {
		t.Fatal(err)
	}
	store := diskCache{dir: dir}
	want := cacheState{SchemaVersion: cacheSchemaVersion, ETag: `"tag"`, Records: []domain.Record{record}}
	if err := store.Save(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(context.Background())
	if err != nil || got.SchemaVersion != cacheSchemaVersion || got.ETag != want.ETag || len(got.Records) != 1 {
		t.Fatalf("state=%+v error=%v", got, err)
	}
	path := filepath.Join(dir, cacheFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte(`{}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("expected trailing JSON rejection")
	}
}

func TestDiskCacheRejectsForeignIdentity(t *testing.T) {
	dir := t.TempDir()
	record, err := (catalogItem{Title: "Cached", InfoHash: testHash, Size: 9}).record()
	if err != nil {
		t.Fatal(err)
	}
	record.Provider = "internet_archive"
	store := diskCache{dir: dir}
	if err := store.Save(context.Background(), cacheState{SchemaVersion: cacheSchemaVersion, Records: []domain.Record{record}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("foreign cached identity was accepted")
	}
}

func TestDiskCacheRejectsEmptyAndInjectedMetadata(t *testing.T) {
	dir := t.TempDir()
	store := diskCache{dir: dir}
	if err := store.Save(context.Background(), cacheState{SchemaVersion: cacheSchemaVersion}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("empty cache was accepted")
	}
	record, err := (catalogItem{Title: "Cached", InfoHash: testHash, Size: 9}).record()
	if err != nil {
		t.Fatal(err)
	}
	record.ContentKinds = []string{"dataset"}
	if err := store.Save(context.Background(), cacheState{SchemaVersion: cacheSchemaVersion, Records: []domain.Record{record}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("cache metadata not supplied by the provider was accepted")
	}
}

func TestDiskCacheDoesNotFollowSnapshotSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, cacheFilename)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store := diskCache{dir: dir}
	if _, err := store.Load(context.Background()); err == nil {
		t.Fatal("cache followed a snapshot symlink")
	}
	record, err := (catalogItem{Title: "Cached", InfoHash: testHash, Size: 9}).record()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), cacheState{SchemaVersion: cacheSchemaVersion, Records: []domain.Record{record}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != "sentinel" {
		t.Fatalf("outside file = %q, %v", data, err)
	}
}

func TestDiskCacheHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := diskCache{dir: t.TempDir()}
	if _, err := store.Load(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load() error = %v", err)
	}
	if err := store.Save(ctx, cacheState{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Save() error = %v", err)
	}
}

func TestContentTypes(t *testing.T) {
	for _, mediaType := range []string{"", "application/xml", "text/xml", "application/rss+xml"} {
		if err := validateXMLContentType(mediaType); err != nil {
			t.Errorf("XML %q: %v", mediaType, err)
		}
	}
	for _, mediaType := range []string{"", "application/x-bittorrent", "application/octet-stream"} {
		if err := validateTorrentContentType(mediaType); err != nil {
			t.Errorf("torrent %q: %v", mediaType, err)
		}
	}
	if validateXMLContentType("text/html") == nil || validateTorrentContentType("text/plain") == nil {
		t.Fatal("unexpected content type accepted")
	}
}

func FuzzParseCatalog(f *testing.F) {
	f.Add([]byte(catalogXML(itemXML("First", testHash, "42"))))
	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = parseCatalog(context.Background(), bytes.NewReader(input))
	})
}

func BenchmarkParseCatalog(b *testing.B) {
	var items strings.Builder
	for i := 0; i < 2_850; i++ {
		hash := fmt.Sprintf("%040x", i+1)
		items.WriteString(itemXML("Dataset", hash, "1048576"))
	}
	payload := []byte(catalogXML(items.String()))
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseCatalog(context.Background(), bytes.NewReader(payload)); err != nil {
			b.Fatal(err)
		}
	}
}

func catalogXML(items string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><rss xmlns:academictorrents="http://academictorrents.com" version="2.0"><channel><title>Academic Torrents</title><description>All Torrents</description><link>http://academictorrents.com/</link>` + items + `</channel></rss>`
}

func itemXML(title, hash, size string) string {
	return `<item><title>` + title + `</title><category>Dataset</category><infohash>` + hash + `</infohash><guid>http://academictorrents.com/details/` + hash + `</guid><link>http://academictorrents.com/details/` + hash + `</link><description>Open research data</description><size>` + size + `</size></item>`
}

type memoryCache struct {
	state   cacheState
	loadErr error
	saveErr error
	saves   int
}

type blockingSaveCache struct{}

func (blockingSaveCache) Load(context.Context) (cacheState, error) { return cacheState{}, nil }
func (blockingSaveCache) Save(ctx context.Context, _ cacheState) error {
	<-ctx.Done()
	return nil
}

func (c *memoryCache) Load(context.Context) (cacheState, error) { return c.state, c.loadErr }

func (c *memoryCache) Save(_ context.Context, state cacheState) error {
	c.saves++
	c.state = state
	return c.saveErr
}
