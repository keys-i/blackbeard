// Package academictorrents reads Academic Torrents' documented offline catalogue.
package academictorrents

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"

	"github.com/keys-i/blackbeard/src/internal/domain"
	"github.com/keys-i/blackbeard/src/internal/provider"
)

const (
	origin              = "https://academictorrents.com"
	cacheFilename       = "academic-torrents.json"
	cacheSchemaVersion  = 1
	maxCatalogBytes     = 8 << 20
	maxMetainfoBytes    = 16 << 20
	maxRecords          = 100_000
	maxTitleBytes       = domain.MaxTitleBytes
	maxCategoryBytes    = 256
	maxURLBytes         = 4 << 10
	maxDescriptionBytes = 64 << 10
)

var (
	errCatalogTooLarge = errors.New("academic torrents catalogue exceeds 8 MiB")
	errTooManyRecords  = errors.New("academic torrents catalogue exceeds 100000 records")
)

// VerifyV1 computes the v1 infohash from a complete metainfo document.
type VerifyV1 func(context.Context, []byte) (string, error)

type fetchFunc func(context.Context, string, provider.FetchOptions) (provider.Response, error)

type cacheStore interface {
	Load() (cacheState, error)
	Save(cacheState) error
}

// Catalog synchronises and resolves Academic Torrents records.
type Catalog struct {
	fetch  fetchFunc
	cache  cacheStore
	verify VerifyV1
}

var (
	_ provider.CatalogSyncer = (*Catalog)(nil)
	_ provider.Resolver      = (*Catalog)(nil)
)

// New constructs the provider. cacheDir must already be a trusted application
// cache directory.
func New(cacheDir string, verify VerifyV1) (*Catalog, error) {
	if cacheDir == "" {
		return nil, errors.New("academic torrents cache directory is empty")
	}
	client, err := provider.NewClient(origin)
	if err != nil {
		return nil, fmt.Errorf("create academic torrents client: %w", err)
	}
	return newCatalog(client.Fetch, diskCache{dir: cacheDir}, verify)
}

func newCatalog(fetch fetchFunc, cache cacheStore, verify VerifyV1) (*Catalog, error) {
	if fetch == nil || cache == nil || verify == nil {
		return nil, errors.New("academic torrents dependencies must not be nil")
	}
	return &Catalog{fetch: fetch, cache: cache, verify: verify}, nil
}

// Sync refreshes the documented database.xml catalogue. A 304 returns the
// validated cached records without rewriting their atomic snapshot.
func (c *Catalog) Sync(ctx context.Context) (provider.CatalogSnapshot, error) {
	if err := requestContext(ctx); err != nil {
		return provider.CatalogSnapshot{}, err
	}
	state, loadErr := c.cache.Load()
	if loadErr != nil {
		state = cacheState{}
	}
	response, err := c.fetch(ctx, "/database.xml", provider.FetchOptions{
		MaxBody:             maxCatalogBytes,
		ETag:                state.ETag,
		LastModified:        state.LastModified,
		ValidateContentType: validateXMLContentType,
	})
	if err != nil {
		if loadErr != nil {
			return provider.CatalogSnapshot{}, fmt.Errorf("load academic torrents cache: %w; refresh: %w", loadErr, err)
		}
		return provider.CatalogSnapshot{}, fmt.Errorf("refresh academic torrents catalogue: %w", err)
	}
	if response.NotModified {
		if loadErr != nil || state.SchemaVersion != cacheSchemaVersion {
			return provider.CatalogSnapshot{}, errors.New("academic torrents returned 304 without a usable cache")
		}
		return provider.CatalogSnapshot{Records: state.Records, NotModified: true}, nil
	}

	items, err := parseCatalog(ctx, bytes.NewReader(response.Body))
	if err != nil {
		return provider.CatalogSnapshot{}, err
	}
	records := make([]domain.Record, 0, len(items))
	for i, item := range items {
		record, err := item.record()
		if err != nil {
			return provider.CatalogSnapshot{}, fmt.Errorf("normalise academic torrents item %d: %w", i+1, err)
		}
		records = append(records, record)
	}
	state = cacheState{
		SchemaVersion: cacheSchemaVersion,
		ETag:          response.ETag,
		LastModified:  response.LastModified,
		Records:       records,
	}
	if err := c.cache.Save(state); err != nil {
		return provider.CatalogSnapshot{}, fmt.Errorf("save academic torrents cache: %w", err)
	}
	return provider.CatalogSnapshot{Records: records}, nil
}

// Resolve downloads metainfo from the fixed Academic Torrents origin and
// rejects it unless the injected independent parser computes the expected hash.
func (c *Catalog) Resolve(ctx context.Context, record domain.Record) (provider.Source, error) {
	if err := requestContext(ctx); err != nil {
		return provider.Source{}, err
	}
	if record.Provider != "academic_torrents" || record.SourceID != record.InfoHashV1 || !validInfoHash(record.InfoHashV1) {
		return provider.Source{}, errors.New("academic torrents record identity is invalid")
	}
	hash := record.InfoHashV1
	torrentURL := origin + "/download/" + hash + ".torrent"
	response, err := c.fetch(ctx, "/download/"+hash+".torrent", provider.FetchOptions{
		MaxBody:             maxMetainfoBytes,
		ValidateContentType: validateTorrentContentType,
	})
	if err != nil {
		return provider.Source{}, fmt.Errorf("resolve academic torrents metainfo: %w", err)
	}
	if response.NotModified || len(response.Body) == 0 || len(response.Body) > maxMetainfoBytes {
		return provider.Source{}, errors.New("academic torrents returned invalid metainfo content")
	}
	computed, err := c.verify(ctx, response.Body)
	if err != nil {
		return provider.Source{}, fmt.Errorf("verify academic torrents metainfo: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return provider.Source{}, err
	}
	if computed != hash {
		return provider.Source{}, fmt.Errorf("academic torrents metainfo infohash mismatch: expected %s, got %s", hash, computed)
	}
	return provider.Source{TorrentURL: torrentURL, Metainfo: response.Body}, nil
}

func requestContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("academic torrents request context is nil")
	}
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("academic torrents request context has no deadline")
	}
	return ctx.Err()
}

func validateXMLContentType(mediaType string) error {
	switch mediaType {
	case "", "application/xml", "text/xml", "application/rss+xml":
		return nil
	default:
		return fmt.Errorf("unexpected Academic Torrents catalogue content type %q", mediaType)
	}
}

func validateTorrentContentType(mediaType string) error {
	switch mediaType {
	case "", "application/x-bittorrent", "application/octet-stream":
		return nil
	default:
		return fmt.Errorf("unexpected Academic Torrents metainfo content type %q", mediaType)
	}
}

type cacheState struct {
	SchemaVersion int             `json:"schema_version"`
	ETag          string          `json:"etag,omitempty"`
	LastModified  string          `json:"last_modified,omitempty"`
	Records       []domain.Record `json:"records"`
}

type diskCache struct{ dir string }

func (c diskCache) Load() (cacheState, error) {
	root, err := os.OpenRoot(c.dir)
	if err != nil {
		return cacheState{}, fmt.Errorf("open cache directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(cacheFilename)
	if errors.Is(err, fs.ErrNotExist) {
		return cacheState{}, nil
	}
	if err != nil {
		return cacheState{}, fmt.Errorf("open cache snapshot: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, provider.MaxResponseBody+1))
	if err != nil {
		return cacheState{}, fmt.Errorf("read cache snapshot: %w", err)
	}
	if int64(len(data)) > provider.MaxResponseBody {
		return cacheState{}, errors.New("academic torrents cache exceeds 32 MiB")
	}
	var state cacheState
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&state); err != nil {
		return cacheState{}, fmt.Errorf("decode cache snapshot: %w", err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return cacheState{}, err
	}
	if state.SchemaVersion != cacheSchemaVersion || len(state.Records) == 0 || len(state.Records) > maxRecords {
		return cacheState{}, errors.New("academic torrents cache schema or record count is invalid")
	}
	for i := range state.Records {
		state.Records[i], err = normalizeCachedRecord(state.Records[i])
		if err != nil {
			return cacheState{}, fmt.Errorf("validate cached record %d: %w", i+1, err)
		}
	}
	return state, nil
}

func normalizeCachedRecord(record domain.Record) (domain.Record, error) {
	if record.Provider != "academic_torrents" || record.SourceID != record.InfoHashV1 || !validInfoHash(record.InfoHashV1) ||
		record.InfoHashV2 != "" || record.SizeBytes == nil || len(record.Categories) > 1 || record.PublishedAt != nil ||
		record.Seeders != nil || record.Leechers != nil || len(record.ContentKinds) != 0 || len(record.MediaKinds) != 0 ||
		len(record.Extensions) != 0 || len(record.Languages) != 0 || len(record.Architectures) != 0 ||
		len(record.Resolutions) != 0 || len(record.Codecs) != 0 {
		return domain.Record{}, errors.New("academic torrents cached record is invalid")
	}
	hash := record.InfoHashV1
	record.DetailsURL = origin + "/details/" + hash
	record.MagnetURI = "magnet:?xt=urn:btih:" + hash
	record.TorrentURL = origin + "/download/" + hash + ".torrent"
	return domain.NormalizeRecord(record)
}

func (c diskCache) Save(state cacheState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode cache snapshot: %w", err)
	}
	if len(data)+1 > int(provider.MaxResponseBody) {
		return errors.New("academic torrents cache exceeds 32 MiB")
	}
	data = append(data, '\n')
	return provider.ReplaceFile(c.dir, cacheFilename, data, 0o600)
}

func ensureJSONEOF(dec *json.Decoder) error {
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("academic torrents cache contains trailing JSON")
		}
		return fmt.Errorf("decode trailing cache data: %w", err)
	}
	return nil
}

type catalogItem struct {
	Title       string
	Category    string
	InfoHash    string
	Description string
	Size        int64
}

func (item catalogItem) record() (domain.Record, error) {
	hash := item.InfoHash
	size := item.Size
	record := domain.Record{
		Provider:    "academic_torrents",
		SourceID:    hash,
		Title:       item.Title,
		Description: item.Description,
		InfoHashV1:  hash,
		DetailsURL:  origin + "/details/" + hash,
		MagnetURI:   "magnet:?xt=urn:btih:" + hash,
		TorrentURL:  origin + "/download/" + hash + ".torrent",
		SizeBytes:   &size,
	}
	if validFacet(item.Category) {
		record.Categories = []string{item.Category}
	}
	return domain.NormalizeRecord(record)
}

func validFacet(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > domain.MaxFacetBytes {
		return false
	}
	for _, c := range value {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func parseCatalog(ctx context.Context, r io.Reader) ([]catalogItem, error) {
	return parseCatalogLimits(ctx, r, maxCatalogBytes, maxRecords)
}

func parseCatalogLimits(ctx context.Context, r io.Reader, maxBytes int64, recordLimit int) ([]catalogItem, error) {
	limited := &countingReader{r: io.LimitReader(&contextReader{ctx: ctx, r: r}, maxBytes+1)}
	dec := xml.NewDecoder(limited)

	var (
		items       []catalogItem
		stack       []xml.Name
		seenRSS     bool
		seenChannel bool
		seenHashes  = make(map[string]struct{})
		recordCount int
		done        bool
	)
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			if limited.n > maxBytes {
				return nil, errCatalogTooLarge
			}
			if !done || !seenChannel || recordCount == 0 {
				return nil, errors.New("academic torrents catalogue is incomplete")
			}
			return items, nil
		}
		if err != nil {
			if limited.n > maxBytes {
				return nil, errCatalogTooLarge
			}
			return nil, fmt.Errorf("parse academic torrents catalogue: %w", err)
		}

		switch tok := tok.(type) {
		case xml.Directive:
			return nil, errors.New("academic torrents catalogue contains a forbidden XML directive")
		case xml.ProcInst:
			if seenRSS {
				return nil, errors.New("academic torrents catalogue contains a forbidden processing instruction")
			}
			if !strings.EqualFold(tok.Target, "xml") {
				return nil, errors.New("academic torrents catalogue contains a forbidden processing instruction")
			}
		case xml.CharData:
			if strings.TrimSpace(string(tok)) != "" {
				return nil, errors.New("academic torrents catalogue contains text outside an element")
			}
		case xml.StartElement:
			if done || tok.Name.Space != "" {
				return nil, fmt.Errorf("unexpected XML element %q", tok.Name.Local)
			}
			switch len(stack) {
			case 0:
				if seenRSS || tok.Name.Local != "rss" {
					return nil, errors.New("academic torrents catalogue root must be rss")
				}
				seenRSS = true
				stack = append(stack, tok.Name)
			case 1:
				if seenChannel || tok.Name.Local != "channel" {
					return nil, errors.New("academic torrents catalogue must contain one channel")
				}
				seenChannel = true
				stack = append(stack, tok.Name)
			case 2:
				switch tok.Name.Local {
				case "item":
					recordCount++
					if recordCount > recordLimit {
						return nil, errTooManyRecords
					}
					item, err := parseItem(dec, tok)
					if err != nil {
						return nil, fmt.Errorf("academic torrents item %d: %w", recordCount, err)
					}
					if _, duplicate := seenHashes[item.InfoHash]; !duplicate {
						seenHashes[item.InfoHash] = struct{}{}
						items = append(items, item)
					}
				case "title", "description", "link":
					if _, err := readText(dec, tok, maxDescriptionBytes); err != nil {
						return nil, fmt.Errorf("academic torrents channel %s: %w", tok.Name.Local, err)
					}
				default:
					return nil, fmt.Errorf("unexpected academic torrents channel element %q", tok.Name.Local)
				}
			default:
				return nil, fmt.Errorf("unexpected nested XML element %q", tok.Name.Local)
			}
		case xml.EndElement:
			if len(stack) == 0 || stack[len(stack)-1] != tok.Name {
				return nil, fmt.Errorf("unexpected XML closing element %q", tok.Name.Local)
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				done = true
			}
		}
	}
}

func parseItem(dec *xml.Decoder, start xml.StartElement) (catalogItem, error) {
	var item catalogItem
	var seen uint8
	for {
		tok, err := dec.Token()
		if err != nil {
			return item, err
		}
		switch tok := tok.(type) {
		case xml.Directive, xml.ProcInst:
			return item, errors.New("forbidden XML instruction")
		case xml.CharData:
			if strings.TrimSpace(string(tok)) != "" {
				return item, errors.New("text outside an item field")
			}
		case xml.StartElement:
			limit, bit, ok := itemField(tok.Name.Local)
			if !ok {
				return item, fmt.Errorf("unexpected field %q", tok.Name.Local)
			}
			if tok.Name.Space != "" || seen&bit != 0 {
				return item, fmt.Errorf("unexpected or duplicate field %q", tok.Name.Local)
			}
			value, err := readText(dec, tok, limit)
			if err != nil {
				return item, fmt.Errorf("field %s: %w", tok.Name.Local, err)
			}
			seen |= bit
			switch tok.Name.Local {
			case "title":
				item.Title = strings.TrimSpace(value)
			case "category":
				item.Category = strings.TrimSpace(value)
			case "infohash":
				item.InfoHash = strings.TrimSpace(value)
			case "description":
				item.Description = strings.TrimSpace(value)
			case "size":
				item.Size, err = parseSize(strings.TrimSpace(value))
				if err != nil {
					return item, err
				}
			}
		case xml.EndElement:
			if tok.Name != start.Name {
				return item, fmt.Errorf("unexpected closing field %q", tok.Name.Local)
			}
			for i, name := range []string{"title", "category", "infohash", "guid", "link", "description", "size"} {
				if seen&(1<<i) == 0 {
					return item, fmt.Errorf("missing field %q", name)
				}
			}
			if item.Title == "" {
				return item, errors.New("title is empty")
			}
			if !validInfoHash(item.InfoHash) {
				return item, errors.New("infohash must be 40 lowercase hexadecimal characters")
			}
			return item, nil
		}
	}
}

func itemField(name string) (int, uint8, bool) {
	switch name {
	case "title":
		return maxTitleBytes, 1 << 0, true
	case "category":
		return maxCategoryBytes, 1 << 1, true
	case "infohash":
		return 40, 1 << 2, true
	case "guid":
		return maxURLBytes, 1 << 3, true
	case "link":
		return maxURLBytes, 1 << 4, true
	case "description":
		return maxDescriptionBytes, 1 << 5, true
	case "size":
		return 40, 1 << 6, true
	default:
		return 0, 0, false
	}
}

func readText(dec *xml.Decoder, start xml.StartElement, limit int) (string, error) {
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch tok := tok.(type) {
		case xml.CharData:
			if b.Len()+len(tok) > limit {
				return "", fmt.Errorf("value exceeds %d bytes", limit)
			}
			b.Write(tok)
		case xml.Comment:
		case xml.EndElement:
			if tok.Name != start.Name {
				return "", fmt.Errorf("unexpected closing element %q", tok.Name.Local)
			}
			return b.String(), nil
		default:
			return "", errors.New("nested markup or XML instructions are not allowed")
		}
	}
}

func parseSize(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("size is empty")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("size must be a nonnegative base-10 integer")
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, errors.New("size exceeds int64")
	}
	return n, nil
}

func validInfoHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

type countingReader struct {
	r io.Reader
	n int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.n += int64(n)
	return n, err
}
