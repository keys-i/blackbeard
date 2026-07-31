package query

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestSpecificationExamples(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		check func(*testing.T, AST)
	}{
		{
			name:  "Linux image",
			input: "arm64 Linux image under 2 GiB newest first",
			check: func(t *testing.T, got AST) {
				assertValues(t, got.Architectures, "arm64")
				assertValues(t, got.ContentKinds, "disk_image")
				assertTerms(t, got.Required, "linux")
				assertByteBound(t, got.Size.Max, 2<<30, false)
				assertOrder(t, got.Order, OrderClause{Field: "date", Direction: "desc"})
			},
		},
		{
			name:  "recent academic dataset",
			input: "machine-learning climate dataset after 2023 academic sources only",
			check: func(t *testing.T, got AST) {
				assertTerms(t, got.Required, "machine-learning", "climate")
				assertValues(t, got.ContentKinds, "dataset")
				assertDateBound(t, got.Date.Start, "2024-01-01", true)
				assertValues(t, got.Providers.Allow, "academic_torrents")
			},
		},
		{
			name:  "archive animation phrase",
			input: `"public domain" animation under 4 GiB from archive`,
			check: func(t *testing.T, got AST) {
				if len(got.Phrases) != 1 || got.Phrases[0].Normalized != "public domain" || got.Phrases[0].Occurrence != "required" {
					t.Fatalf("phrases = %#v", got.Phrases)
				}
				if got.Phrases[0].StartByte != 0 || got.Phrases[0].EndByte != len(`"public domain"`) {
					t.Fatalf("phrase span = %#v", got.Phrases[0].Span)
				}
				assertValues(t, got.ContentKinds, "animation")
				assertByteBound(t, got.Size.Max, 4<<30, false)
				assertValues(t, got.Providers.Allow, "internet_archive")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(test.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(got.Warnings) != 0 {
				t.Fatalf("warnings = %#v", got.Warnings)
			}
			test.check(t, got)
		})
	}
}

func TestExplicitFiltersAndOccurrences(t *testing.T) {
	t.Parallel()

	input := `+linux ?desktop -server -"beta build" category:software kind:disk-image ext:mkv media:video date:2020..2023 lang:en-US arch:x86_64 resolution:4k codec:HEVC provider:archive -provider:debian sort:smallest limit:25 size:>=1.5GiB`
	got, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	assertTerms(t, got.Required, "linux")
	assertTerms(t, got.Optional, "desktop")
	assertTerms(t, got.Excluded, "server")
	if len(got.Phrases) != 1 || got.Phrases[0].Normalized != "beta build" || got.Phrases[0].Occurrence != "excluded" {
		t.Fatalf("phrases = %#v", got.Phrases)
	}
	assertValues(t, got.Categories, "software")
	assertValues(t, got.ContentKinds, "disk_image")
	assertValues(t, got.Extensions, ".mkv")
	assertValues(t, got.MediaKinds, "video")
	assertDateBound(t, got.Date.Start, "2020-01-01", true)
	assertDateBound(t, got.Date.End, "2023-12-31", true)
	assertValues(t, got.Languages, "en-us")
	assertValues(t, got.Architectures, "amd64")
	if len(got.Resolutions) != 1 || got.Resolutions[0].Vertical != 2160 || got.Resolutions[0].Label != "2160p" {
		t.Fatalf("resolutions = %#v", got.Resolutions)
	}
	assertValues(t, got.Codecs, "h265")
	assertValues(t, got.Providers.Allow, "internet_archive")
	assertValues(t, got.Providers.Deny, "debian")
	assertOrder(t, got.Order, OrderClause{Field: "size", Direction: "asc"})
	if got.Limit == nil || *got.Limit != 25 {
		t.Fatalf("limit = %v", got.Limit)
	}
	assertByteBound(t, got.Size.Min, 1_610_612_736, true)
	if len(got.Warnings) != 0 {
		t.Fatalf("warnings = %#v", got.Warnings)
	}
}

func TestExactByteSizes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantMin   int64
		wantMax   int64
		inclusive bool
	}{
		{name: "fractional IEC", input: "size:<1.5KiB", wantMax: 1536},
		{name: "fractional SI", input: "max-size:1.25KB", wantMax: 1250, inclusive: true},
		{name: "half GiB", input: "min-size:0.5GiB", wantMin: 536_870_912, inclusive: true},
		{name: "large IEC", input: "max-size:7EiB", wantMax: 8_070_450_532_247_928_832, inclusive: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(test.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if test.wantMin != 0 {
				assertByteBound(t, got.Size.Min, test.wantMin, test.inclusive)
			}
			if test.wantMax != 0 {
				assertByteBound(t, got.Size.Max, test.wantMax, test.inclusive)
			}
		})
	}

	t.Run("natural range", func(t *testing.T) {
		t.Parallel()
		got, err := Parse("between 1 GB and 2 GiB")
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		assertByteBound(t, got.Size.Min, 1_000_000_000, true)
		assertByteBound(t, got.Size.Max, 2_147_483_648, true)
		if len(got.Required) != 0 {
			t.Fatalf("required = %#v", got.Required)
		}
	})

	for _, input := range []string{
		"size:<0.1KiB",
		"size:<8EiB",
		"size:<99999999999999999999999999999999999999999TiB",
	} {
		t.Run(input, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			assertWarningCodes(t, got.Warnings, "invalid_size")
			assertTerms(t, got.Required, strings.ToLower(input))
		})
	}
}

func TestDateBoundsAndContradictions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input          string
		wantStart      string
		startInclusive bool
		wantEnd        string
		endInclusive   bool
	}{
		{input: "after:2023", wantStart: "2024-01-01", startInclusive: true},
		{input: "since:2023-06-15", wantStart: "2023-06-15", startInclusive: true},
		{input: "after:2023-06-15", wantStart: "2023-06-15"},
		{input: "before:2023-06-15", wantEnd: "2023-06-15"},
		{input: "year:2020", wantStart: "2020-01-01", startInclusive: true, wantEnd: "2020-12-31", endInclusive: true},
	}
	for _, test := range tests {
		t.Run(test.input, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(test.input)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if test.wantStart != "" {
				assertDateBound(t, got.Date.Start, test.wantStart, test.startInclusive)
			}
			if test.wantEnd != "" {
				assertDateBound(t, got.Date.End, test.wantEnd, test.endInclusive)
			}
		})
	}

	for _, input := range []string{
		"size:>2GiB size:<1GiB",
		"after:2023 before:2024",
	} {
		t.Run("conflict "+input, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(input)
			if !errors.Is(err, ErrUnsatisfiable) {
				t.Fatalf("Parse() error = %v, want %v", err, ErrUnsatisfiable)
			}
			assertWarningCodes(t, got.Warnings, "range_conflict")
		})
	}
}

func TestByteOffsetsUseOriginalUTF8(t *testing.T) {
	t.Parallel()

	input := `α ?β -γ +"quoted phrase" ?"δelta phrase"`
	got, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	assertTerms(t, got.Required, "α")
	assertTerms(t, got.Optional, "β")
	assertTerms(t, got.Excluded, "γ")
	if input[got.Required[0].StartByte:got.Required[0].EndByte] != "α" {
		t.Fatalf("required span selects %q", input[got.Required[0].StartByte:got.Required[0].EndByte])
	}
	if input[got.Optional[0].StartByte:got.Optional[0].EndByte] != "?β" {
		t.Fatalf("optional span selects %q", input[got.Optional[0].StartByte:got.Optional[0].EndByte])
	}
	if input[got.Excluded[0].StartByte:got.Excluded[0].EndByte] != "-γ" {
		t.Fatalf("excluded span selects %q", input[got.Excluded[0].StartByte:got.Excluded[0].EndByte])
	}
	if len(got.Phrases) != 2 {
		t.Fatalf("phrases = %#v", got.Phrases)
	}
	if got.Phrases[0].Occurrence != "required" || input[got.Phrases[0].StartByte:got.Phrases[0].EndByte] != `+"quoted phrase"` {
		t.Fatalf("required phrase = %#v", got.Phrases[0])
	}
	if got.Phrases[1].Occurrence != "optional" || input[got.Phrases[1].StartByte:got.Phrases[1].EndByte] != `?"δelta phrase"` {
		t.Fatalf("optional phrase = %#v", got.Phrases[1])
	}
}

func TestUnicodeNormalizationAndCaseFolding(t *testing.T) {
	t.Parallel()

	got, err := Parse("ＡＲＭ６４ Straße e\u0301 ＳＩＺＥ:<1GiB \"Straße SEE\"")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	assertValues(t, got.Architectures, "arm64")
	assertTerms(t, got.Required, "strasse", "é")
	if len(got.Phrases) != 1 || got.Phrases[0].Normalized != "strasse see" {
		t.Fatalf("phrases = %#v", got.Phrases)
	}
	assertByteBound(t, got.Size.Max, 1<<30, false)
}

func TestUnknownFiltersRemainLexical(t *testing.T) {
	t.Parallel()

	got, err := Parse("frobnicate source:unknown codec:madeup lang:not_a_tag")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	assertTerms(t, got.Required, "frobnicate", "source:unknown", "codec:madeup", "lang:not_a_tag")
	assertWarningCodes(t, got.Warnings, "unknown_provider", "invalid_filter", "invalid_filter")
}

func TestOptionalAndExcludedFacetsRemainText(t *testing.T) {
	t.Parallel()

	got, err := Parse("-codec:h265 ?kind:video ?provider:archive")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	assertValues(t, got.Codecs)
	assertValues(t, got.ContentKinds)
	assertValues(t, got.Providers.Allow)
	assertTerms(t, got.Excluded, "codec:h265")
	assertTerms(t, got.Optional, "kind:video", "provider:archive")
}

func TestAmbiguousTextRemainsLexicalWithWarnings(t *testing.T) {
	t.Parallel()

	got, err := Parse("4K 03/04/2025")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	assertTerms(t, got.Required, "4k", "03/04/2025")
	assertWarningCodes(t, got.Warnings, "ambiguous_resolution", "ambiguous_date")
}

func TestExcludedOrQuotedVideoDoesNotImplyResolution(t *testing.T) {
	t.Parallel()

	for _, input := range []string{"-video 4k", `-"video" 4k`, `?video 4k`} {
		got, err := Parse(input)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", input, err)
		}
		if len(got.Resolutions) != 0 {
			t.Fatalf("Parse(%q) resolutions = %#v", input, got.Resolutions)
		}
		assertWarningCodes(t, got.Warnings, "ambiguous_resolution")
	}
}

func TestProviderAndOrderConflictsAreDeterministic(t *testing.T) {
	t.Parallel()

	got, err := Parse("provider:archive -provider:archive provider:archive sort:newest sort:oldest sort:smallest")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	assertValues(t, got.Providers.Allow)
	assertValues(t, got.Providers.Deny, "internet_archive")
	assertOrder(t, got.Order,
		OrderClause{Field: "date", Direction: "desc"},
		OrderClause{Field: "size", Direction: "asc"},
	)
	assertWarningCodes(t, got.Warnings, "provider_conflict", "provider_conflict", "order_conflict")
}

func TestStableJSONAndNonNilSlices(t *testing.T) {
	t.Parallel()

	first, err := Parse("provider:debian limit:4")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	second, err := Parse("provider:debian limit:4")
	if err != nil {
		t.Fatalf("second Parse() error = %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated parse differs:\nfirst  = %#v\nsecond = %#v", first, second)
	}
	one, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	two, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("second json.Marshal() error = %v", err)
	}
	if string(one) != string(two) {
		t.Fatalf("JSON differs:\n%s\n%s", one, two)
	}
	for _, field := range []string{`"required":[]`, `"optional":[]`, `"excluded":[]`, `"phrases":[]`, `"warnings":[]`} {
		if !strings.Contains(string(one), field) {
			t.Fatalf("JSON %s does not contain %s", one, field)
		}
	}
}

func TestInputLimits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  error
	}{
		{name: "empty", input: " \t\n", want: ErrEmpty},
		{name: "too long", input: strings.Repeat("a", MaxInputBytes+1), want: ErrTooLong},
		{name: "invalid UTF-8", input: string([]byte{0xff}), want: ErrInvalidUTF8},
		{name: "too many tokens", input: strings.Repeat("x ", MaxTokens+1), want: ErrTooManyTokens},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(test.input)
			if !errors.Is(err, test.want) {
				t.Fatalf("Parse() error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestUnclosedQuoteWarnsButParses(t *testing.T) {
	t.Parallel()

	got, err := Parse(`alpha "unfinished phrase`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	assertTerms(t, got.Required, "alpha")
	if len(got.Phrases) != 1 || got.Phrases[0].Normalized != "unfinished phrase" {
		t.Fatalf("phrases = %#v", got.Phrases)
	}
	assertWarningCodes(t, got.Warnings, "unclosed_quote")
}

func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"arm64 Linux image under 2 GiB newest first",
		`?alpha -beta "quoted phrase"`,
		"size:>1.5GiB date:2020..2024 provider:archive",
		"αβγ \x1b[31m",
		string([]byte{0xff}),
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, input string) {
		first, firstErr := Parse(input)
		second, secondErr := Parse(input)
		if errorText(firstErr) != errorText(secondErr) {
			t.Fatalf("errors differ: %q != %q", errorText(firstErr), errorText(secondErr))
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("AST differs for repeated parse")
		}
		if _, err := json.Marshal(first); err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		validateSpans(t, first)
	})
}

func BenchmarkParseStructured(b *testing.B) {
	input := `+linux ?desktop -server "public domain" category:software kind:disk-image ext:mkv media:video date:2020..2023 lang:en-US arch:x86_64 resolution:4k codec:HEVC provider:archive -provider:debian sort:newest limit:50 size:>=1.5GiB`
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(input); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseLexical(b *testing.B) {
	input := "public domain climate machine-learning dataset animation archive collection"
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Parse(input); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTokenize(b *testing.B) {
	input := `+linux ?desktop -server "public domain" under 2 GiB newest first`
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := lex(input); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseByteQuantity(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if value, ok := parseByteQuantity("1.5GiB"); !ok || value != 1_610_612_736 {
			b.Fatalf("parseByteQuantity() = %d, %t", value, ok)
		}
	}
}

func assertTerms(t *testing.T, got []TextClause, want ...string) {
	t.Helper()
	values := make([]string, len(got))
	for i := range got {
		values[i] = got[i].Normalized
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("terms = %#v, want %#v", values, want)
	}
}

func assertValues(t *testing.T, got []ValueClause, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	values := make([]string, len(got))
	for i := range got {
		values[i] = got[i].Value
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %#v, want %#v", values, want)
	}
}

func assertByteBound(t *testing.T, got *ByteBound, want int64, inclusive bool) {
	t.Helper()
	if got == nil || got.Bytes != want || got.Inclusive != inclusive {
		t.Fatalf("byte bound = %#v, want bytes=%d inclusive=%t", got, want, inclusive)
	}
}

func assertDateBound(t *testing.T, got *DateBound, want string, inclusive bool) {
	t.Helper()
	if got == nil || got.Date != want || got.Inclusive != inclusive {
		t.Fatalf("date bound = %#v, want date=%s inclusive=%t", got, want, inclusive)
	}
}

func assertOrder(t *testing.T, got []OrderClause, want ...OrderClause) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %#v, want %#v", got, want)
	}
	for i := range got {
		if got[i].Field != want[i].Field || got[i].Direction != want[i].Direction {
			t.Fatalf("order = %#v, want %#v", got, want)
		}
	}
}

func assertWarningCodes(t *testing.T, got []Warning, want ...string) {
	t.Helper()
	codes := make([]string, len(got))
	for i := range got {
		codes[i] = got[i].Code
	}
	if !reflect.DeepEqual(codes, want) {
		t.Fatalf("warning codes = %#v, want %#v", codes, want)
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func validateSpans(t *testing.T, ast AST) {
	t.Helper()
	check := func(span Span) {
		if span.StartByte < 0 || span.EndByte < span.StartByte || span.EndByte > len(ast.Raw) {
			t.Fatalf("invalid span %#v for %d-byte input", span, len(ast.Raw))
		}
	}
	for _, values := range [][]TextClause{ast.Required, ast.Optional, ast.Excluded} {
		for _, value := range values {
			check(value.Span)
		}
	}
	for _, value := range ast.Phrases {
		check(value.Span)
	}
	for _, values := range [][]ValueClause{
		ast.Categories,
		ast.ContentKinds,
		ast.Extensions,
		ast.MediaKinds,
		ast.Languages,
		ast.Architectures,
		ast.Codecs,
		ast.Providers.Allow,
		ast.Providers.Deny,
	} {
		for _, value := range values {
			check(value.Span)
		}
	}
	for _, value := range ast.Resolutions {
		check(value.Span)
	}
	for _, value := range ast.Order {
		check(value.Span)
	}
	for _, value := range ast.Warnings {
		check(value.Span)
	}
	for _, value := range []*ByteBound{ast.Size.Min, ast.Size.Max} {
		if value != nil {
			check(value.Span)
		}
	}
	for _, value := range []*DateBound{ast.Date.Start, ast.Date.End} {
		if value != nil {
			check(value.Span)
		}
	}
}
