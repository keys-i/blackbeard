// Package index owns Blackbeard's persistent offline catalogue search.
package index

import (
	"bytes"
	"container/heap"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/analysis/analyzer/custom"
	"github.com/blevesearch/bleve/v2/analysis/token/lowercase"
	"github.com/blevesearch/bleve/v2/analysis/tokenizer/unicode"
	"github.com/blevesearch/bleve/v2/mapping"
	blevequery "github.com/blevesearch/bleve/v2/search/query"
	"github.com/keys-i/blackbeard/src/internal/domain"
	productquery "github.com/keys-i/blackbeard/src/internal/query"
)

const (
	DefaultLimit     = 50
	MaxRecords       = 1_000_000
	maxPreparedBytes = 256 << 20
	maxGroupBytes    = 16 << 20
	maxGroupSources  = 4096
	recordOverhead   = 1024
	batchSize        = 512
	searchPageSize   = 64
	textAnalyzerName = "blackbeard_text"
)

var (
	ErrClosed         = errors.New("catalogue index is closed")
	ErrInvalidQuery   = errors.New("invalid catalogue query")
	ErrSourceConflict = errors.New("catalogue source has conflicting infohashes")
)

// Hit is a deduplicated catalogue result. ID remains stable while its
// infohash/provider identity remains stable.
type Hit struct {
	ID      string          `json:"id"`
	Score   float64         `json:"score"`
	Record  domain.Record   `json:"record"`
	Sources []domain.Record `json:"sources"`

	rankSources []domain.Record
}

// Store is one on-disk Bleve catalogue. Rebuilds are serialized while reads
// continue against the previous generation until the swap.
type Store struct {
	path    string
	buildMu sync.Mutex
	mu      sync.RWMutex
	index   bleve.Index
	closed  bool
}

type document struct {
	Title        []string `json:"title"`
	Structured   []string `json:"structured"`
	Description  []string `json:"description"`
	Provider     string   `json:"provider"`
	Group        string   `json:"group"`
	Sources      string   `json:"sources"`
	Completeness int      `json:"completeness"`
	SourceCount  int      `json:"source_count"`
}

type prepared struct {
	id           string
	group        string
	provider     string
	title        []string
	structured   []string
	description  []string
	encoded      string
	completeness int
	sourceCount  int
}

type sourceItem struct {
	key     string
	record  domain.Record
	encoded []byte
}

type rankedHit struct {
	hit   Hit
	index int
}

type hitHeap struct {
	items []*rankedHit
	order []productquery.OrderClause
}

func (h hitHeap) Len() int { return len(h.items) }
func (h hitHeap) Less(i, j int) bool {
	return compareHits(h.items[i].hit, h.items[j].hit, h.order) > 0
}
func (h hitHeap) Swap(i, j int) {
	h.items[i], h.items[j] = h.items[j], h.items[i]
	h.items[i].index, h.items[j].index = i, j
}
func (h *hitHeap) Push(value any) {
	item := value.(*rankedHit)
	item.index = len(h.items)
	h.items = append(h.items, item)
}
func (h *hitHeap) Pop() any {
	last := len(h.items) - 1
	item := h.items[last]
	h.items[last] = nil
	h.items = h.items[:last]
	item.index = -1
	return item
}

type hitCollector struct {
	limit   int
	heap    hitHeap
	byGroup map[string]*rankedHit
}

func newHitCollector(limit int, order []productquery.OrderClause) *hitCollector {
	return &hitCollector{limit: limit, heap: hitHeap{order: order}, byGroup: make(map[string]*rankedHit, limit)}
}

func (c *hitCollector) add(candidate Hit) {
	if existing, ok := c.byGroup[candidate.ID]; ok {
		if compareHits(candidate, existing.hit, c.heap.order) < 0 {
			existing.hit = candidate
			heap.Fix(&c.heap, existing.index)
		}
		return
	}
	if c.heap.Len() == c.limit && compareHits(candidate, c.heap.items[0].hit, c.heap.order) >= 0 {
		return
	}
	if c.heap.Len() == c.limit {
		removed := heap.Pop(&c.heap).(*rankedHit)
		delete(c.byGroup, removed.hit.ID)
	}
	item := &rankedHit{hit: candidate}
	heap.Push(&c.heap, item)
	c.byGroup[candidate.ID] = item
}

func (c *hitCollector) results() []Hit {
	results := make([]Hit, len(c.heap.items))
	for i, item := range c.heap.items {
		results[i] = item.hit
	}
	slices.SortFunc(results, func(a, b Hit) int { return compareHits(a, b, c.heap.order) })
	return results
}

// Open opens an existing index or creates an empty one at path.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("open catalogue index: empty path")
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, errors.New("open catalogue index: path must be absolute")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create catalogue index parent: %w", err)
	}
	// ponytail: locking is process-local; add a lockfile only when concurrent
	// Blackbeard processes need to rebuild the same catalogue.
	previous := path + ".previous"
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if _, previousErr := os.Stat(previous); previousErr == nil {
			if err := os.Rename(previous, path); err != nil {
				return nil, fmt.Errorf("recover previous catalogue index: %w", err)
			}
		}
	}
	_, statErr := os.Stat(path)
	liveExists := statErr == nil
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect catalogue index: %w", statErr)
	}

	idx, err := bleve.Open(path)
	if err != nil && (liveExists || !errors.Is(err, bleve.ErrorIndexPathDoesNotExist)) {
		original := fmt.Errorf("open live catalogue index: %w", err)
		if recoveryErr := recoverPrevious(path); recoveryErr != nil {
			return nil, errors.Join(original, recoveryErr)
		}
		idx, err = bleve.Open(path)
		if err != nil {
			return nil, errors.Join(original, fmt.Errorf("open recovered catalogue index: %w", err))
		}
	}
	if errors.Is(err, bleve.ErrorIndexPathDoesNotExist) {
		mapping, mappingErr := indexMapping()
		if mappingErr != nil {
			return nil, mappingErr
		}
		idx, err = bleve.New(path, mapping)
	}
	if err != nil {
		return nil, fmt.Errorf("open catalogue index: %w", err)
	}
	return &Store{path: path, index: idx}, nil
}

func recoverPrevious(path string) error {
	previous := path + ".previous"
	if _, err := os.Stat(previous); err != nil {
		return fmt.Errorf("recover previous catalogue index: %w", err)
	}
	failed := path + ".failed"
	if err := os.RemoveAll(failed); err != nil {
		return fmt.Errorf("remove stale failed catalogue index: %w", err)
	}
	if err := os.Rename(path, failed); err != nil {
		return fmt.Errorf("quarantine failed catalogue index: %w", err)
	}
	if err := os.Rename(previous, path); err != nil {
		rollbackErr := os.Rename(failed, path)
		return errors.Join(fmt.Errorf("restore previous catalogue index: %w", err), wrapError("restore failed live catalogue index", rollbackErr))
	}
	return nil
}

// Rebuild validates and indexes records into a new generation, then swaps it
// in only after the build has completed successfully.
func (s *Store) Rebuild(ctx context.Context, records []domain.Record) error {
	if ctx == nil {
		return errors.New("rebuild catalogue index: nil context")
	}
	if len(records) > MaxRecords {
		return fmt.Errorf("rebuild catalogue index: %d records exceeds %d", len(records), MaxRecords)
	}
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	documents, err := prepare(ctx, records)
	if err != nil {
		return err
	}
	mapping, err := indexMapping()
	if err != nil {
		return err
	}
	buildDirectory := s.path + ".building"
	if err := os.RemoveAll(buildDirectory); err != nil {
		return fmt.Errorf("remove stale catalogue index build: %w", err)
	}
	if err := os.MkdirAll(buildDirectory, 0o700); err != nil {
		return fmt.Errorf("create catalogue index build directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(buildDirectory) }()
	temporary := filepath.Join(buildDirectory, "catalogue.bleve")
	building, err := bleve.New(temporary, mapping)
	if err != nil {
		return fmt.Errorf("create catalogue index: %w", err)
	}
	if err := indexDocuments(ctx, building, documents); err != nil {
		_ = building.Close()
		return err
	}
	if err := building.Close(); err != nil {
		return fmt.Errorf("close rebuilt catalogue index: %w", err)
	}
	if err := verifyGeneration(temporary, uint64(len(documents))); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.replace(temporary)
}

func verifyGeneration(path string, expected uint64) error {
	idx, err := bleve.Open(path)
	if err != nil {
		return fmt.Errorf("verify rebuilt catalogue index: %w", err)
	}
	count, countErr := idx.DocCount()
	closeErr := idx.Close()
	if countErr != nil {
		return errors.Join(fmt.Errorf("count rebuilt catalogue documents: %w", countErr), wrapError("close rebuilt catalogue verification", closeErr))
	}
	if closeErr != nil {
		return fmt.Errorf("close rebuilt catalogue verification: %w", closeErr)
	}
	if count != expected {
		return fmt.Errorf("verify rebuilt catalogue index: got %d documents, want %d", count, expected)
	}
	return nil
}

// Search evaluates lexical terms with exact structured filters, then performs
// transitive infohash deduplication through precomputed group IDs.
func (s *Store) Search(ctx context.Context, ast productquery.AST, limit int) ([]Hit, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidQuery)
	}
	if err := validateQuery(ast, limit); err != nil {
		return nil, err
	}
	bq := buildQuery(ast)
	explicitOrder := hasNonRelevanceOrder(ast.Order)

	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, ErrClosed
	}

	collector := newHitCollector(limit, ast.Order)
	request := bleve.NewSearchRequestOptions(bq, searchPageSize, 0, false)
	request.Fields = []string{"group", "sources"}
	request.SortBy([]string{"-_score", "-completeness", "-source_count", "group"})
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := s.index.SearchInContext(ctx, request)
		if err != nil {
			return nil, fmt.Errorf("search catalogue index: %w", err)
		}
		for _, match := range page.Hits {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			group, ok := match.Fields["group"].(string)
			if !ok || group == "" {
				return nil, errors.New("search catalogue index: corrupt group field")
			}
			encoded, ok := match.Fields["sources"].(string)
			if !ok {
				return nil, errors.New("search catalogue index: corrupt sources field")
			}
			var sources []domain.Record
			if err := json.Unmarshal([]byte(encoded), &sources); err != nil {
				return nil, fmt.Errorf("search catalogue index: decode sources: %w", err)
			}
			if len(sources) == 0 {
				return nil, errors.New("search catalogue index: empty sources field")
			}
			eligible := eligibleSources(sources, ast.Providers)
			if len(eligible) == 0 || !matchesGroup(eligible, ast) {
				continue
			}
			candidate := Hit{ID: group, Score: match.Score, Record: representative(eligible, ast.Order), Sources: sources, rankSources: eligible}
			collector.add(candidate)
			if !explicitOrder && len(ast.Providers.Allow) == 0 && len(ast.Providers.Deny) == 0 && collector.heap.Len() == limit {
				return collector.results(), nil
			}
		}
		if len(page.Hits) < searchPageSize {
			break
		}
		// Bleve v2.6 emits the literal "_score" as its score search-after
		// key, so deep score-sorted paging cannot advance reliably.
		request.From += searchPageSize
	}

	return collector.results(), nil
}

func eligibleSources(sources []domain.Record, selection productquery.ProviderSelection) []domain.Record {
	if len(selection.Allow) == 0 && len(selection.Deny) == 0 {
		return sources
	}
	eligible := make([]domain.Record, 0, len(sources))
	for _, source := range sources {
		if providerAllowed(source.Provider, selection) {
			eligible = append(eligible, source)
		}
	}
	return eligible
}

func representative(sources []domain.Record, order []productquery.OrderClause) domain.Record {
	best := sources[0]
	explicitOrder := hasNonRelevanceOrder(order)
	for _, candidate := range sources[1:] {
		comparison := compareHits(Hit{Record: candidate}, Hit{Record: best}, order)
		if (!explicitOrder && sourceLess(candidate, best)) ||
			(explicitOrder && (comparison < 0 || comparison == 0 && sourceLess(candidate, best))) {
			best = candidate
		}
	}
	return best
}

// Close flushes and closes the catalogue.
func (s *Store) Close() error {
	s.buildMu.Lock()
	defer s.buildMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := s.index.Close(); err != nil {
		return fmt.Errorf("close catalogue index: %w", err)
	}
	return nil
}

func (s *Store) replace(temporary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	previous := s.path + ".previous"
	if err := os.RemoveAll(previous); err != nil {
		return fmt.Errorf("remove stale catalogue index backup: %w", err)
	}
	if err := s.index.Close(); err != nil {
		return errors.Join(fmt.Errorf("close previous catalogue index: %w", err), s.reopenCurrent())
	}
	if err := os.Rename(s.path, previous); err != nil {
		return errors.Join(fmt.Errorf("backup previous catalogue index: %w", err), s.reopenCurrent())
	}
	if err := os.Rename(temporary, s.path); err != nil {
		rollbackErr := os.Rename(previous, s.path)
		return errors.Join(
			fmt.Errorf("activate rebuilt catalogue index: %w", err),
			wrapError("restore previous catalogue index", rollbackErr),
			s.reopenCurrent(),
		)
	}
	idx, err := bleve.Open(s.path)
	if err != nil {
		recoveryErr := recoverPrevious(s.path)
		return errors.Join(
			fmt.Errorf("reopen rebuilt catalogue index: %w", err),
			recoveryErr,
			s.reopenCurrent(),
		)
	}
	s.index = idx
	return nil
}

func (s *Store) reopenCurrent() error {
	idx, err := bleve.Open(s.path)
	if err != nil {
		s.closed = true
		return fmt.Errorf("reopen live catalogue index: %w", err)
	}
	s.index = idx
	return nil
}

func wrapError(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func indexMapping() (mapping.IndexMapping, error) {
	mapping := bleve.NewIndexMapping()
	if err := mapping.AddCustomAnalyzer(textAnalyzerName, map[string]any{
		"type":          custom.Name,
		"tokenizer":     unicode.Name,
		"token_filters": []string{lowercase.Name},
	}); err != nil {
		return nil, fmt.Errorf("define catalogue text analyzer: %w", err)
	}
	mapping.DefaultAnalyzer = textAnalyzerName
	mapping.IndexDynamic = false
	mapping.StoreDynamic = false
	mapping.DocValuesDynamic = false
	documentMapping := bleve.NewDocumentMapping()
	documentMapping.Dynamic = false
	for _, field := range []string{"title", "description"} {
		text := bleve.NewTextFieldMapping()
		text.Analyzer = textAnalyzerName
		text.Store = false
		text.IncludeTermVectors = true
		text.IncludeInAll = false
		text.DocValues = false
		documentMapping.AddFieldMappingsAt(field, text)
	}
	structured := bleve.NewTextFieldMapping()
	structured.Analyzer = textAnalyzerName
	structured.Store = false
	structured.IncludeTermVectors = false
	structured.IncludeInAll = false
	structured.DocValues = false
	documentMapping.AddFieldMappingsAt("structured", structured)
	provider := bleve.NewKeywordFieldMapping()
	provider.Store = false
	provider.IncludeTermVectors = false
	provider.IncludeInAll = false
	provider.DocValues = false
	documentMapping.AddFieldMappingsAt("provider", provider)
	group := bleve.NewKeywordFieldMapping()
	group.Store = true
	group.IncludeTermVectors = false
	group.IncludeInAll = false
	documentMapping.AddFieldMappingsAt("group", group)
	stored := bleve.NewKeywordFieldMapping()
	stored.Index = false
	stored.Store = true
	stored.IncludeTermVectors = false
	stored.IncludeInAll = false
	stored.DocValues = false
	documentMapping.AddFieldMappingsAt("sources", stored)
	for _, field := range []string{"completeness", "source_count"} {
		numeric := bleve.NewNumericFieldMapping()
		numeric.Index = false
		numeric.Store = false
		numeric.IncludeInAll = false
		numeric.DocValues = true
		documentMapping.AddFieldMappingsAt(field, numeric)
	}
	mapping.DefaultMapping = documentMapping
	return mapping, nil
}

func indexDocuments(ctx context.Context, idx bleve.Index, documents []prepared) error {
	batch := idx.NewBatch()
	for i, item := range documents {
		if err := ctx.Err(); err != nil {
			return err
		}
		doc := document{
			Title:        item.title,
			Structured:   item.structured,
			Description:  item.description,
			Provider:     item.provider,
			Group:        item.group,
			Sources:      item.encoded,
			Completeness: item.completeness,
			SourceCount:  item.sourceCount,
		}
		if err := batch.Index(item.id, doc); err != nil {
			return fmt.Errorf("map catalogue group %q provider %q: %w", item.group, item.provider, err)
		}
		if batch.Size() == batchSize || i == len(documents)-1 {
			if err := idx.Batch(batch); err != nil {
				return fmt.Errorf("write catalogue index batch: %w", err)
			}
			batch = idx.NewBatch()
		}
	}
	return nil
}

func prepare(ctx context.Context, records []domain.Record) ([]prepared, error) {
	items := make([]sourceItem, 0, len(records))
	var preparedBytes int64
	for i, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		normalized, err := domain.NormalizeRecord(record)
		if err != nil {
			return nil, fmt.Errorf("normalise catalogue record %d: %w", i, err)
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return nil, fmt.Errorf("encode catalogue record %d: %w", i, err)
		}
		preparedBytes, err = addPreparedBytes(preparedBytes, len(encoded), recordOverhead)
		if err != nil {
			return nil, err
		}
		items = append(items, sourceItem{key: sourceKey(normalized), record: normalized, encoded: encoded})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(items, func(a, b sourceItem) int {
		if order := strings.Compare(a.key, b.key); order != 0 {
			return order
		}
		return bytes.Compare(a.encoded, b.encoded)
	})
	unique := make([]sourceItem, 0, len(items))
	for start := 0; start < len(items); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := start + 1
		for end < len(items) && items[end].key == items[start].key {
			end++
		}
		item, err := mergeDuplicateSource(items[start:end])
		if err != nil {
			return nil, err
		}
		unique = append(unique, item)
		start = end
	}
	return buildDocuments(ctx, unique, preparedBytes)
}

func mergeDuplicateSource(items []sourceItem) (sourceItem, error) {
	var v1, v2 string
	hasPair := false
	for _, item := range items {
		hasPair = hasPair || item.record.InfoHashV1 != "" && item.record.InfoHashV2 != ""
		if item.record.InfoHashV1 != "" {
			if v1 != "" && v1 != item.record.InfoHashV1 {
				return sourceItem{}, fmt.Errorf("%w: %q", ErrSourceConflict, item.key)
			}
			v1 = item.record.InfoHashV1
		}
		if item.record.InfoHashV2 != "" {
			if v2 != "" && v2 != item.record.InfoHashV2 {
				return sourceItem{}, fmt.Errorf("%w: %q", ErrSourceConflict, item.key)
			}
			v2 = item.record.InfoHashV2
		}
	}
	if v1 != "" && v2 != "" && !hasPair {
		return sourceItem{}, fmt.Errorf("%w: %q has ambiguous v1/v2 association", ErrSourceConflict, items[0].key)
	}
	for i := range items {
		items[i].record.InfoHashV1, items[i].record.InfoHashV2 = v1, v2
		encoded, err := json.Marshal(items[i].record)
		if err != nil {
			return sourceItem{}, fmt.Errorf("encode merged catalogue source %q: %w", items[i].key, err)
		}
		items[i].encoded = encoded
	}
	best := items[0]
	for _, item := range items[1:] {
		if sourceItemLess(item, best) {
			best = item
		}
	}
	return best, nil
}

func sourceItemLess(a, b sourceItem) bool {
	if difference := completeness(b.record) - completeness(a.record); difference != 0 {
		return difference < 0
	}
	return bytes.Compare(a.encoded, b.encoded) < 0
}

func buildDocuments(ctx context.Context, items []sourceItem, preparedBytes int64) ([]prepared, error) {
	parents, err := assignGroups(ctx, items)
	if err != nil {
		return nil, err
	}
	groups := make(map[int][]sourceItem, len(items))
	for i, item := range items {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		root := find(parents, i)
		groups[root] = append(groups[root], item)
	}
	documents := make([]prepared, 0, len(items))
	for _, group := range groups {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := groupEncodedSize(group); err != nil {
			return nil, err
		}
		slices.SortFunc(group, func(a, b sourceItem) int {
			if order := strings.Compare(a.key, b.key); order != 0 {
				return order
			}
			return bytes.Compare(a.encoded, b.encoded)
		})
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		groupID := sourceGroupID(group)
		sources := make([]domain.Record, len(group))
		byProvider := make(map[string][]sourceItem)
		groupCompleteness := 0
		for i, item := range group {
			sources[i] = item.record
			byProvider[item.record.Provider] = append(byProvider[item.record.Provider], item)
			groupCompleteness = max(groupCompleteness, completeness(item.record))
		}
		encoded, err := json.Marshal(sources)
		if err != nil {
			return nil, fmt.Errorf("encode catalogue group %q: %w", groupID, err)
		}
		encodedString := string(encoded)
		providers := make([]string, 0, len(byProvider))
		for provider := range byProvider {
			providers = append(providers, provider)
		}
		slices.Sort(providers)
		for _, provider := range providers {
			providerSources := byProvider[provider]
			title := sourceTexts(providerSources, func(record domain.Record) string { return record.Title })
			description := sourceTexts(providerSources, func(record domain.Record) string { return record.Description })
			structured := sourceTexts(providerSources, structuredText)
			preparedBytes, err = addPreparedBytes(preparedBytes, len(encoded), stringsLength(title), stringsLength(description), stringsLength(structured))
			if err != nil {
				return nil, err
			}
			documents = append(documents, prepared{
				id: digest(groupID + "\x00" + provider), group: groupID, provider: provider,
				title: title, description: description, structured: structured,
				encoded: encodedString, completeness: groupCompleteness, sourceCount: len(sources),
			})
		}
	}
	slices.SortFunc(documents, func(a, b prepared) int { return strings.Compare(a.id, b.id) })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return documents, nil
}

func assignGroups(ctx context.Context, items []sourceItem) ([]int, error) {
	parents := make([]int, len(items))
	for i := range parents {
		parents[i] = i
	}
	seen := make(map[string]int, len(items))
	for i, item := range items {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for _, hash := range []struct{ kind, value string }{{"v1", item.record.InfoHashV1}, {"v2", item.record.InfoHashV2}} {
			if hash.value == "" {
				continue
			}
			key := hash.kind + ":" + hash.value
			if previous, ok := seen[key]; ok {
				union(parents, i, previous)
			} else {
				seen[key] = i
			}
		}
	}
	return parents, nil
}

func groupEncodedSize(group []sourceItem) (int, error) {
	if len(group) > maxGroupSources {
		return 0, fmt.Errorf("prepare catalogue index: group has %d sources, limit is %d", len(group), maxGroupSources)
	}
	total := 2 // JSON array brackets.
	for _, item := range group {
		var err error
		total, err = addGroupBytes(total, len(item.encoded)+1)
		if err != nil {
			return 0, err
		}
	}
	return total, nil
}

func addGroupBytes(total, size int) (int, error) {
	if size < 0 || size > maxGroupBytes-total {
		return 0, fmt.Errorf("prepare catalogue index: group metadata exceeds %d bytes", maxGroupBytes)
	}
	return total + size, nil
}

func find(parents []int, at int) int {
	for parents[at] != at {
		parents[at] = parents[parents[at]]
		at = parents[at]
	}
	return at
}

func union(parents []int, a, b int) {
	a, b = find(parents, a), find(parents, b)
	if a == b {
		return
	}
	if a < b {
		parents[b] = a
	} else {
		parents[a] = b
	}
}

func sourceKey(record domain.Record) string {
	return record.Provider + "\x00" + record.SourceID
}

func sourceGroupID(items []sourceItem) string {
	hashes := make([]string, 0, len(items)*2)
	for _, item := range items {
		if item.record.InfoHashV1 != "" {
			hashes = append(hashes, "v1:"+item.record.InfoHashV1)
		}
		if item.record.InfoHashV2 != "" {
			hashes = append(hashes, "v2:"+item.record.InfoHashV2)
		}
	}
	if len(hashes) == 0 {
		return digest(items[0].key)
	}
	slices.Sort(hashes)
	hashes = slices.Compact(hashes)
	return digest(strings.Join(hashes, "\x00"))
}

func sourceTexts(items []sourceItem, value func(domain.Record) string) []string {
	values := make([]string, 0, len(items))
	for _, item := range items {
		if normalized := domain.NormalizeSearchText(value(item.record)); normalized != "" {
			values = append(values, normalized)
		}
	}
	slices.Sort(values)
	return slices.Compact(values)
}

func stringsLength(values []string) int {
	total := 0
	for _, value := range values {
		total += len(value)
	}
	return total
}

func addPreparedBytes(total int64, sizes ...int) (int64, error) {
	for _, size := range sizes {
		if size < 0 || int64(size) > maxPreparedBytes-total {
			return 0, fmt.Errorf("prepare catalogue index: indexed data exceeds %d bytes", maxPreparedBytes)
		}
		total += int64(size)
	}
	return total, nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func completeness(record domain.Record) int {
	score := 0
	for _, value := range []string{record.Description, record.InfoHashV1, record.InfoHashV2, record.DetailsURL, record.MagnetURI, record.TorrentURL} {
		if value != "" {
			score++
		}
	}
	for _, present := range []bool{record.SizeBytes != nil, record.PublishedAt != nil, record.Seeders != nil, record.Leechers != nil} {
		if present {
			score++
		}
	}
	return score + len(record.Categories) + len(record.ContentKinds) + len(record.MediaKinds) + len(record.Extensions) + len(record.Languages) + len(record.Architectures) + len(record.Resolutions) + len(record.Codecs)
}

func structuredText(record domain.Record) string {
	parts := make([]string, 0, len(record.Categories)+len(record.ContentKinds)+len(record.MediaKinds)+len(record.Extensions)+len(record.Languages)+len(record.Architectures)+len(record.Resolutions)+len(record.Codecs))
	parts = append(parts, record.Categories...)
	parts = append(parts, record.ContentKinds...)
	parts = append(parts, record.MediaKinds...)
	parts = append(parts, record.Extensions...)
	parts = append(parts, record.Languages...)
	parts = append(parts, record.Architectures...)
	for _, resolution := range record.Resolutions {
		parts = append(parts, strconv.Itoa(resolution)+"p")
	}
	parts = append(parts, record.Codecs...)
	return strings.Join(parts, " ")
}

func buildQuery(ast productquery.AST) blevequery.Query {
	must := make([]blevequery.Query, 0, len(ast.Required)+len(ast.Phrases)+1)
	should := make([]blevequery.Query, 0, len(ast.Optional)+len(ast.Phrases))
	mustNot := make([]blevequery.Query, 0, len(ast.Excluded)+len(ast.Phrases)+len(ast.Providers.Deny))
	for _, term := range ast.Required {
		must = append(must, fieldMatch(term.Normalized, false))
	}
	for _, term := range ast.Optional {
		should = append(should, fieldMatch(term.Normalized, false))
	}
	for _, term := range ast.Excluded {
		mustNot = append(mustNot, fieldMatch(term.Normalized, false))
	}
	for _, phrase := range ast.Phrases {
		match := fieldMatch(phrase.Normalized, true)
		switch phrase.Occurrence {
		case "optional":
			should = append(should, match)
		case "excluded":
			mustNot = append(mustNot, match)
		default:
			must = append(must, match)
		}
	}
	if len(ast.Providers.Allow) > 0 {
		allowed := make([]blevequery.Query, 0, len(ast.Providers.Allow))
		for _, provider := range ast.Providers.Allow {
			term := bleve.NewTermQuery(provider.Value)
			term.SetField("provider")
			allowed = append(allowed, term)
		}
		must = append(must, bleve.NewDisjunctionQuery(allowed...))
	}
	for _, provider := range ast.Providers.Deny {
		term := bleve.NewTermQuery(provider.Value)
		term.SetField("provider")
		mustNot = append(mustNot, term)
	}
	if len(must) == 0 {
		must = append(must, bleve.NewMatchAllQuery())
	}
	result := bleve.NewBooleanQuery()
	result.AddMust(must...)
	result.AddShould(should...)
	result.AddMustNot(mustNot...)
	return result
}

func fieldMatch(value string, phrase bool) blevequery.Query {
	fields := []struct {
		name  string
		boost float64
	}{{"title", 8}, {"structured", 3}, {"description", 1}}
	if phrase {
		fields = []struct {
			name  string
			boost float64
		}{{"title", 8}, {"description", 1}}
	}
	matches := make([]blevequery.Query, 0, len(fields))
	for _, field := range fields {
		if phrase {
			match := bleve.NewMatchPhraseQuery(value)
			match.SetField(field.name)
			match.SetBoost(field.boost)
			matches = append(matches, match)
		} else {
			match := bleve.NewMatchQuery(value)
			match.SetField(field.name)
			match.SetBoost(field.boost)
			match.SetOperator(blevequery.MatchQueryOperatorAnd)
			matches = append(matches, match)
		}
	}
	return bleve.NewDisjunctionQuery(matches...)
}

func validateQuery(ast productquery.AST, limit int) error {
	if limit < 1 || limit > productquery.MaxLimit {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalidQuery, productquery.MaxLimit)
	}
	if ast.SchemaVersion != productquery.SchemaVersion || len(ast.Raw) > productquery.MaxInputBytes {
		return fmt.Errorf("%w: unsupported schema or oversized raw input", ErrInvalidQuery)
	}
	count := len(ast.Required) + len(ast.Optional) + len(ast.Excluded) + len(ast.Phrases) + len(ast.Categories) + len(ast.ContentKinds) + len(ast.Extensions) + len(ast.MediaKinds) + len(ast.Languages) + len(ast.Architectures) + len(ast.Resolutions) + len(ast.Codecs) + len(ast.Providers.Allow) + len(ast.Providers.Deny) + len(ast.Order)
	if count > productquery.MaxTokens {
		return fmt.Errorf("%w: too many clauses", ErrInvalidQuery)
	}
	for _, term := range append(append(slices.Clone(ast.Required), ast.Optional...), ast.Excluded...) {
		if term.Normalized == "" || len(term.Normalized) > productquery.MaxInputBytes || !utf8.ValidString(term.Normalized) {
			return fmt.Errorf("%w: invalid text clause", ErrInvalidQuery)
		}
	}
	for _, phrase := range ast.Phrases {
		if phrase.Normalized == "" || len(phrase.Normalized) > productquery.MaxInputBytes || !utf8.ValidString(phrase.Normalized) ||
			phrase.Occurrence != "required" && phrase.Occurrence != "optional" && phrase.Occurrence != "excluded" {
			return fmt.Errorf("%w: invalid phrase clause", ErrInvalidQuery)
		}
	}
	for _, values := range [][]productquery.ValueClause{ast.Categories, ast.ContentKinds, ast.Extensions, ast.MediaKinds, ast.Languages, ast.Architectures, ast.Codecs, ast.Providers.Allow, ast.Providers.Deny} {
		for _, value := range values {
			if value.Value == "" || len(value.Value) > domain.MaxFacetBytes || !utf8.ValidString(value.Value) {
				return fmt.Errorf("%w: invalid structured value", ErrInvalidQuery)
			}
		}
	}
	for _, resolution := range ast.Resolutions {
		if resolution.Vertical < 100 || resolution.Vertical > 8640 {
			return fmt.Errorf("%w: invalid resolution", ErrInvalidQuery)
		}
	}
	for _, order := range ast.Order {
		if !slices.Contains([]string{"relevance", "date", "size", "title"}, order.Field) || !slices.Contains([]string{"asc", "desc"}, order.Direction) {
			return fmt.Errorf("%w: invalid ordering", ErrInvalidQuery)
		}
	}
	for _, bound := range []*productquery.DateBound{ast.Date.Start, ast.Date.End} {
		if bound != nil {
			if _, err := time.Parse(time.DateOnly, bound.Date); err != nil {
				return fmt.Errorf("%w: invalid date bound", ErrInvalidQuery)
			}
		}
	}
	for _, bound := range []*productquery.ByteBound{ast.Size.Min, ast.Size.Max} {
		if bound != nil && bound.Bytes < 0 {
			return fmt.Errorf("%w: negative size bound", ErrInvalidQuery)
		}
	}
	return nil
}

func matchesGroup(sources []domain.Record, ast productquery.AST) bool {
	return matchesGroupValues(sources, ast.Categories, func(record domain.Record) []string { return record.Categories }) &&
		matchesGroupValues(sources, ast.ContentKinds, func(record domain.Record) []string { return record.ContentKinds }) &&
		matchesGroupValues(sources, ast.Extensions, func(record domain.Record) []string { return record.Extensions }) &&
		matchesGroupValues(sources, ast.MediaKinds, func(record domain.Record) []string { return record.MediaKinds }) &&
		matchesGroupValues(sources, ast.Languages, func(record domain.Record) []string { return record.Languages }) &&
		matchesGroupValues(sources, ast.Architectures, func(record domain.Record) []string { return record.Architectures }) &&
		matchesGroupValues(sources, ast.Codecs, func(record domain.Record) []string { return record.Codecs }) &&
		matchesGroupResolutions(sources, ast.Resolutions) &&
		matchesSize(exactSize(sources), ast.Size) &&
		matchesDate(exactDate(sources), ast.Date)
}

func matchesGroupValues(sources []domain.Record, wanted []productquery.ValueClause, values func(domain.Record) []string) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, source := range sources {
		if matchesValues(values(source), wanted) {
			return true
		}
	}
	return false
}

func matchesGroupResolutions(sources []domain.Record, wanted []productquery.Resolution) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, source := range sources {
		if matchesResolutions(source.Resolutions, wanted) {
			return true
		}
	}
	return false
}

func exactSize(sources []domain.Record) *int64 {
	var value *int64
	for _, source := range sources {
		if source.SizeBytes == nil {
			continue
		}
		if value != nil && *value != *source.SizeBytes {
			return nil
		}
		copy := *source.SizeBytes
		value = &copy
	}
	return value
}

func exactDate(sources []domain.Record) *time.Time {
	var value *time.Time
	for _, source := range sources {
		if source.PublishedAt == nil {
			continue
		}
		if value != nil && !value.Equal(*source.PublishedAt) {
			return nil
		}
		copy := *source.PublishedAt
		value = &copy
	}
	return value
}

func sourceLess(a, b domain.Record) bool {
	if difference := completeness(b) - completeness(a); difference != 0 {
		return difference < 0
	}
	if order := strings.Compare(a.Provider, b.Provider); order != 0 {
		return order < 0
	}
	if order := strings.Compare(a.SourceID, b.SourceID); order != 0 {
		return order < 0
	}
	return false
}

func providerAllowed(provider string, selection productquery.ProviderSelection) bool {
	for _, denied := range selection.Deny {
		if provider == denied.Value {
			return false
		}
	}
	if len(selection.Allow) == 0 {
		return true
	}
	for _, allowed := range selection.Allow {
		if provider == allowed.Value {
			return true
		}
	}
	return false
}

func matchesValues(have []string, wanted []productquery.ValueClause) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, value := range wanted {
		if slices.Contains(have, value.Value) {
			return true
		}
	}
	return false
}

func matchesResolutions(have []int, wanted []productquery.Resolution) bool {
	if len(wanted) == 0 {
		return true
	}
	for _, value := range wanted {
		if slices.Contains(have, value.Vertical) {
			return true
		}
	}
	return false
}

func matchesSize(size *int64, bounds productquery.ByteRange) bool {
	if bounds.Min == nil && bounds.Max == nil {
		return true
	}
	if size == nil {
		return false
	}
	return above(*size, bounds.Min) && below(*size, bounds.Max)
}

func above(value int64, bound *productquery.ByteBound) bool {
	return bound == nil || value > bound.Bytes || bound.Inclusive && value == bound.Bytes
}

func below(value int64, bound *productquery.ByteBound) bool {
	return bound == nil || value < bound.Bytes || bound.Inclusive && value == bound.Bytes
}

func matchesDate(date *time.Time, bounds productquery.DateRange) bool {
	if bounds.Start == nil && bounds.End == nil {
		return true
	}
	if date == nil {
		return false
	}
	value := date.Format(time.DateOnly)
	return afterDate(value, bounds.Start) && beforeDate(value, bounds.End)
}

func afterDate(value string, bound *productquery.DateBound) bool {
	return bound == nil || value > bound.Date || bound.Inclusive && value == bound.Date
}

func beforeDate(value string, bound *productquery.DateBound) bool {
	return bound == nil || value < bound.Date || bound.Inclusive && value == bound.Date
}

func hasNonRelevanceOrder(order []productquery.OrderClause) bool {
	for _, clause := range order {
		if clause.Field != "relevance" {
			return true
		}
	}
	return false
}

func compareHits(a, b Hit, order []productquery.OrderClause) int {
	if len(order) == 0 {
		if result := compareFloatDescending(a.Score, b.Score); result != 0 {
			return result
		}
		return compareHitSignals(a, b)
	}
	for _, clause := range order {
		var result int
		switch clause.Field {
		case "relevance":
			result = compareFloatDescending(a.Score, b.Score)
		case "date":
			result = compareOptionalDate(a.Record.PublishedAt, b.Record.PublishedAt)
			if clause.Direction == "desc" && a.Record.PublishedAt != nil && b.Record.PublishedAt != nil {
				result = -result
			}
		case "size":
			result = compareOptionalInt64(a.Record.SizeBytes, b.Record.SizeBytes)
			if clause.Direction == "desc" && a.Record.SizeBytes != nil && b.Record.SizeBytes != nil {
				result = -result
			}
		case "title":
			result = strings.Compare(domain.NormalizeSearchText(a.Record.Title), domain.NormalizeSearchText(b.Record.Title))
			if clause.Direction == "desc" {
				result = -result
			}
		}
		if result != 0 {
			return result
		}
	}
	return compareHitSignals(a, b)
}

func compareHitSignals(a, b Hit) int {
	aSources, bSources := a.Sources, b.Sources
	if a.rankSources != nil {
		aSources = a.rankSources
	}
	if b.rankSources != nil {
		bSources = b.rankSources
	}
	if result := groupCompleteness(bSources) - groupCompleteness(aSources); result != 0 {
		return result
	}
	if result := len(bSources) - len(aSources); result != 0 {
		return result
	}
	return strings.Compare(a.ID, b.ID)
}

func groupCompleteness(sources []domain.Record) int {
	result := 0
	for _, source := range sources {
		result = max(result, completeness(source))
	}
	return result
}

func compareFloatDescending(a, b float64) int {
	if a > b {
		return -1
	}
	if a < b {
		return 1
	}
	return 0
}

func compareOptionalDate(a, b *time.Time) int {
	if a == nil || b == nil {
		return compareMissing(a == nil, b == nil)
	}
	return a.Compare(*b)
}

func compareOptionalInt64(a, b *int64) int {
	if a == nil || b == nil {
		return compareMissing(a == nil, b == nil)
	}
	if *a < *b {
		return -1
	}
	if *a > *b {
		return 1
	}
	return 0
}

func compareMissing(aMissing, bMissing bool) int {
	if aMissing == bMissing {
		return 0
	}
	if aMissing {
		return 1
	}
	return -1
}
