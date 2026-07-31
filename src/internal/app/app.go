package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/keys-i/blackbeard/src/internal/output"
	"github.com/keys-i/blackbeard/src/internal/query"
	"github.com/spf13/cobra"
)

const (
	outputTable  = "table"
	outputJSON   = "json"
	outputNDJSON = "ndjson"
)

// Run executes Blackbeard without owning process exit or its standard streams.
func Run(ctx context.Context, args []string, _ io.Reader, stdout, stderr io.Writer, version string) error {
	var format string
	var explain bool
	var showVersion bool
	var helpErr error

	root := &cobra.Command{
		Use:           "blackbeard",
		Short:         "Chart lawful torrents from the terminal",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          noArgs,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			return validateOutputFormat(format)
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if showVersion {
				return writeVersion(cmd.OutOrStdout(), format, version)
			}
			return cmd.Help()
		},
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return usageError(err)
	})
	root.Flags().BoolVar(&showVersion, "version", false, "print the Blackbeard version")
	root.PersistentFlags().StringVar(&format, "output", outputTable, "output format: table, json, or ndjson")
	plainHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if err := validateOutputFormat(format); err != nil {
			helpErr = err
			return
		}
		var rendered bytes.Buffer
		destination := cmd.OutOrStdout()
		cmd.SetOut(&rendered)
		plainHelp(cmd, args)
		cmd.SetOut(destination)
		if format == outputTable {
			_, helpErr = io.WriteString(destination, rendered.String())
			return
		}
		helpErr = writeHelp(destination, format, cmd.CommandPath(), rendered.String())
	})

	root.AddCommand(&cobra.Command{
		Use:   "version",
		Short: "Print the Blackbeard version",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeVersion(cmd.OutOrStdout(), format, version)
		},
	})

	search := &cobra.Command{
		Use:   "search <query>",
		Short: "Search configured open catalogues",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageError(errors.New("search needs a query"))
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ast, err := query.Parse(strings.Join(args, " "))
			if err != nil {
				return usageError(fmt.Errorf("parse query: %w", err))
			}
			if !explain {
				return errors.New("catalogue execution is not available in this build; use search --explain")
			}
			return writeExplanation(cmd.OutOrStdout(), format, ast)
		},
	}
	search.Flags().BoolVar(&explain, "explain", false, "show the deterministic query interpretation")
	search.Flags().SetInterspersed(false)
	root.AddCommand(search)

	if err := root.ExecuteContext(ctx); err != nil {
		return err
	}
	return helpErr
}

func validateOutputFormat(format string) error {
	if format != outputTable && format != outputJSON && format != outputNDJSON {
		return usageError(fmt.Errorf("invalid output format %q", format))
	}
	return nil
}

func writeHelp(dst io.Writer, format, command, text string) error {
	data := struct {
		Command string `json:"command"`
		Text    string `json:"text"`
	}{Command: command, Text: text}
	encoder := output.NewEncoder(dst)
	if format == outputJSON {
		return encoder.JSON("help", data)
	}
	return encoder.NDJSON("help", data)
}

func writeVersion(dst io.Writer, format, version string) error {
	if format == outputTable {
		_, err := fmt.Fprintln(dst, version)
		return err
	}
	data := struct {
		Version string `json:"version"`
	}{Version: version}
	encoder := output.NewEncoder(dst)
	if format == outputJSON {
		return encoder.JSON("version", data)
	}
	return encoder.NDJSON("version", data)
}

func writeExplanation(dst io.Writer, format string, ast query.AST) error {
	encoder := output.NewEncoder(dst)
	switch format {
	case outputTable:
		return encoder.Table(explanationTable(ast))
	case outputJSON:
		return encoder.JSON("query_explain", ast)
	case outputNDJSON:
		if err := encoder.NDJSON("query", ast); err != nil {
			return err
		}
		return encoder.NDJSON("done", struct {
			Warnings int `json:"warnings"`
		}{Warnings: len(ast.Warnings)})
	default:
		return usageError(fmt.Errorf("invalid output format %q", format))
	}
}

func explanationTable(ast query.AST) output.Table {
	rows := [][]string{{"raw", ast.Raw}}
	add := func(name, value string) {
		if value != "" {
			rows = append(rows, []string{name, value})
		}
	}
	add("required", textValues(ast.Required))
	add("optional", textValues(ast.Optional))
	add("excluded", textValues(ast.Excluded))
	add("phrases", phraseValues(ast.Phrases))
	add("categories", clauseValues(ast.Categories))
	add("kinds", clauseValues(ast.ContentKinds))
	add("extensions", clauseValues(ast.Extensions))
	add("media", clauseValues(ast.MediaKinds))
	add("dates", dateValues(ast.Date))
	add("languages", clauseValues(ast.Languages))
	add("architectures", clauseValues(ast.Architectures))
	add("resolutions", resolutionValues(ast.Resolutions))
	add("codecs", clauseValues(ast.Codecs))
	add("size", sizeValues(ast.Size))
	add("providers", providerValues(ast.Providers))
	add("order", orderValues(ast.Order))
	if ast.Limit != nil {
		add("limit", strconv.Itoa(*ast.Limit))
	}
	for _, warning := range ast.Warnings {
		add("warning", warning.Code+": "+warning.Message)
	}
	return output.Table{Columns: []string{"FIELD", "VALUE"}, Rows: rows}
}

func textValues(values []query.TextClause) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = value.Normalized
	}
	return strings.Join(out, ", ")
}

func clauseValues(values []query.ValueClause) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = value.Value
	}
	return strings.Join(out, ", ")
}

func phraseValues(values []query.PhraseClause) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = value.Occurrence + ":" + strconv.Quote(value.Normalized)
	}
	return strings.Join(out, ", ")
}

func resolutionValues(values []query.Resolution) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = value.Label
	}
	return strings.Join(out, ", ")
}

func sizeValues(value query.ByteRange) string {
	var bounds []string
	if value.Min != nil {
		operator := ">="
		if !value.Min.Inclusive {
			operator = ">"
		}
		bounds = append(bounds, operator+strconv.FormatInt(value.Min.Bytes, 10)+" B")
	}
	if value.Max != nil {
		operator := "<="
		if !value.Max.Inclusive {
			operator = "<"
		}
		bounds = append(bounds, operator+strconv.FormatInt(value.Max.Bytes, 10)+" B")
	}
	return strings.Join(bounds, ", ")
}

func dateValues(value query.DateRange) string {
	var bounds []string
	if value.Start != nil {
		operator := ">="
		if !value.Start.Inclusive {
			operator = ">"
		}
		bounds = append(bounds, operator+value.Start.Date)
	}
	if value.End != nil {
		operator := "<="
		if !value.End.Inclusive {
			operator = "<"
		}
		bounds = append(bounds, operator+value.End.Date)
	}
	return strings.Join(bounds, ", ")
}

func providerValues(value query.ProviderSelection) string {
	var parts []string
	if allowed := clauseValues(value.Allow); allowed != "" {
		parts = append(parts, "allow:"+allowed)
	}
	if denied := clauseValues(value.Deny); denied != "" {
		parts = append(parts, "deny:"+denied)
	}
	return strings.Join(parts, ", ")
}

func orderValues(values []query.OrderClause) string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = value.Field + ":" + value.Direction
	}
	return strings.Join(out, ", ")
}

type codedError struct {
	code int
	err  error
}

func (e codedError) Error() string { return e.err.Error() }
func (e codedError) Unwrap() error { return e.err }

func usageError(err error) error {
	return codedError{code: 2, err: err}
}

func noArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.NoArgs(cmd, args); err != nil {
		return usageError(err)
	}
	return nil
}

// ExitCode maps returned errors to Blackbeard's documented process status.
func ExitCode(err error) int {
	var coded codedError
	if errors.As(err, &coded) {
		return coded.code
	}
	return 1
}
