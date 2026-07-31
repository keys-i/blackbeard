package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestHelpAndVersion(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		args []string
		want string
	}{
		{nil, "Usage:"},
		{[]string{"version"}, "test-version\n"},
		{[]string{"--version"}, "test-version\n"},
	} {
		var stdout, stderr bytes.Buffer
		if err := Run(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr, "test-version"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), test.want) {
			t.Fatalf("stdout %q does not contain %q", stdout.String(), test.want)
		}
		if stderr.Len() != 0 {
			t.Fatalf("stderr = %q", stderr.String())
		}
	}
}

func TestMachineHelpAndVersion(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		args     []string
		wantType string
	}{
		{[]string{"--output", "json", "help"}, `"type":"help"`},
		{[]string{"--output", "ndjson", "version"}, `"type":"version"`},
		{[]string{"--output", "json", "--version"}, `"type":"version"`},
	} {
		var stdout, stderr bytes.Buffer
		if err := Run(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr, "test-version"); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdout.String(), test.wantType) {
			t.Fatalf("Run(%q) stdout = %q", test.args, stdout.String())
		}
		if stderr.Len() != 0 {
			t.Fatalf("Run(%q) stderr = %q", test.args, stderr.String())
		}
	}
}

func TestSearchExplainJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := Run(
		context.Background(),
		[]string{"search", "--explain", "--output", "json", "arm64 Linux image under 2 GiB newest first"},
		strings.NewReader(""),
		&stdout,
		&stderr,
		"test-version",
	)
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	var envelope struct {
		SchemaVersion int       `json:"schema_version"`
		Type          string    `json:"type"`
		Data          queryData `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, stdout.String())
	}
	if envelope.SchemaVersion != 1 || envelope.Type != "query_explain" {
		t.Fatalf("envelope = %#v", envelope)
	}
	if envelope.Data.SchemaVersion != 1 {
		t.Fatalf("query schema version = %d", envelope.Data.SchemaVersion)
	}
	if len(envelope.Data.Architectures) != 1 || envelope.Data.Architectures[0].Value != "arm64" {
		t.Fatalf("data = %#v", envelope.Data)
	}
}

func TestSearchExplainNDJSON(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := Run(
		context.Background(),
		[]string{"--output", "ndjson", "search", "--explain", "4K"},
		strings.NewReader(""),
		&stdout,
		&stderr,
		"test-version",
	)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"type":"query"`) || !strings.Contains(lines[1], `"type":"done"`) {
		t.Fatalf("NDJSON = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSearchKeepsNegativeTermsAfterTheFirstWord(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := Run(
		context.Background(),
		[]string{"--output", "json", "search", "--explain", "linux", "-server"},
		strings.NewReader(""),
		&stdout,
		&stderr,
		"test-version",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"excluded":[{"raw":"server","normalized":"server"`) {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestSearchExplainTableSanitizesControls(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	err := Run(
		context.Background(),
		[]string{"search", "--explain", "Debian\x1b[31m"},
		strings.NewReader(""),
		&stdout,
		&stderr,
		"test-version",
	)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stdout.Bytes(), []byte{0x1b}) {
		t.Fatalf("stdout contains escape: %q", stdout.String())
	}
}

func TestSearchErrorsDoNotPolluteStdout(t *testing.T) {
	t.Parallel()

	tests := []struct {
		args     []string
		wantCode int
	}{
		{[]string{"search"}, 2},
		{[]string{"search", "--explain", "--output", "yaml", "debian"}, 2},
		{[]string{"--output", "yaml", "help"}, 2},
		{[]string{"search", "debian"}, 1},
		{[]string{"version", "extra"}, 2},
		{[]string{"nonesuch"}, 2},
	}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		err := Run(context.Background(), test.args, strings.NewReader(""), &stdout, &stderr, "test-version")
		if err == nil {
			t.Fatalf("Run(%q) succeeded", test.args)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("Run(%q) wrote stdout=%q stderr=%q", test.args, stdout.String(), stderr.String())
		}
		if got := ExitCode(err); got != test.wantCode {
			t.Fatalf("ExitCode(%v) = %d, want %d", err, got, test.wantCode)
		}
	}
}

func TestExitCodePreservesWrappedUsageError(t *testing.T) {
	t.Parallel()

	err := errors.Join(errors.New("context"), usageError(errors.New("bad query")))
	if got := ExitCode(err); got != 2 {
		t.Fatalf("ExitCode() = %d, want 2", got)
	}
}

type queryData struct {
	SchemaVersion int `json:"schema_version"`
	Architectures []struct {
		Value string `json:"value"`
	} `json:"architectures"`
}
