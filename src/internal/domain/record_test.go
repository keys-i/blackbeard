package domain

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeRecord(t *testing.T) {
	t.Parallel()

	size := int64(42)
	seeders := 3
	date := time.Date(2025, 4, 3, 19, 20, 0, 0, time.FixedZone("test", 10*60*60))
	got, err := NormalizeRecord(Record{
		Provider:      " Internet_Archive ",
		SourceID:      " item\t42 ",
		Title:         "  Public\u202e  Domain\nFilm ",
		Description:   "A\x00 restored\tfilm",
		InfoHashV1:    strings.Repeat("A", 40),
		DetailsURL:    "https://archive.org/details/item42",
		MagnetURI:     "magnet:?xt=urn:btih:" + strings.Repeat("a", 40),
		TorrentURL:    "https://archive.org/download/item42/item42_archive.torrent",
		SizeBytes:     &size,
		PublishedAt:   &date,
		Categories:    []string{"Film", "film"},
		Extensions:    []string{"MKV", ".mkv"},
		Architectures: []string{},
		Resolutions:   []int{1080, 720, 1080},
		Seeders:       &seeders,
	})
	if err != nil {
		t.Fatalf("NormalizeRecord() error = %v", err)
	}
	if got.Provider != "internet_archive" || got.SourceID != "item 42" {
		t.Fatalf("identity = %q/%q", got.Provider, got.SourceID)
	}
	if got.Title != "Public Domain Film" || got.Description != "A restored film" {
		t.Fatalf("text = %q / %q", got.Title, got.Description)
	}
	if got.InfoHashV1 != strings.Repeat("a", 40) {
		t.Fatalf("v1 hash = %q", got.InfoHashV1)
	}
	if len(got.Categories) != 1 || got.Categories[0] != "film" || len(got.Extensions) != 1 || got.Extensions[0] != ".mkv" {
		t.Fatalf("facets = %#v / %#v", got.Categories, got.Extensions)
	}
	if len(got.Resolutions) != 2 || got.Resolutions[0] != 720 || got.Resolutions[1] != 1080 {
		t.Fatalf("resolutions = %#v", got.Resolutions)
	}
	if got.PublishedAt.Format(time.DateOnly) != "2025-04-03" || got.PublishedAt.Location() != time.UTC {
		t.Fatalf("published_at = %v", got.PublishedAt)
	}
	size = 99
	seeders = 99
	if *got.SizeBytes != 42 || *got.Seeders != 3 {
		t.Fatal("NormalizeRecord retained mutable input pointers")
	}
}

func TestNormalizeRecordRejectsHostileBounds(t *testing.T) {
	t.Parallel()

	valid := Record{Provider: "debian", SourceID: "debian-13", Title: "Debian 13"}
	tests := []struct {
		name   string
		mutate func(*Record)
	}{
		{"empty provider", func(r *Record) { r.Provider = "" }},
		{"oversized title", func(r *Record) { r.Title = strings.Repeat("x", MaxTitleBytes+1) }},
		{"invalid UTF-8", func(r *Record) { r.Description = string([]byte{0xff}) }},
		{"invalid v1", func(r *Record) { r.InfoHashV1 = strings.Repeat("g", 40) }},
		{"credentials in URL", func(r *Record) { r.TorrentURL = "https://token@example.test/file.torrent" }},
		{"non-HTTPS URL", func(r *Record) { r.DetailsURL = "http://example.test/item" }},
		{"negative size", func(r *Record) { n := int64(-1); r.SizeBytes = &n }},
		{"too many facets", func(r *Record) { r.Categories = make([]string, MaxFacetValues+1) }},
		{"bad resolution", func(r *Record) { r.Resolutions = []int{99} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			record := valid
			test.mutate(&record)
			_, err := NormalizeRecord(record)
			if !errors.Is(err, ErrInvalidRecord) {
				t.Fatalf("NormalizeRecord() error = %v", err)
			}
		})
	}
}

func TestNormalizeSearchTextPreservesDisplayAndFoldsUnicode(t *testing.T) {
	t.Parallel()

	const display = " Straße ＳＥＥ "
	if got := NormalizeSearchText(display); got != "strasse see" {
		t.Fatalf("NormalizeSearchText() = %q", got)
	}
	if display != " Straße ＳＥＥ " {
		t.Fatal("NormalizeSearchText changed its input")
	}
}

func FuzzNormalizeRecord(f *testing.F) {
	f.Add("archive", "id", "title", "description")
	f.Add("debian", "debian-13", "Debian\u202e", "\x00")
	f.Fuzz(func(t *testing.T, provider, sourceID, title, description string) {
		_, _ = NormalizeRecord(Record{Provider: provider, SourceID: sourceID, Title: title, Description: description})
	})
}
