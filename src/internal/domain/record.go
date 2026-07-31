// Package domain contains dependency-free product data shared by providers and indexes.
package domain

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	MaxTitleBytes       = 1024
	MaxDescriptionBytes = 64 << 10
	MaxSourceIDBytes    = 1024
	MaxURLBytes         = 4096
	MaxFacetValues      = 64
	MaxFacetBytes       = 64
)

var (
	ErrInvalidRecord = errors.New("invalid catalogue record")
	fold             = cases.Fold()
)

// NormalizeSearchText returns the shared lexical representation used by the
// query parser and catalogue index. Display text remains unchanged.
func NormalizeSearchText(value string) string {
	value = strings.TrimSpace(value)
	if isASCII(value) {
		return strings.ToLower(value)
	}
	value = norm.NFKC.String(value)
	return norm.NFKC.String(fold.String(value))
}

func isASCII(value string) bool {
	for i := range len(value) {
		if value[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// Record is one provider's catalogue entry. Nil numeric and date fields mean
// unknown; callers must not treat them as zero.
type Record struct {
	Provider      string     `json:"provider"`
	SourceID      string     `json:"source_id"`
	Title         string     `json:"title"`
	Description   string     `json:"description,omitempty"`
	InfoHashV1    string     `json:"infohash_v1,omitempty"`
	InfoHashV2    string     `json:"infohash_v2,omitempty"`
	DetailsURL    string     `json:"details_url,omitempty"`
	MagnetURI     string     `json:"magnet_uri,omitempty"`
	TorrentURL    string     `json:"torrent_url,omitempty"`
	SizeBytes     *int64     `json:"size_bytes,omitempty"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	Categories    []string   `json:"categories,omitempty"`
	ContentKinds  []string   `json:"content_kinds,omitempty"`
	MediaKinds    []string   `json:"media_kinds,omitempty"`
	Extensions    []string   `json:"extensions,omitempty"`
	Languages     []string   `json:"languages,omitempty"`
	Architectures []string   `json:"architectures,omitempty"`
	Resolutions   []int      `json:"resolutions,omitempty"`
	Codecs        []string   `json:"codecs,omitempty"`
	Seeders       *int       `json:"seeders,omitempty"`
	Leechers      *int       `json:"leechers,omitempty"`
}

// NormalizeRecord validates hostile provider data and returns a canonical,
// detached value suitable for deterministic indexing.
func NormalizeRecord(input Record) (Record, error) {
	r := input
	var err error
	if r.Provider, err = identifier(r.Provider, "provider", MaxFacetBytes); err != nil {
		return Record{}, err
	}
	if r.SourceID, err = boundedText(r.SourceID, "source ID", MaxSourceIDBytes, true); err != nil {
		return Record{}, err
	}
	if r.Title, err = boundedText(r.Title, "title", MaxTitleBytes, true); err != nil {
		return Record{}, err
	}
	if r.Description, err = boundedText(r.Description, "description", MaxDescriptionBytes, false); err != nil {
		return Record{}, err
	}
	if r.InfoHashV1, err = infoHash(r.InfoHashV1, 40, "v1"); err != nil {
		return Record{}, err
	}
	if r.InfoHashV2, err = infoHash(r.InfoHashV2, 64, "v2"); err != nil {
		return Record{}, err
	}
	if r.DetailsURL, err = webURL(r.DetailsURL, "details URL"); err != nil {
		return Record{}, err
	}
	if r.TorrentURL, err = webURL(r.TorrentURL, "torrent URL"); err != nil {
		return Record{}, err
	}
	if r.MagnetURI, err = magnetURI(r.MagnetURI); err != nil {
		return Record{}, err
	}

	if r.SizeBytes != nil {
		if *r.SizeBytes < 0 {
			return Record{}, invalid("size must not be negative")
		}
		r.SizeBytes = clone(r.SizeBytes)
	}
	if r.PublishedAt != nil {
		if r.PublishedAt.IsZero() {
			return Record{}, invalid("published date is zero")
		}
		date := r.PublishedAt.UTC()
		date = time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
		r.PublishedAt = &date
	}
	if r.Seeders, err = nonNegative(r.Seeders, "seeders"); err != nil {
		return Record{}, err
	}
	if r.Leechers, err = nonNegative(r.Leechers, "leechers"); err != nil {
		return Record{}, err
	}

	for _, facet := range []struct {
		name   string
		values *[]string
	}{
		{"category", &r.Categories},
		{"content kind", &r.ContentKinds},
		{"media kind", &r.MediaKinds},
		{"language", &r.Languages},
		{"architecture", &r.Architectures},
		{"codec", &r.Codecs},
	} {
		if *facet.values, err = identifiers(*facet.values, facet.name); err != nil {
			return Record{}, err
		}
	}
	if r.Extensions, err = extensions(r.Extensions); err != nil {
		return Record{}, err
	}
	if len(r.Resolutions) > MaxFacetValues {
		return Record{}, invalid("too many resolutions")
	}
	r.Resolutions = slices.Clone(r.Resolutions)
	for _, resolution := range r.Resolutions {
		if resolution < 100 || resolution > 8640 {
			return Record{}, invalid("resolution %d is outside 100..8640", resolution)
		}
	}
	slices.Sort(r.Resolutions)
	r.Resolutions = slices.Compact(r.Resolutions)
	return r, nil
}

func boundedText(value, name string, limit int, required bool) (string, error) {
	if !utf8.ValidString(value) {
		return "", invalid("%s is not valid UTF-8", name)
	}
	if len(value) > limit {
		return "", invalid("%s exceeds %d bytes", name, limit)
	}
	value = norm.NFKC.String(value)
	value = strings.Map(cleanRune, value)
	value = strings.Join(strings.Fields(value), " ")
	if required && value == "" {
		return "", invalid("%s is empty", name)
	}
	if len(value) > limit {
		return "", invalid("normalised %s exceeds %d bytes", name, limit)
	}
	return value, nil
}

func cleanRune(r rune) rune {
	if unicode.IsControl(r) || isBidiControl(r) {
		return ' '
	}
	return r
}

func isBidiControl(r rune) bool {
	return r == '\u061c' || r == '\u200e' || r == '\u200f' ||
		r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069'
}

func identifier(value, name string, limit int) (string, error) {
	if !utf8.ValidString(value) {
		return "", invalid("%s is not valid UTF-8", name)
	}
	value = strings.ToLower(norm.NFKC.String(strings.TrimSpace(value)))
	if value == "" || len(value) > limit {
		return "", invalid("%s must contain 1..%d bytes", name, limit)
	}
	for _, r := range value {
		if r > unicode.MaxASCII || (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return "", invalid("%s contains unsupported characters", name)
		}
	}
	return value, nil
}

func identifiers(values []string, name string) ([]string, error) {
	if len(values) > MaxFacetValues {
		return nil, invalid("too many %s values", name)
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = fold.String(value)
		canonical, err := identifier(value, name, MaxFacetBytes)
		if err != nil {
			return nil, err
		}
		out = append(out, canonical)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func extensions(values []string) ([]string, error) {
	if len(values) > MaxFacetValues {
		return nil, invalid("too many extension values")
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(value)), ".")
		canonical, err := identifier(value, "extension", 16)
		if err != nil || strings.ContainsAny(value, "_-") {
			return nil, invalid("invalid extension %q", value)
		}
		out = append(out, "."+canonical)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

func infoHash(value string, length int, version string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if len(value) != length {
		return "", invalid("%s infohash must be %d hexadecimal characters", version, length)
	}
	for _, c := range []byte(value) {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", invalid("%s infohash is not hexadecimal", version)
		}
	}
	return value, nil
}

func webURL(value, name string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > MaxURLBytes || !utf8.ValidString(value) {
		return "", invalid("%s is invalid or too long", name)
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", invalid("%s must be an HTTPS URL without credentials", name)
	}
	return u.String(), nil
}

func magnetURI(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if len(value) > MaxURLBytes || !utf8.ValidString(value) {
		return "", invalid("magnet URI is invalid or too long")
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "magnet" || u.RawQuery == "" {
		return "", invalid("magnet URI is malformed")
	}
	return u.String(), nil
}

func nonNegative(value *int, name string) (*int, error) {
	if value == nil {
		return nil, nil
	}
	if *value < 0 {
		return nil, invalid("%s must not be negative", name)
	}
	return clone(value), nil
}

func clone[T any](value *T) *T {
	copy := *value
	return &copy
}

func invalid(format string, values ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidRecord, fmt.Sprintf(format, values...))
}
