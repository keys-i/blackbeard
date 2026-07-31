// Package output owns Blackbeard's stdout formats. It never logs or writes
// anywhere except the io.Writer supplied to Encoder.
package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/keys-i/blackbeard/src/internal/termtext"
)

const SchemaVersion = 1
const maxTableCellBytes = 4096

// Envelope is the single-document JSON contract.
type Envelope struct {
	SchemaVersion int    `json:"schema_version"`
	Type          string `json:"type"`
	Data          any    `json:"data"`
}

// StreamRecord is one line of the NDJSON contract. Sequence starts at one for
// each Encoder and advances only after a successful write.
type StreamRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Type          string `json:"type"`
	Sequence      uint64 `json:"sequence"`
	Data          any    `json:"data"`
}

// Table is the dependency-free, human-readable output model. Column and row
// order are preserved.
type Table struct {
	Columns []string
	Rows    [][]string
}

// Encoder writes to one caller-owned destination. It is not safe for
// concurrent use.
type Encoder struct {
	dst      io.Writer
	sequence uint64
}

// NewEncoder returns an encoder that writes only to dst.
func NewEncoder(dst io.Writer) *Encoder {
	return &Encoder{dst: dst}
}

// JSON writes one schema-versioned JSON document.
func (e *Encoder) JSON(recordType string, data any) error {
	if recordType == "" {
		return errors.New("output type is empty")
	}
	if err := e.writeJSON(Envelope{SchemaVersion: SchemaVersion, Type: recordType, Data: data}); err != nil {
		return fmt.Errorf("encode JSON output: %w", err)
	}
	return nil
}

// NDJSON writes one schema-versioned record followed by a newline.
func (e *Encoder) NDJSON(recordType string, data any) error {
	if recordType == "" {
		return errors.New("output type is empty")
	}
	next := e.sequence + 1
	if err := e.writeJSON(StreamRecord{SchemaVersion: SchemaVersion, Type: recordType, Sequence: next, Data: data}); err != nil {
		return fmt.Errorf("encode NDJSON output: %w", err)
	}
	e.sequence = next
	return nil
}

// Table writes a plain, tab-aligned table with one row per line.
func (e *Encoder) Table(table Table) error {
	if len(table.Columns) == 0 {
		return errors.New("table has no columns")
	}
	for i, row := range table.Rows {
		if len(row) != len(table.Columns) {
			return fmt.Errorf("table row %d has %d cells; want %d", i+1, len(row), len(table.Columns))
		}
	}

	tw := tabwriter.NewWriter(e.dst, 0, 4, 2, ' ', 0)
	writeRow := func(row []string) error {
		for i, cell := range row {
			if i > 0 {
				if _, err := io.WriteString(tw, "\t"); err != nil {
					return err
				}
			}
			if _, err := io.WriteString(tw, termtext.Sanitize(cell, maxTableCellBytes)); err != nil {
				return err
			}
		}
		_, err := io.WriteString(tw, "\n")
		return err
	}

	if err := writeRow(table.Columns); err != nil {
		return fmt.Errorf("write table header: %w", err)
	}
	for i, row := range table.Rows {
		if err := writeRow(row); err != nil {
			return fmt.Errorf("write table row %d: %w", i+1, err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush table output: %w", err)
	}
	return nil
}

func (e *Encoder) writeJSON(value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	b = terminalSafeJSON(b)
	b = append(b, '\n')
	n, err := e.dst.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

// terminalSafeJSON leaves the decoded value unchanged while preventing C1
// terminal and bidi controls from reaching a terminal as literal runes.
func terminalSafeJSON(src []byte) []byte {
	var dst []byte
	for i := 0; i < len(src); {
		r, size := utf8.DecodeRune(src[i:])
		if !unsafeTerminalRune(r) {
			if dst != nil {
				dst = append(dst, src[i:i+size]...)
			}
			i += size
			continue
		}
		if dst == nil {
			dst = make([]byte, 0, len(src)+6)
			dst = append(dst, src[:i]...)
		}
		const hex = "0123456789abcdef"
		dst = append(dst, '\\', 'u', hex[r>>12], hex[r>>8&15], hex[r>>4&15], hex[r&15])
		i += size
	}
	if dst == nil {
		return src
	}
	return dst
}

func unsafeTerminalRune(r rune) bool {
	return r >= '\u007f' && r <= '\u009f' || unicode.Is(unicode.Bidi_Control, r)
}
