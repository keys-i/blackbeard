package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/keys-i/blackbeard/src/internal/domain"
	"github.com/keys-i/blackbeard/src/internal/index"
	"github.com/keys-i/blackbeard/src/internal/output"
	"github.com/keys-i/blackbeard/src/internal/provider"
	"github.com/keys-i/blackbeard/src/internal/provider/academictorrents"
	"github.com/keys-i/blackbeard/src/internal/query"
	"github.com/keys-i/blackbeard/src/internal/termtext"
)

const (
	academicProvider = "academic_torrents"
	syncTimeout      = 30 * time.Second
	rebuildTimeout   = 2 * time.Minute
	searchTimeout    = 4 * time.Second
)

type catalogueDeps struct {
	userCacheDir func() (string, error)
	newAcademic  func(string) (provider.CatalogSyncer, error)
}

type cataloguePaths struct {
	cacheRoot string
	academic  string
	index     string
}

type searchResult struct {
	Rank    int             `json:"rank"`
	ID      string          `json:"id"`
	Record  domain.Record   `json:"record"`
	Sources []domain.Record `json:"sources"`
}

type searchOutput struct {
	Mode     string          `json:"mode"`
	Results  []searchResult  `json:"results"`
	Warnings []query.Warning `json:"warnings"`
}

type syncStatus struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Records  int    `json:"records"`
}

type syncOutput struct {
	Providers []syncStatus `json:"providers"`
	Records   int          `json:"records"`
}

func productionCatalogueDeps() catalogueDeps {
	return catalogueDeps{userCacheDir: os.UserCacheDir, newAcademic: academictorrents.NewSyncer}
}

func resolveCataloguePaths(deps catalogueDeps) (cataloguePaths, error) {
	if deps.userCacheDir == nil {
		return cataloguePaths{}, errors.New("catalogue cache directory resolver is unavailable")
	}
	root, err := deps.userCacheDir()
	if err != nil {
		return cataloguePaths{}, fmt.Errorf("resolve user cache directory: %w", err)
	}
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return cataloguePaths{}, errors.New("user cache directory must be absolute")
	}
	cacheRoot := root
	root = filepath.Join(cacheRoot, "blackbeard")
	return cataloguePaths{
		cacheRoot: cacheRoot,
		academic:  filepath.Join(root, "providers", academicProvider),
		index:     filepath.Join(root, "catalogue.bleve"),
	}, nil
}

func syncAcademicCatalogue(ctx context.Context, deps catalogueDeps) (result syncStatus, err error) {
	paths, err := resolveCataloguePaths(deps)
	if err != nil {
		return result, err
	}
	if deps.newAcademic == nil {
		return result, errors.New("academic torrents catalogue sync is unavailable")
	}
	if err := secureCacheDirectories(paths.cacheRoot, true, "blackbeard", "providers", academicProvider); err != nil {
		return result, err
	}

	initialized, err := catalogueExists(paths.index)
	if err != nil {
		return result, err
	}
	var store *index.Store
	var existing []domain.Record
	created := false
	defer func() {
		if store == nil {
			return
		}
		err = errors.Join(err, store.Close())
		if created && err != nil {
			err = errors.Join(err, wrapCleanup("remove incomplete catalogue index", os.RemoveAll(paths.index)))
		}
	}()
	if initialized {
		store, err = index.Open(paths.index)
		if err != nil {
			return result, err
		}
		readCtx, cancelRead := context.WithTimeout(ctx, rebuildTimeout)
		existing, err = store.Records(readCtx)
		cancelRead()
		if err != nil {
			return result, fmt.Errorf("read current catalogue: %w", err)
		}
	}

	syncer, err := deps.newAcademic(paths.academic)
	if err != nil {
		return result, fmt.Errorf("create academic torrents catalogue: %w", err)
	}
	syncCtx, cancelSync := context.WithTimeout(ctx, syncTimeout)
	snapshot, err := syncer.Sync(syncCtx)
	cancelSync()
	if err != nil {
		return result, fmt.Errorf("sync academic torrents catalogue: %w", err)
	}
	if len(snapshot.Records) == 0 {
		return result, errors.New("sync academic torrents catalogue: empty snapshot")
	}

	aggregate := existing[:0]
	for _, record := range existing {
		if record.Provider != academicProvider {
			aggregate = append(aggregate, record)
		}
	}
	for i, record := range snapshot.Records {
		if record.Provider != academicProvider {
			return result, fmt.Errorf("sync academic torrents catalogue: record %d has provider %q", i+1, record.Provider)
		}
		aggregate = append(aggregate, record)
	}
	if store == nil {
		created = true
		store, err = index.Open(paths.index)
		if err != nil {
			return result, errors.Join(err, wrapCleanup("remove incomplete catalogue index", os.RemoveAll(paths.index)))
		}
	}

	rebuildCtx, cancelRebuild := context.WithTimeout(ctx, rebuildTimeout)
	err = store.Rebuild(rebuildCtx, aggregate)
	cancelRebuild()
	if err != nil {
		return result, fmt.Errorf("rebuild catalogue index: %w", err)
	}
	status := "updated"
	if snapshot.NotModified {
		status = "unchanged"
	}
	return syncStatus{Provider: academicProvider, Status: status, Records: len(snapshot.Records)}, nil
}

func searchOffline(ctx context.Context, deps catalogueDeps, ast query.AST, limit int) (results []searchResult, err error) {
	paths, err := resolveCataloguePaths(deps)
	if err != nil {
		return nil, err
	}
	if err := secureCacheDirectories(paths.cacheRoot, false, "blackbeard"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("offline catalogue is not initialized; run `blackbeard providers sync`")
		}
		return nil, err
	}
	initialized, err := catalogueExists(paths.index)
	if err != nil {
		return nil, err
	}
	if !initialized {
		return nil, errors.New("offline catalogue is not initialized; run `blackbeard providers sync`")
	}
	store, err := index.Open(paths.index)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, store.Close()) }()

	searchCtx, cancel := context.WithTimeout(ctx, searchTimeout)
	hits, err := store.Search(searchCtx, ast, limit)
	cancel()
	if err != nil {
		return nil, err
	}
	results = make([]searchResult, len(hits))
	for i, hit := range hits {
		results[i] = searchResult{Rank: i + 1, ID: hit.ID, Record: hit.Record, Sources: hit.Sources}
	}
	return results, nil
}

func secureCacheDirectories(base string, create bool, names ...string) error {
	if create {
		if err := os.MkdirAll(base, 0o700); err != nil {
			return fmt.Errorf("create user cache directory: %w", err)
		}
	}
	current, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("open user cache directory: %w", err)
	}
	for _, name := range names {
		info, statErr := current.Lstat(name)
		if errors.Is(statErr, os.ErrNotExist) && create {
			statErr = current.Mkdir(name, 0o700)
			if statErr == nil {
				info, statErr = current.Lstat(name)
			}
		}
		if statErr != nil {
			return errors.Join(fmt.Errorf("inspect cache directory %q: %w", name, statErr), current.Close())
		}
		if !info.IsDir() {
			return errors.Join(fmt.Errorf("cache path component %q is not a directory", name), current.Close())
		}
		next, openErr := current.OpenRoot(name)
		closeErr := current.Close()
		if openErr != nil {
			return errors.Join(fmt.Errorf("open cache directory %q: %w", name, openErr), closeErr)
		}
		if closeErr != nil {
			return errors.Join(fmt.Errorf("close parent cache directory: %w", closeErr), next.Close())
		}
		current = next
	}
	if err := current.Close(); err != nil {
		return fmt.Errorf("close cache directory: %w", err)
	}
	return nil
}

func catalogueExists(path string) (bool, error) {
	for _, candidate := range []string{path, path + ".previous"} {
		if _, err := os.Stat(candidate); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("inspect offline catalogue: %w", err)
		}
	}
	return false, nil
}

func wrapCleanup(message string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", message, err)
}

func writeOfflineSearch(stdout *output.Encoder, warningDst io.Writer, format string, ast query.AST, results []searchResult) error {
	data := searchOutput{Mode: "offline", Results: results, Warnings: ast.Warnings}
	switch format {
	case outputTable:
		rows := make([][]string, len(results))
		for i, result := range results {
			rows[i] = []string{
				strconv.Itoa(result.Rank), result.ID, result.Record.Title,
				resultProviders(result.Sources), optionalBytes(result.Record.SizeBytes), optionalDate(result.Record.PublishedAt),
			}
		}
		if err := stdout.Table(output.Table{Columns: []string{"RANK", "ID", "TITLE", "PROVIDERS", "BYTES", "DATE"}, Rows: rows}); err != nil {
			return err
		}
		for _, warning := range ast.Warnings {
			if _, err := fmt.Fprintf(warningDst, "warning: %s\n", termtext.Sanitize(warning.Message, 4096)); err != nil {
				return fmt.Errorf("write query warning: %w", err)
			}
		}
		return nil
	case outputJSON:
		return stdout.JSON("search", data)
	case outputNDJSON:
		for _, result := range results {
			if err := stdout.NDJSON("result", result); err != nil {
				return err
			}
		}
		return stdout.NDJSON("done", struct {
			Count    int             `json:"count"`
			Warnings []query.Warning `json:"warnings"`
		}{Count: len(results), Warnings: ast.Warnings})
	default:
		return usageError(fmt.Errorf("invalid output format %q", format))
	}
}

func writeSyncResult(dst *output.Encoder, format string, result syncStatus) error {
	data := syncOutput{Providers: []syncStatus{result}, Records: result.Records}
	switch format {
	case outputTable:
		return dst.Table(output.Table{
			Columns: []string{"PROVIDER", "STATUS", "RECORDS"},
			Rows:    [][]string{{result.Provider, result.Status, strconv.Itoa(result.Records)}},
		})
	case outputJSON:
		return dst.JSON("provider_sync", data)
	case outputNDJSON:
		if err := dst.NDJSON("provider_sync", result); err != nil {
			return err
		}
		return dst.NDJSON("done", struct {
			Records int `json:"records"`
		}{Records: result.Records})
	default:
		return usageError(fmt.Errorf("invalid output format %q", format))
	}
}

func resultProviders(records []domain.Record) string {
	providers := make([]string, len(records))
	for i, record := range records {
		providers[i] = record.Provider
	}
	slices.Sort(providers)
	return strings.Join(slices.Compact(providers), ",")
}

func optionalBytes(value *int64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatInt(*value, 10)
}

func optionalDate(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.Format(time.DateOnly)
}
