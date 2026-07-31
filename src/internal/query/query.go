// Package query parses Blackbeard's deterministic search language.
package query

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	SchemaVersion = 1
	MaxInputBytes = 4096
	MaxTokens     = 256
	MaxLimit      = 1000
)

var (
	ErrEmpty         = errors.New("query is empty")
	ErrTooLong       = errors.New("query exceeds 4096 bytes")
	ErrTooManyTokens = errors.New("query exceeds 256 tokens")
	ErrInvalidUTF8   = errors.New("query is not valid UTF-8")
	ErrUnsatisfiable = errors.New("query constraints are contradictory")
)

type AST struct {
	SchemaVersion int               `json:"schema_version"`
	Raw           string            `json:"raw"`
	Required      []TextClause      `json:"required"`
	Optional      []TextClause      `json:"optional"`
	Excluded      []TextClause      `json:"excluded"`
	Phrases       []PhraseClause    `json:"phrases"`
	Categories    []ValueClause     `json:"categories"`
	ContentKinds  []ValueClause     `json:"content_kinds"`
	Extensions    []ValueClause     `json:"extensions"`
	MediaKinds    []ValueClause     `json:"media_kinds"`
	Date          DateRange         `json:"date"`
	Languages     []ValueClause     `json:"languages"`
	Architectures []ValueClause     `json:"architectures"`
	Resolutions   []Resolution      `json:"resolutions"`
	Codecs        []ValueClause     `json:"codecs"`
	Size          ByteRange         `json:"size"`
	Providers     ProviderSelection `json:"providers"`
	Order         []OrderClause     `json:"order"`
	Limit         *int              `json:"limit,omitempty"`
	Warnings      []Warning         `json:"warnings"`
}

type Span struct {
	StartByte int `json:"start_byte"`
	EndByte   int `json:"end_byte"`
}

type TextClause struct {
	Raw        string `json:"raw"`
	Normalized string `json:"normalized"`
	Span
}

type PhraseClause struct {
	TextClause
	Occurrence string `json:"occurrence"`
}

type ValueClause struct {
	Value string `json:"value"`
	Span
}

type Resolution struct {
	Vertical int    `json:"vertical"`
	Label    string `json:"label"`
	Span
}

type ByteRange struct {
	Min *ByteBound `json:"min,omitempty"`
	Max *ByteBound `json:"max,omitempty"`
}

type ByteBound struct {
	Bytes     int64  `json:"bytes"`
	Inclusive bool   `json:"inclusive"`
	Lexeme    string `json:"lexeme"`
	Span
}

type DateRange struct {
	Start *DateBound `json:"start,omitempty"`
	End   *DateBound `json:"end,omitempty"`
}

type DateBound struct {
	Date      string `json:"date"`
	Inclusive bool   `json:"inclusive"`
	Span
}

type ProviderSelection struct {
	Allow []ValueClause `json:"allow"`
	Deny  []ValueClause `json:"deny"`
}

type OrderClause struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
	Span
}

type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Span
}

type occurrence uint8

const (
	required occurrence = iota
	optional
	excluded
)

func (o occurrence) String() string {
	switch o {
	case optional:
		return "optional"
	case excluded:
		return "excluded"
	default:
		return "required"
	}
}

type token struct {
	text       string
	span       Span
	quoted     bool
	occurrence occurrence
}

func Parse(input string) (AST, error) {
	ast := newAST(input)
	if strings.TrimSpace(input) == "" {
		return ast, ErrEmpty
	}
	if len(input) > MaxInputBytes {
		return ast, ErrTooLong
	}
	if !utf8.ValidString(input) {
		return ast, ErrInvalidUTF8
	}

	tokens, warnings, err := lex(input)
	ast.Warnings = append(ast.Warnings, warnings...)
	if err != nil {
		return ast, err
	}
	used := make([]bool, len(tokens))

	for i := range tokens {
		if used[i] || tokens[i].quoted {
			continue
		}
		if parseExplicit(&ast, tokens, used, i) ||
			parseNaturalSize(&ast, tokens, used, i) ||
			parseNaturalDate(&ast, tokens, used, i) ||
			parseNaturalProvider(&ast, tokens, used, i) ||
			parseNaturalOrder(&ast, tokens, used, i) ||
			parseNaturalLimit(&ast, tokens, used, i) ||
			parseNaturalLanguage(&ast, tokens, used, i) ||
			parseNaturalFacet(&ast, tokens, used, i) {
			continue
		}
	}

	for i, tok := range tokens {
		if used[i] {
			continue
		}
		clause := TextClause{Raw: tok.text, Normalized: normalize(tok.text), Span: tok.span}
		if tok.quoted {
			if clause.Normalized == "" {
				ast.warn("empty_phrase", "empty quoted phrase ignored", tok.span)
				continue
			}
			ast.Phrases = append(ast.Phrases, PhraseClause{TextClause: clause, Occurrence: tok.occurrence.String()})
			continue
		}
		if clause.Normalized == "" {
			continue
		}
		switch {
		case clause.Normalized == "4k" || clause.Normalized == "8k":
			ast.warn("ambiguous_resolution", "resolution shorthand needs video context or an explicit resolution: filter", tok.span)
		case looksLikeLocaleDate(clause.Normalized):
			ast.warn("ambiguous_date", "use an ISO date such as 2025-04-03 with an explicit date filter", tok.span)
		}
		switch tok.occurrence {
		case optional:
			ast.Optional = append(ast.Optional, clause)
		case excluded:
			ast.Excluded = append(ast.Excluded, clause)
		default:
			ast.Required = append(ast.Required, clause)
		}
	}

	if contradictoryBytes(ast.Size) || contradictoryDates(ast.Date) {
		ast.warn("range_conflict", "query constraints have no possible match", Span{0, len(input)})
		return ast, ErrUnsatisfiable
	}
	return ast, nil
}

func newAST(raw string) AST {
	return AST{
		SchemaVersion: SchemaVersion,
		Raw:           raw,
		Required:      []TextClause{},
		Optional:      []TextClause{},
		Excluded:      []TextClause{},
		Phrases:       []PhraseClause{},
		Categories:    []ValueClause{},
		ContentKinds:  []ValueClause{},
		Extensions:    []ValueClause{},
		MediaKinds:    []ValueClause{},
		Languages:     []ValueClause{},
		Architectures: []ValueClause{},
		Resolutions:   []Resolution{},
		Codecs:        []ValueClause{},
		Providers: ProviderSelection{
			Allow: []ValueClause{},
			Deny:  []ValueClause{},
		},
		Order:    []OrderClause{},
		Warnings: []Warning{},
	}
}

func lex(input string) ([]token, []Warning, error) {
	tokens := make([]token, 0, min(16, MaxTokens))
	warnings := []Warning{}
	for i := 0; i < len(input); {
		for i < len(input) {
			r, size := utf8.DecodeRuneInString(input[i:])
			if !unicode.IsSpace(r) {
				break
			}
			i += size
		}
		if i == len(input) {
			break
		}

		start := i
		occ := required
		if i+1 < len(input) && !isSpaceAt(input, i+1) {
			switch input[i] {
			case '+':
				i++
			case '?':
				occ = optional
				i++
			case '-':
				occ = excluded
				i++
			}
		}

		if i < len(input) && input[i] == '"' {
			i++
			var text strings.Builder
			closed := false
			for i < len(input) {
				if input[i] == '"' {
					i++
					closed = true
					break
				}
				if input[i] == '\\' && i+1 < len(input) && (input[i+1] == '"' || input[i+1] == '\\') {
					text.WriteByte(input[i+1])
					i += 2
					continue
				}
				r, size := utf8.DecodeRuneInString(input[i:])
				text.WriteRune(r)
				i += size
			}
			span := Span{start, i}
			tokens = append(tokens, token{text: text.String(), span: span, quoted: true, occurrence: occ})
			if !closed {
				warnings = append(warnings, Warning{Code: "unclosed_quote", Message: "quoted phrase continues to end of input", Span: span})
			}
		} else {
			textStart := i
			for i < len(input) && !isSpaceAt(input, i) {
				_, size := utf8.DecodeRuneInString(input[i:])
				i += size
			}
			tokens = append(tokens, token{text: input[textStart:i], span: Span{start, i}, occurrence: occ})
		}
		if len(tokens) > MaxTokens {
			return tokens, warnings, ErrTooManyTokens
		}
	}
	return tokens, warnings, nil
}

func isSpaceAt(input string, at int) bool {
	r, _ := utf8.DecodeRuneInString(input[at:])
	return unicode.IsSpace(r)
}

func parseExplicit(ast *AST, tokens []token, used []bool, i int) bool {
	tok := tokens[i]
	key, value, ok := strings.Cut(tok.text, ":")
	if !ok || key == "" {
		return false
	}
	key = normalize(key)
	if tok.occurrence == optional || tok.occurrence == excluded && key != "provider" && key != "source" {
		return false
	}
	span := tok.span
	switch key {
	case "category":
		return addExplicitValue(ast, &ast.Categories, value, span, used, i, "category")
	case "kind", "type":
		canonical, ok := contentKinds[normalize(value)]
		if !ok {
			ast.warn("invalid_filter", fmt.Sprintf("unknown content kind %q", value), span)
			return false
		}
		addValue(&ast.ContentKinds, canonical, span)
		used[i] = true
		return true
	case "ext", "extension":
		extension, ok := parseExtension(value)
		if !ok {
			ast.warn("invalid_filter", fmt.Sprintf("invalid extension %q", value), span)
			return false
		}
		addValue(&ast.Extensions, extension, span)
		used[i] = true
		return true
	case "media":
		media, ok := mediaKinds[normalize(value)]
		if !ok {
			ast.warn("invalid_filter", fmt.Sprintf("unknown media kind %q", value), span)
			return false
		}
		addValue(&ast.MediaKinds, media, span)
		used[i] = true
		return true
	case "lang", "language":
		language, ok := parseLanguage(value)
		if !ok {
			ast.warn("invalid_filter", fmt.Sprintf("invalid language %q", value), span)
			return false
		}
		addValue(&ast.Languages, language, span)
		used[i] = true
		return true
	case "arch", "architecture":
		architecture, ok := architectures[normalize(value)]
		if !ok {
			ast.warn("invalid_filter", fmt.Sprintf("unknown architecture %q", value), span)
			return false
		}
		addValue(&ast.Architectures, architecture, span)
		used[i] = true
		return true
	case "resolution", "res":
		resolution, ok := parseResolution(value, span, true)
		if !ok {
			ast.warn("invalid_filter", fmt.Sprintf("invalid resolution %q", value), span)
			return false
		}
		addResolution(&ast.Resolutions, resolution)
		used[i] = true
		return true
	case "codec":
		codec, ok := codecs[compact(value)]
		if !ok {
			ast.warn("invalid_filter", fmt.Sprintf("unknown codec %q", value), span)
			return false
		}
		addValue(&ast.Codecs, codec, span)
		used[i] = true
		return true
	case "provider", "source":
		deny := tok.occurrence == excluded || strings.HasPrefix(value, "-") || strings.HasPrefix(value, "!")
		value = strings.TrimLeft(value, "-!")
		provider, ok := providers[normalize(value)]
		if !ok {
			ast.warn("unknown_provider", fmt.Sprintf("unknown provider %q", value), span)
			return false
		}
		ast.addProvider(provider, deny, span)
		used[i] = true
		return true
	case "sort", "order":
		field, direction, ok := parseOrder(value)
		if !ok {
			ast.warn("invalid_filter", fmt.Sprintf("unknown ordering %q", value), span)
			return false
		}
		ast.addOrder(field, direction, span)
		used[i] = true
		return true
	case "limit":
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > MaxLimit {
			ast.warn("invalid_limit", fmt.Sprintf("limit must be between 1 and %d", MaxLimit), span)
			return false
		}
		ast.Limit = &limit
		used[i] = true
		return true
	case "year", "date":
		start, end, ok := parseDateRangeValue(value, span)
		if !ok {
			ast.warn("invalid_date", fmt.Sprintf("invalid date or year range %q", value), span)
			return false
		}
		ast.mergeDate(start, end)
		used[i] = true
		return true
	case "after", "since", "before":
		start, end, ok := directionalDate(key, value, span)
		if !ok {
			ast.warn("invalid_date", fmt.Sprintf("invalid date %q", value), span)
			return false
		}
		ast.mergeDate(start, end)
		used[i] = true
		return true
	case "size", "min-size", "max-size":
		minBound, maxBound, ok := explicitSize(key, value, span)
		if !ok {
			ast.warn("invalid_size", fmt.Sprintf("invalid size %q", value), span)
			return false
		}
		ast.mergeBytes(minBound, maxBound)
		used[i] = true
		return true
	default:
		return false
	}
}

func addExplicitValue(ast *AST, destination *[]ValueClause, value string, span Span, used []bool, i int, name string) bool {
	value = normalize(value)
	if !validIdentifier(value) {
		ast.warn("invalid_filter", fmt.Sprintf("invalid %s %q", name, value), span)
		return false
	}
	addValue(destination, value, span)
	used[i] = true
	return true
}

func parseNaturalSize(ast *AST, tokens []token, used []bool, i int) bool {
	if tokens[i].occurrence != required {
		return false
	}
	for _, candidate := range sizeRelations {
		if !matchWords(tokens, used, i, candidate.words) {
			continue
		}
		at := i + len(candidate.words)
		bytes, count, lexeme, ok := quantityAt(tokens, used, at)
		if !ok {
			return false
		}
		span := Span{tokens[i].span.StartByte, tokens[at+count-1].span.EndByte}
		bound := &ByteBound{Bytes: bytes, Inclusive: candidate.inclusive, Lexeme: lexeme, Span: span}
		if candidate.minimum {
			ast.mergeBytes(bound, nil)
		} else {
			ast.mergeBytes(nil, bound)
		}
		mark(used, i, len(candidate.words)+count)
		return true
	}

	if normalize(tokens[i].text) != "between" || !available(tokens, used, i, 1) {
		return false
	}
	first, firstCount, firstLexeme, ok := quantityAt(tokens, used, i+1)
	if !ok {
		return false
	}
	and := i + 1 + firstCount
	if and >= len(tokens) || normalize(tokens[and].text) != "and" || tokens[and].occurrence != required {
		return false
	}
	second, secondCount, secondLexeme, ok := quantityAt(tokens, used, and+1)
	if !ok {
		return false
	}
	span := Span{tokens[i].span.StartByte, tokens[and+secondCount].span.EndByte}
	ast.mergeBytes(
		&ByteBound{Bytes: first, Inclusive: true, Lexeme: firstLexeme, Span: span},
		&ByteBound{Bytes: second, Inclusive: true, Lexeme: secondLexeme, Span: span},
	)
	mark(used, i, 2+firstCount+secondCount)
	return true
}

func quantityAt(tokens []token, used []bool, at int) (int64, int, string, bool) {
	if at >= len(tokens) || used[at] || tokens[at].quoted || tokens[at].occurrence != required {
		return 0, 0, "", false
	}
	if bytes, ok := parseByteQuantity(tokens[at].text); ok {
		return bytes, 1, tokens[at].text, true
	}
	if at+1 >= len(tokens) || used[at+1] || tokens[at+1].quoted || tokens[at+1].occurrence != required {
		return 0, 0, "", false
	}
	lexeme := tokens[at].text + " " + tokens[at+1].text
	bytes, ok := parseByteQuantity(tokens[at].text + tokens[at+1].text)
	return bytes, 2, lexeme, ok
}

func parseNaturalDate(ast *AST, tokens []token, used []bool, i int) bool {
	if tokens[i].occurrence != required || used[i] {
		return false
	}
	word := normalize(tokens[i].text)
	if word == "after" || word == "since" || word == "before" {
		if !available(tokens, used, i, 2) {
			return false
		}
		span := Span{tokens[i].span.StartByte, tokens[i+1].span.EndByte}
		start, end, ok := directionalDate(word, tokens[i+1].text, span)
		if !ok {
			return false
		}
		ast.mergeDate(start, end)
		mark(used, i, 2)
		return true
	}
	if word != "between" || !available(tokens, used, i, 4) || normalize(tokens[i+2].text) != "and" {
		return false
	}
	span := Span{tokens[i].span.StartByte, tokens[i+3].span.EndByte}
	firstStart, _, okFirst := exactDate(tokens[i+1].text, span)
	_, secondEnd, okSecond := exactDate(tokens[i+3].text, span)
	if !okFirst || !okSecond {
		return false
	}
	ast.mergeDate(firstStart, secondEnd)
	mark(used, i, 4)
	return true
}

func parseNaturalProvider(ast *AST, tokens []token, used []bool, i int) bool {
	if tokens[i].occurrence != required || used[i] {
		return false
	}
	word := normalize(tokens[i].text)
	if word == "from" && available(tokens, used, i, 2) {
		if provider, ok := providers[normalize(tokens[i+1].text)]; ok {
			span := Span{tokens[i].span.StartByte, tokens[i+1].span.EndByte}
			ast.addProvider(provider, false, span)
			mark(used, i, 2)
			return true
		}
	}
	if word == "not" && available(tokens, used, i, 3) && normalize(tokens[i+1].text) == "from" {
		if provider, ok := providers[normalize(tokens[i+2].text)]; ok {
			span := Span{tokens[i].span.StartByte, tokens[i+2].span.EndByte}
			ast.addProvider(provider, true, span)
			mark(used, i, 3)
			return true
		}
	}
	if available(tokens, used, i, 3) && normalize(tokens[i+1].text) == "sources" && normalize(tokens[i+2].text) == "only" {
		if provider, ok := providers[word]; ok {
			span := Span{tokens[i].span.StartByte, tokens[i+2].span.EndByte}
			ast.addProvider(provider, false, span)
			mark(used, i, 3)
			return true
		}
	}
	return false
}

func parseNaturalOrder(ast *AST, tokens []token, used []bool, i int) bool {
	if !available(tokens, used, i, 2) || normalize(tokens[i+1].text) != "first" {
		return false
	}
	field, direction, ok := parseOrder(tokens[i].text)
	if !ok {
		return false
	}
	span := Span{tokens[i].span.StartByte, tokens[i+1].span.EndByte}
	ast.addOrder(field, direction, span)
	mark(used, i, 2)
	return true
}

func parseNaturalLimit(ast *AST, tokens []token, used []bool, i int) bool {
	if !available(tokens, used, i, 2) {
		return false
	}
	word := normalize(tokens[i].text)
	if word != "top" && word != "first" {
		return false
	}
	limit, err := strconv.Atoi(tokens[i+1].text)
	if err != nil || limit < 1 || limit > MaxLimit {
		return false
	}
	ast.Limit = &limit
	mark(used, i, 2)
	if i+2 < len(tokens) && !used[i+2] && normalize(tokens[i+2].text) == "results" {
		used[i+2] = true
	}
	return true
}

func parseNaturalLanguage(ast *AST, tokens []token, used []bool, i int) bool {
	if !available(tokens, used, i, 2) {
		return false
	}
	word := normalize(tokens[i].text)
	if word != "in" && word != "language" {
		return false
	}
	language, ok := languageAliases[normalize(tokens[i+1].text)]
	if !ok {
		return false
	}
	span := Span{tokens[i].span.StartByte, tokens[i+1].span.EndByte}
	addValue(&ast.Languages, language, span)
	mark(used, i, 2)
	return true
}

func parseNaturalFacet(ast *AST, tokens []token, used []bool, i int) bool {
	tok := tokens[i]
	if tok.occurrence != required || used[i] {
		return false
	}
	word := normalize(tok.text)
	if architecture, ok := architectures[word]; ok {
		addValue(&ast.Architectures, architecture, tok.span)
		used[i] = true
		return true
	}
	if codec, ok := codecs[compact(word)]; ok {
		addValue(&ast.Codecs, codec, tok.span)
		used[i] = true
		return true
	}
	if resolution, ok := parseResolution(word, tok.span, false); ok {
		addResolution(&ast.Resolutions, resolution)
		used[i] = true
		return true
	}
	if (word == "4k" || word == "8k") && neighboringVideo(tokens, i) {
		resolution, _ := parseResolution(word, tok.span, true)
		addResolution(&ast.Resolutions, resolution)
		used[i] = true
		return true
	}
	if extension, ok := parseExtensionToken(word); ok {
		addValue(&ast.Extensions, extension, tok.span)
		used[i] = true
		return true
	}
	if i+1 < len(tokens) && word == "linux" && normalize(tokens[i+1].text) == "image" && available(tokens, used, i, 2) {
		addValue(&ast.ContentKinds, "disk_image", tokens[i+1].span)
		used[i+1] = true
		return false // Linux remains a lexical term.
	}
	if kind, ok := contentKinds[word]; ok {
		addValue(&ast.ContentKinds, kind, tok.span)
		used[i] = true
		return true
	}
	return false
}

func neighboringVideo(tokens []token, i int) bool {
	for _, at := range []int{i - 1, i + 1} {
		if at >= 0 && at < len(tokens) {
			candidate := tokens[at]
			if candidate.quoted || candidate.occurrence != required {
				continue
			}
			word := normalize(candidate.text)
			if word == "video" || word == "movie" || word == "animation" {
				return true
			}
		}
	}
	return false
}

func explicitSize(key, value string, span Span) (*ByteBound, *ByteBound, bool) {
	minimum, inclusive := false, true
	switch key {
	case "min-size":
		minimum = true
	case "max-size":
	case "size":
		switch {
		case strings.HasPrefix(value, "<="):
			value = value[2:]
		case strings.HasPrefix(value, ">="):
			minimum, value = true, value[2:]
		case strings.HasPrefix(value, "<"):
			inclusive, value = false, value[1:]
		case strings.HasPrefix(value, ">"):
			minimum, inclusive, value = true, false, value[1:]
		default:
			return nil, nil, false
		}
	}
	bytes, ok := parseByteQuantity(value)
	if !ok {
		return nil, nil, false
	}
	bound := &ByteBound{Bytes: bytes, Inclusive: inclusive, Lexeme: value, Span: span}
	if minimum {
		return bound, nil, true
	}
	return nil, bound, true
}

func parseByteQuantity(input string) (int64, bool) {
	input = strings.TrimSpace(input)
	if input == "" {
		return 0, false
	}
	at := 0
	dots := 0
	for at < len(input) {
		c := input[at]
		if c >= '0' && c <= '9' {
			at++
			continue
		}
		if c == '.' && dots == 0 {
			dots++
			at++
			continue
		}
		break
	}
	if at == 0 || at == len(input) || input[0] == '.' || input[at-1] == '.' {
		return 0, false
	}
	number, unit := input[:at], strings.ToLower(input[at:])
	scale, ok := byteUnits[unit]
	if !ok {
		return 0, false
	}
	parts := strings.Split(number, ".")
	digits := strings.Join(parts, "")
	if len(digits) > 40 {
		return 0, false
	}
	numerator, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return 0, false
	}
	numerator.Mul(numerator, big.NewInt(scale))
	denominator := big.NewInt(1)
	if len(parts) == 2 {
		denominator.Exp(big.NewInt(10), big.NewInt(int64(len(parts[1]))), nil)
	}
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if remainder.Sign() != 0 || !quotient.IsInt64() || quotient.Sign() < 0 {
		return 0, false
	}
	return quotient.Int64(), true
}

func parseDateRangeValue(value string, span Span) (*DateBound, *DateBound, bool) {
	if left, right, ok := strings.Cut(value, ".."); ok {
		start, _, okLeft := exactDate(left, span)
		_, end, okRight := exactDate(right, span)
		return start, end, okLeft && okRight
	}
	return exactDate(value, span)
}

func exactDate(value string, span Span) (*DateBound, *DateBound, bool) {
	if year, ok := parseYear(value); ok {
		return &DateBound{Date: fmt.Sprintf("%04d-01-01", year), Inclusive: true, Span: span},
			&DateBound{Date: fmt.Sprintf("%04d-12-31", year), Inclusive: true, Span: span}, true
	}
	date, ok := parseISODate(value)
	if !ok {
		return nil, nil, false
	}
	return &DateBound{Date: date, Inclusive: true, Span: span}, &DateBound{Date: date, Inclusive: true, Span: span}, true
}

func directionalDate(direction, value string, span Span) (*DateBound, *DateBound, bool) {
	if year, ok := parseYear(value); ok {
		switch direction {
		case "after":
			if year == 9999 {
				return nil, nil, false
			}
			return &DateBound{Date: fmt.Sprintf("%04d-01-01", year+1), Inclusive: true, Span: span}, nil, true
		case "since":
			return &DateBound{Date: fmt.Sprintf("%04d-01-01", year), Inclusive: true, Span: span}, nil, true
		case "before":
			if year == 1 {
				return nil, nil, false
			}
			return nil, &DateBound{Date: fmt.Sprintf("%04d-12-31", year-1), Inclusive: true, Span: span}, true
		}
	}
	date, ok := parseISODate(value)
	if !ok {
		return nil, nil, false
	}
	switch direction {
	case "after":
		return &DateBound{Date: date, Inclusive: false, Span: span}, nil, true
	case "since":
		return &DateBound{Date: date, Inclusive: true, Span: span}, nil, true
	case "before":
		return nil, &DateBound{Date: date, Inclusive: false, Span: span}, true
	default:
		return nil, nil, false
	}
}

func parseYear(value string) (int, bool) {
	if len(value) != 4 || !allASCII(value, asciiDigit) {
		return 0, false
	}
	year, _ := strconv.Atoi(value)
	return year, year >= 1 && year <= 9999
}

func parseISODate(value string) (string, bool) {
	if len(value) != len("2006-01-02") {
		return "", false
	}
	parsed, err := time.Parse("2006-01-02", value)
	return value, err == nil && parsed.Format("2006-01-02") == value
}

func looksLikeLocaleDate(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 3 &&
		len(parts[0]) >= 1 && len(parts[0]) <= 2 && allASCII(parts[0], asciiDigit) &&
		len(parts[1]) >= 1 && len(parts[1]) <= 2 && allASCII(parts[1], asciiDigit) &&
		len(parts[2]) == 4 && allASCII(parts[2], asciiDigit)
}

func parseLanguage(value string) (string, bool) {
	value = normalize(value)
	if alias, ok := languageAliases[value]; ok {
		return alias, true
	}
	value = strings.ReplaceAll(value, "_", "-")
	parts := strings.Split(value, "-")
	if len(parts[0]) < 2 || len(parts[0]) > 3 || !allASCII(parts[0], asciiLetter) {
		return "", false
	}
	for _, part := range parts[1:] {
		if len(part) < 2 || len(part) > 8 || !allASCII(part, asciiLetterOrDigit) {
			return "", false
		}
	}
	return value, len(value) <= 35
}

func parseResolution(value string, span Span, explicit bool) (Resolution, bool) {
	value = compact(value)
	if explicit {
		switch value {
		case "4k", "uhd":
			return Resolution{Vertical: 2160, Label: "2160p", Span: span}, true
		case "8k":
			return Resolution{Vertical: 4320, Label: "4320p", Span: span}, true
		}
	}
	if len(value) < 2 || value[len(value)-1] != 'p' || !allASCII(value[:len(value)-1], asciiDigit) {
		return Resolution{}, false
	}
	vertical, err := strconv.Atoi(value[:len(value)-1])
	if err != nil || vertical < 100 || vertical > 8640 {
		return Resolution{}, false
	}
	return Resolution{Vertical: vertical, Label: strconv.Itoa(vertical) + "p", Span: span}, true
}

func parseExtension(value string) (string, bool) {
	value = strings.TrimPrefix(normalize(value), ".")
	if len(value) < 1 || len(value) > 16 || !allASCII(value, asciiLetterOrDigit) {
		return "", false
	}
	return "." + value, true
}

func parseExtensionToken(value string) (string, bool) {
	if !strings.HasPrefix(value, ".") {
		return "", false
	}
	return parseExtension(value)
}

func parseOrder(value string) (string, string, bool) {
	switch compact(value) {
	case "newest", "newestfirst", "datedesc":
		return "date", "desc", true
	case "oldest", "oldestfirst", "dateasc":
		return "date", "asc", true
	case "smallest", "smallestfirst", "sizeasc":
		return "size", "asc", true
	case "largest", "largestfirst", "sizedesc":
		return "size", "desc", true
	case "relevance", "relevant":
		return "relevance", "desc", true
	case "title", "titleasc":
		return "title", "asc", true
	case "titledesc":
		return "title", "desc", true
	default:
		return "", "", false
	}
}

func (ast *AST) mergeBytes(minimum, maximum *ByteBound) {
	if minimum != nil && (ast.Size.Min == nil || minimum.Bytes > ast.Size.Min.Bytes || minimum.Bytes == ast.Size.Min.Bytes && !minimum.Inclusive && ast.Size.Min.Inclusive) {
		ast.Size.Min = minimum
	}
	if maximum != nil && (ast.Size.Max == nil || maximum.Bytes < ast.Size.Max.Bytes || maximum.Bytes == ast.Size.Max.Bytes && !maximum.Inclusive && ast.Size.Max.Inclusive) {
		ast.Size.Max = maximum
	}
}

func (ast *AST) mergeDate(start, end *DateBound) {
	if start != nil && (ast.Date.Start == nil || start.Date > ast.Date.Start.Date || start.Date == ast.Date.Start.Date && !start.Inclusive && ast.Date.Start.Inclusive) {
		ast.Date.Start = start
	}
	if end != nil && (ast.Date.End == nil || end.Date < ast.Date.End.Date || end.Date == ast.Date.End.Date && !end.Inclusive && ast.Date.End.Inclusive) {
		ast.Date.End = end
	}
}

func (ast *AST) addProvider(provider string, deny bool, span Span) {
	if deny {
		if containsValue(ast.Providers.Allow, provider) {
			ast.warn("provider_conflict", fmt.Sprintf("provider %q is both allowed and denied; deny wins", provider), span)
			ast.Providers.Allow = removeValue(ast.Providers.Allow, provider)
		}
		addValue(&ast.Providers.Deny, provider, span)
		return
	}
	if containsValue(ast.Providers.Deny, provider) {
		ast.warn("provider_conflict", fmt.Sprintf("provider %q is denied; allow ignored", provider), span)
		return
	}
	addValue(&ast.Providers.Allow, provider, span)
}

func (ast *AST) addOrder(field, direction string, span Span) {
	for _, order := range ast.Order {
		if order.Field != field {
			continue
		}
		if order.Direction != direction {
			ast.warn("order_conflict", fmt.Sprintf("ordering for %q already uses %s; later ordering ignored", field, order.Direction), span)
		}
		return
	}
	ast.Order = append(ast.Order, OrderClause{Field: field, Direction: direction, Span: span})
}

func (ast *AST) warn(code, message string, span Span) {
	ast.Warnings = append(ast.Warnings, Warning{Code: code, Message: message, Span: span})
}

func contradictoryBytes(value ByteRange) bool {
	if value.Min == nil || value.Max == nil {
		return false
	}
	return value.Min.Bytes > value.Max.Bytes || value.Min.Bytes == value.Max.Bytes && (!value.Min.Inclusive || !value.Max.Inclusive)
}

func contradictoryDates(value DateRange) bool {
	if value.Start == nil || value.End == nil {
		return false
	}
	return value.Start.Date > value.End.Date || value.Start.Date == value.End.Date && (!value.Start.Inclusive || !value.End.Inclusive)
}

func addValue(values *[]ValueClause, value string, span Span) {
	if !containsValue(*values, value) {
		*values = append(*values, ValueClause{Value: value, Span: span})
	}
}

func addResolution(values *[]Resolution, value Resolution) {
	for _, existing := range *values {
		if existing.Vertical == value.Vertical {
			return
		}
	}
	*values = append(*values, value)
}

func containsValue(values []ValueClause, value string) bool {
	for _, existing := range values {
		if existing.Value == value {
			return true
		}
	}
	return false
}

func removeValue(values []ValueClause, value string) []ValueClause {
	for i, existing := range values {
		if existing.Value == value {
			return append(values[:i], values[i+1:]...)
		}
	}
	return values
}

func available(tokens []token, used []bool, at, count int) bool {
	if at < 0 || at+count > len(tokens) {
		return false
	}
	for i := at; i < at+count; i++ {
		if used[i] || tokens[i].quoted || tokens[i].occurrence != required {
			return false
		}
	}
	return true
}

func mark(used []bool, at, count int) {
	for i := at; i < at+count; i++ {
		used[i] = true
	}
}

func matchWords(tokens []token, used []bool, at int, words []string) bool {
	if !available(tokens, used, at, len(words)) {
		return false
	}
	for i, word := range words {
		if normalize(tokens[at+i].text) != word {
			return false
		}
	}
	return true
}

func normalize(value string) string {
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

func compact(value string) string {
	value = normalize(value)
	return compactReplacer.Replace(value)
}

func validIdentifier(value string) bool {
	return value != "" && len(value) <= 40 && allASCII(value, func(c byte) bool {
		return asciiLetterOrDigit(c) || c == '_' || c == '-'
	})
}

func allASCII(value string, allowed func(byte) bool) bool {
	if value == "" {
		return false
	}
	for i := range len(value) {
		if !allowed(value[i]) {
			return false
		}
	}
	return true
}

func asciiDigit(c byte) bool         { return c >= '0' && c <= '9' }
func asciiLetter(c byte) bool        { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func asciiLetterOrDigit(c byte) bool { return asciiLetter(c) || asciiDigit(c) }

var fold = cases.Fold()

var byteUnits = map[string]int64{
	"b":   1,
	"kb":  1_000,
	"mb":  1_000_000,
	"gb":  1_000_000_000,
	"tb":  1_000_000_000_000,
	"pb":  1_000_000_000_000_000,
	"eb":  1_000_000_000_000_000_000,
	"kib": 1 << 10,
	"mib": 1 << 20,
	"gib": 1 << 30,
	"tib": 1 << 40,
	"pib": 1 << 50,
	"eib": 1 << 60,
}

var compactReplacer = strings.NewReplacer("-", "", "_", "", ".", "", " ", "")

var sizeRelations = []struct {
	words     []string
	minimum   bool
	inclusive bool
}{
	{[]string{"less", "than"}, false, false},
	{[]string{"more", "than"}, true, false},
	{[]string{"at", "most"}, false, true},
	{[]string{"at", "least"}, true, true},
	{[]string{"no", "more", "than"}, false, true},
	{[]string{"no", "less", "than"}, true, true},
	{[]string{"under"}, false, false},
	{[]string{"below"}, false, false},
	{[]string{"over"}, true, false},
	{[]string{"above"}, true, false},
	{[]string{"minimum"}, true, true},
	{[]string{"maximum"}, false, true},
}

var architectures = map[string]string{
	"amd64":   "amd64",
	"x86_64":  "amd64",
	"x64":     "amd64",
	"arm64":   "arm64",
	"aarch64": "arm64",
	"armhf":   "armhf",
	"ppc64el": "ppc64el",
	"riscv64": "riscv64",
	"s390x":   "s390x",
}

var codecs = map[string]string{
	"h264":   "h264",
	"avc":    "h264",
	"x264":   "h264",
	"h265":   "h265",
	"hevc":   "h265",
	"x265":   "h265",
	"av1":    "av1",
	"vp9":    "vp9",
	"theora": "theora",
	"xvid":   "xvid",
}

var providers = map[string]string{
	"archive":           "internet_archive",
	"ia":                "internet_archive",
	"internet_archive":  "internet_archive",
	"internet-archive":  "internet_archive",
	"academic":          "academic_torrents",
	"academictorrents":  "academic_torrents",
	"academic_torrents": "academic_torrents",
	"academic-torrents": "academic_torrents",
	"debian":            "debian",
	"debian_official":   "debian",
	"debian-official":   "debian",
}

var contentKinds = map[string]string{
	"animation":  "animation",
	"audio":      "audio",
	"book":       "book",
	"dataset":    "dataset",
	"disk-image": "disk_image",
	"disk_image": "disk_image",
	"ebook":      "book",
	"image":      "image",
	"movie":      "video",
	"music":      "audio",
	"software":   "software",
	"video":      "video",
}

var mediaKinds = map[string]string{
	"archive":    "archive",
	"audio":      "audio",
	"disk-image": "disk_image",
	"disk_image": "disk_image",
	"document":   "document",
	"image":      "image",
	"text":       "document",
	"video":      "video",
}

var languageAliases = map[string]string{
	"arabic":     "ar",
	"chinese":    "zh",
	"english":    "en",
	"french":     "fr",
	"german":     "de",
	"hindi":      "hi",
	"italian":    "it",
	"japanese":   "ja",
	"korean":     "ko",
	"portuguese": "pt",
	"russian":    "ru",
	"spanish":    "es",
}
