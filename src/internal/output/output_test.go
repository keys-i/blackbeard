package output

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

type result struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Provider string `json:"provider"`
}

func TestStructuredOutput(t *testing.T) {
	var dst bytes.Buffer
	encoder := NewEncoder(&dst)
	payload := result{ID: "abc", Title: "Debian\x1b[31m\u009b32m\u200f\u202e", Provider: "debian"}

	if err := encoder.JSON("search", payload); err != nil {
		t.Fatal(err)
	}
	if err := encoder.NDJSON("result", payload); err != nil {
		t.Fatal(err)
	}
	if err := encoder.NDJSON("done", nil); err != nil {
		t.Fatal(err)
	}

	want := "" +
		`{"schema_version":1,"type":"search","data":{"id":"abc","title":"Debian\u001b[31m\u009b32m\u200f\u202e","provider":"debian"}}` + "\n" +
		`{"schema_version":1,"type":"result","sequence":1,"data":{"id":"abc","title":"Debian\u001b[31m\u009b32m\u200f\u202e","provider":"debian"}}` + "\n" +
		`{"schema_version":1,"type":"done","sequence":2,"data":null}` + "\n"
	if got := dst.String(); got != want {
		t.Fatalf("output mismatch\n got: %q\nwant: %q", got, want)
	}
	if bytes.Contains(dst.Bytes(), []byte{0x1b}) {
		t.Fatal("structured output contains a literal escape byte")
	}
}

func TestTable(t *testing.T) {
	var dst bytes.Buffer
	err := NewEncoder(&dst).Table(Table{
		Columns: []string{"ID", "TITLE"},
		Rows: [][]string{
			{"1", "Debian\x1b[31m"},
			{"2\nforged", "Archive\titem"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := dst.String(), "ID       TITLE\n1        Debian\n2forged  Archiveitem\n"; got != want {
		t.Fatalf("table mismatch\n got: %q\nwant: %q", got, want)
	}
	if bytes.Contains(dst.Bytes(), []byte{0x1b}) {
		t.Fatal("table contains a literal escape byte")
	}
}

func TestValidationWritesNothing(t *testing.T) {
	tests := []struct {
		name  string
		write func(*Encoder) error
	}{
		{"empty type", func(e *Encoder) error { return e.JSON("", nil) }},
		{"empty columns", func(e *Encoder) error { return e.Table(Table{}) }},
		{"short row", func(e *Encoder) error {
			return e.Table(Table{Columns: []string{"A", "B"}, Rows: [][]string{{"one"}}})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var dst bytes.Buffer
			if err := test.write(NewEncoder(&dst)); err == nil {
				t.Fatal("expected an error")
			}
			if dst.Len() != 0 {
				t.Fatalf("validation polluted destination with %q", dst.String())
			}
		})
	}
}

func TestWriteError(t *testing.T) {
	want := errors.New("boom")
	err := NewEncoder(errorWriter{want}).NDJSON("result", nil)
	if !errors.Is(err, want) {
		t.Fatalf("error %v does not wrap %v", err, want)
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) {
	return 0, w.err
}

func BenchmarkJSON(b *testing.B) {
	encoder := NewEncoder(io.Discard)
	payload := result{ID: "7238f090", Title: "Debian arm64 installer", Provider: "debian"}
	b.ReportAllocs()
	for b.Loop() {
		if err := encoder.JSON("search", payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNDJSON(b *testing.B) {
	encoder := NewEncoder(io.Discard)
	payload := result{ID: "7238f090", Title: "Debian arm64 installer", Provider: "debian"}
	b.ReportAllocs()
	for b.Loop() {
		if err := encoder.NDJSON("result", payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTable(b *testing.B) {
	table := Table{Columns: []string{"ID", "TITLE", "PROVIDER"}, Rows: [][]string{{"7238f090", "Debian arm64 installer", "debian"}}}
	b.ReportAllocs()
	for b.Loop() {
		if err := NewEncoder(io.Discard).Table(table); err != nil {
			b.Fatal(err)
		}
	}
}
