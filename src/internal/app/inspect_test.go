package app

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/keys-i/blackbeard/src/internal/torrent"
)

func TestInspectMagnetsJSON(t *testing.T) {
	t.Parallel()

	const (
		v1 = "0123456789abcdef0123456789abcdef01234567"
		v2 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	tests := []struct {
		name, uri, want string
	}{
		{
			"v1",
			"magnet:?xt=urn:btih:" + v1 + "&dn=Debian",
			`{"schema_version":1,"type":"torrent_inspect","data":{"source_type":"magnet","metadata_available":false,"name":"Debian","infohash_v1":"` + v1 + `","files":[],"trackers":[],"webseeds":[]}}` + "\n",
		},
		{
			"v2",
			"magnet:?xt=urn:btmh:1220" + v2 + "&dn=Dataset",
			`{"schema_version":1,"type":"torrent_inspect","data":{"source_type":"magnet","metadata_available":false,"name":"Dataset","infohash_v2":"` + v2 + `","files":[],"trackers":[],"webseeds":[]}}` + "\n",
		},
		{
			"hybrid",
			"magnet:?xt=urn:btih:" + v1 + "&xt=urn:btmh:1220" + v2,
			`{"schema_version":1,"type":"torrent_inspect","data":{"source_type":"magnet","metadata_available":false,"infohash_v1":"` + v1 + `","infohash_v2":"` + v2 + `","files":[],"trackers":[],"webseeds":[]}}` + "\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			stdout, stderr, err := runInspectCLI(context.Background(), nil, "--output", "json", "inspect", test.uri)
			if err != nil || stderr != "" {
				t.Fatalf("stdout=%q stderr=%q err=%v", stdout, stderr, err)
			}
			if stdout != test.want {
				t.Fatalf("JSON snapshot\n got: %s want: %s", stdout, test.want)
			}
			if test.name == "v1" {
				stdout, _, err = runInspectCLI(context.Background(), nil, "inspect", test.uri)
				if err != nil || !strings.Contains(stdout, "unknown (metadata unavailable)") || strings.Contains(stdout, test.uri) {
					t.Fatalf("table snapshot = %q, %v", stdout, err)
				}
			}
		})
	}
}

func TestInspectLocalMetainfoFormats(t *testing.T) {
	t.Parallel()

	_, metainfo := testV1Metainfo("sample.bin")
	path := filepath.Join(t.TempDir(), "sample.torrent")
	if err := os.WriteFile(path, metainfo, 0o600); err != nil {
		t.Fatal(err)
	}
	const wantHash = "30dea4f60556367a3b3e9c117394a50c7d000f8a"

	stdout, stderr, err := runInspectCLI(context.Background(), nil, "--output", "ndjson", "inspect", path)
	if err != nil || stderr != "" {
		t.Fatalf("stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	assertStreamTypes(t, stdout, "torrent", "file", "done")
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if strings.Contains(lines[0], path) || !strings.Contains(lines[0], `"format":"v1"`) || !strings.Contains(lines[0], `"infohash_v1":"`+wantHash+`"`) || !strings.Contains(lines[1], `"index":1`) || !strings.Contains(lines[1], `"path":"sample.bin"`) || !strings.Contains(lines[2], `"files":1`) {
		t.Fatalf("NDJSON snapshot = %q", stdout)
	}

	stdout, stderr, err = runInspectCLI(context.Background(), nil, "inspect", path)
	if err != nil || stderr != "" || !strings.Contains(stdout, "FIELD") || !strings.Contains(stdout, "sample.bin (5 bytes)") || strings.Contains(stdout, path) {
		t.Fatalf("table snapshot stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
}

func TestInspectHTTPSUsesBoundedInjectedFetch(t *testing.T) {
	t.Parallel()

	_, metainfo := testV1Metainfo("remote.bin")
	called := false
	fetch := func(ctx context.Context, source *url.URL, maxBytes int64) ([]byte, error) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > inspectTimeout {
			t.Fatalf("inspect deadline = %v, %v", deadline, ok)
		}
		if source.Scheme != "https" || source.Host != "example.test" || source.EscapedPath() != "/catalogue/sample%20file.torrent" || source.RawQuery != "download=1" {
			t.Fatalf("source = %s", source)
		}
		if maxBytes != 16<<20 {
			t.Fatalf("body cap = %d", maxBytes)
		}
		return metainfo, nil
	}
	stdout, stderr, err := runInspectCLI(context.Background(), fetch, "--output", "json", "inspect", "https://example.test/catalogue/sample%20file.torrent?download=1")
	if err != nil || stderr != "" || !called || !strings.Contains(stdout, `"source_type":"torrent_https"`) || strings.Contains(stdout, "example.test") {
		t.Fatalf("called=%v stdout=%q stderr=%q err=%v", called, stdout, stderr, err)
	}
}

func TestInspectCancellationAndInvalidArgs(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fetch := func(ctx context.Context, _ *url.URL, _ int64) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	stdout, stderr, err := runInspectCLI(ctx, fetch, "inspect", "https://example.test/sample.torrent")
	if !errors.Is(err, context.Canceled) || ExitCode(err) != 130 || stdout != "" || stderr != "" {
		t.Fatalf("cancel stdout=%q stderr=%q err=%v code=%d", stdout, stderr, err, ExitCode(err))
	}

	for _, args := range [][]string{
		{"inspect"},
		{"inspect", "one.torrent", "two.torrent"},
		{"inspect", "http://example.test/sample.torrent"},
		{"inspect", "magnet:?dn=missing-hash"},
	} {
		stdout, stderr, err = runInspectCLI(context.Background(), nil, args...)
		if err == nil || ExitCode(err) != 2 || stdout != "" || stderr != "" {
			t.Fatalf("args=%q stdout=%q stderr=%q err=%v code=%d", args, stdout, stderr, err, ExitCode(err))
		}
	}
}

func TestInspectOutputSanitizesTerminalControls(t *testing.T) {
	t.Parallel()

	info, _ := testV1Metainfo("safe.bin")
	tracker := "https://tracker.example/announce"
	comment := "bad\x1b]8;;https://example.test\a"
	metainfo := []byte("d8:announce" + strconv.Itoa(len(tracker)) + ":" + tracker + "7:comment" + strconv.Itoa(len(comment)) + ":" + comment + "4:info" + string(info) + "e")
	path := filepath.Join(t.TempDir(), "control.torrent")
	if err := os.WriteFile(path, metainfo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"table", "json", "ndjson"} {
		stdout, stderr, err := runInspectCLI(context.Background(), nil, "--output", format, "inspect", path)
		if err != nil || stderr != "" || bytes.Contains([]byte(stdout), []byte{0x1b}) || strings.ContainsRune(stdout, '\a') {
			t.Fatalf("format=%s stdout=%q stderr=%q err=%v", format, stdout, stderr, err)
		}
	}

	_, hostilePath := testV1Metainfo("bad\x1b.bin")
	if err := os.WriteFile(path, hostilePath, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err := runInspectCLI(context.Background(), nil, "inspect", path)
	if err == nil || stdout != "" || stderr != "" {
		t.Fatalf("hostile path stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
}

func testV1Metainfo(name string) ([]byte, []byte) {
	tracker := "https://tracker.example/announce"
	info := []byte("d6:lengthi5e4:name" + strconv.Itoa(len(name)) + ":" + name + "12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae")
	metainfo := []byte("d8:announce" + strconv.Itoa(len(tracker)) + ":" + tracker + "4:info" + string(info) + "e")
	return info, metainfo
}

func runInspectCLI(ctx context.Context, fetch torrent.Fetch, args ...string) (string, string, error) {
	var stdout, stderr bytes.Buffer
	err := run(ctx, args, strings.NewReader(""), &stdout, &stderr, "test", catalogueDeps{fetchTorrent: fetch})
	return stdout.String(), stderr.String(), err
}
