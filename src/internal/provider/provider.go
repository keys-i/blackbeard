package provider

import (
	"context"
	"errors"

	"github.com/keys-i/blackbeard/src/internal/domain"
	"github.com/keys-i/blackbeard/src/internal/query"
)

// SearchRequest is the parsed query and per-provider result budget.
type SearchRequest struct {
	Query query.AST
	Limit int
}

func (r SearchRequest) Validate() error {
	if r.Limit < 1 || r.Limit > query.MaxLimit {
		return errors.New("provider search limit must be between 1 and 1000")
	}
	return nil
}

// Provider searches one live or cached catalogue.
type Provider interface {
	Search(context.Context, SearchRequest) ([]domain.Record, error)
}

// Source is a resolved payload. A resolver returns either a magnet URI or
// verified metainfo bytes; TorrentURL may retain the metainfo provenance.
type Source struct {
	MagnetURI  string
	TorrentURL string
	Metainfo   []byte
}

// Resolver turns a catalogue record into a directly usable payload source.
type Resolver interface {
	Resolve(context.Context, domain.Record) (Source, error)
}

// CatalogSnapshot is a complete provider refresh or an unchanged response.
type CatalogSnapshot struct {
	Records     []domain.Record
	NotModified bool
}

// CatalogSyncer refreshes a provider's offline catalogue.
type CatalogSyncer interface {
	Sync(context.Context) (CatalogSnapshot, error)
}

// HealthChecker performs a bounded availability check.
type HealthChecker interface {
	Health(context.Context) error
}
