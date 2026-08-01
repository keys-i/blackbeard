package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/keys-i/blackbeard/src/internal/output"
	"github.com/keys-i/blackbeard/src/internal/provider"
	"github.com/keys-i/blackbeard/src/internal/torrent"
	"github.com/spf13/cobra"
)

const inspectTimeout = 15 * time.Second

func oneSource(_ *cobra.Command, args []string) error {
	if len(args) != 1 {
		return usageError(errors.New("inspect needs exactly one magnet URI, .torrent path, or HTTPS URL"))
	}
	return nil
}

func inspectSource(ctx context.Context, dst *output.Encoder, format, raw string, fetch torrent.Fetch) error {
	source, err := torrent.ParseSource(raw)
	if err != nil {
		return usageError(fmt.Errorf("parse torrent source: %w", err))
	}
	ctx, cancel := context.WithTimeout(ctx, inspectTimeout)
	defer cancel()
	inspection, err := torrent.Inspect(ctx, source, fetch)
	if err != nil {
		return fmt.Errorf("inspect torrent: %w", err)
	}
	return writeInspection(dst, format, inspection)
}

func writeInspection(dst *output.Encoder, format string, inspection torrent.Inspection) error {
	switch format {
	case outputTable:
		formatValue := optionalText(inspection.Format)
		if !inspection.MetadataAvailable {
			formatValue = "unknown (metadata unavailable)"
		}
		rows := [][]string{
			{"source_type", string(inspection.Source)},
			{"metadata_available", strconv.FormatBool(inspection.MetadataAvailable)},
			{"name", optionalText(inspection.Name)},
			{"format", formatValue},
			{"infohash_v1", optionalText(inspection.InfoHashV1)},
			{"infohash_v2", optionalText(inspection.InfoHashV2)},
			{"total_size", optionalBytes(inspection.TotalSize)},
			{"piece_length", optionalBytes(inspection.PieceLength)},
			{"private", optionalBool(inspection.Private)},
			{"comment", optionalText(inspection.Comment)},
			{"created_by", optionalText(inspection.CreatedBy)},
		}
		for i, tracker := range inspection.Trackers {
			rows = append(rows, []string{fmt.Sprintf("tracker[%d]", i+1), tracker})
		}
		for i, webseed := range inspection.Webseeds {
			rows = append(rows, []string{fmt.Sprintf("webseed[%d]", i+1), webseed})
		}
		for i, file := range inspection.Files {
			rows = append(rows, []string{fmt.Sprintf("file[%d]", i+1), fmt.Sprintf("%s (%d bytes)", file.Path, file.Size)})
		}
		return dst.Table(output.Table{Columns: []string{"FIELD", "VALUE"}, Rows: rows})
	case outputJSON:
		return dst.JSON("torrent_inspect", inspection)
	case outputNDJSON:
		files := inspection.Files
		inspection.Files = []torrent.File{}
		if err := dst.NDJSON("torrent", inspection); err != nil {
			return err
		}
		for i, file := range files {
			if err := dst.NDJSON("file", struct {
				Index int `json:"index"`
				torrent.File
			}{Index: i + 1, File: file}); err != nil {
				return err
			}
		}
		return dst.NDJSON("done", struct {
			Files     int    `json:"files"`
			TotalSize *int64 `json:"total_size,omitempty"`
		}{Files: len(files), TotalSize: inspection.TotalSize})
	default:
		return usageError(fmt.Errorf("invalid output format %q", format))
	}
}

func optionalText(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func optionalBool(value *bool) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatBool(*value)
}

func fetchTorrentHTTPS(ctx context.Context, source *url.URL, maxBytes int64) ([]byte, error) {
	if source == nil {
		return nil, errors.New("torrent URL is nil")
	}
	origin := (&url.URL{Scheme: source.Scheme, Host: source.Host}).String()
	client, err := provider.NewClient(origin)
	if err != nil {
		return nil, fmt.Errorf("create torrent HTTP client: %w", err)
	}
	path := source.EscapedPath()
	if path == "" {
		path = "/"
	}
	if source.RawQuery != "" {
		path += "?" + source.RawQuery
	}
	response, err := client.Fetch(ctx, path, provider.FetchOptions{
		MaxBody:             maxBytes,
		ValidateContentType: provider.ValidateMetainfoContentType,
	})
	if err != nil {
		return nil, err
	}
	return response.Body, nil
}
