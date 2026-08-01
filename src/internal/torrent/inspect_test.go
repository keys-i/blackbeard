package torrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

func TestParseSourceAndInspectMagnet(t *testing.T) {
	const magnet = "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567" +
		"&xt=urn:btmh:1220abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd" +
		"&dn=Public+data&tr=https%3A%2F%2Ftracker.example%2Fannounce&ws=https%3A%2F%2Fseed.example%2Ffile"
	source, err := ParseSource(magnet)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Inspect(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != SourceMagnet || got.MetadataAvailable || got.Format != "" || got.Name != "Public data" {
		t.Fatalf("unexpected magnet inspection: %#v", got)
	}
	if got.InfoHashV1 != "0123456789abcdef0123456789abcdef01234567" || got.InfoHashV2 != "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd" {
		t.Fatalf("unexpected hashes: %q %q", got.InfoHashV1, got.InfoHashV2)
	}
	if got.TotalSize != nil || got.PieceLength != nil || got.Private != nil || got.Files == nil {
		t.Fatalf("magnet metadata availability is ambiguous: %#v", got)
	}
}

func TestInspectV1Metainfo(t *testing.T) {
	raw := fixtureV1(t, metainfo.Info{
		PieceLength: 16 << 10,
		Pieces:      make([]byte, 20),
		Name:        "payload.bin",
		Length:      1,
	}, func(meta *metainfo.MetaInfo) {
		meta.Announce = "https://tracker.example/announce"
		meta.UrlList = []string{"https://seed.example/payload.bin"}
		meta.Comment = "safe\x1b[31m red"
		meta.CreatedBy = "fixture"
	})
	got, err := inspectMetainfo(context.Background(), raw, SourceFile)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != "v1" || !got.MetadataAvailable || got.InfoHashV1 == "" || got.InfoHashV2 != "" {
		t.Fatalf("unexpected protocol fields: %#v", got)
	}
	if got.TotalSize == nil || *got.TotalSize != 1 || got.PieceLength == nil || *got.PieceLength != 16<<10 || got.Private == nil || *got.Private {
		t.Fatalf("unexpected typed metadata: %#v", got)
	}
	if len(got.Files) != 1 || got.Files[0] != (File{Path: "payload.bin", Size: 1}) {
		t.Fatalf("unexpected files: %#v", got.Files)
	}
	if got.Comment != "safe red" {
		t.Fatalf("comment was not sanitized: %q", got.Comment)
	}
}

func TestInspectV2AndHybridMetainfo(t *testing.T) {
	root := strings.Repeat("r", 32)
	tree := metainfo.FileTree{Dir: map[string]metainfo.FileTree{
		"payload.bin": {File: metainfo.FileTreeFile{Length: 1, PiecesRoot: root}},
	}}
	tests := []struct {
		name   string
		info   metainfo.Info
		format string
	}{
		{
			name:   "v2",
			info:   metainfo.Info{PieceLength: 16 << 10, Name: "bundle", MetaVersion: 2, FileTree: tree},
			format: "v2",
		},
		{
			name: "hybrid",
			info: metainfo.Info{
				PieceLength: 16 << 10,
				Pieces:      make([]byte, 20),
				Name:        "bundle",
				Files:       []metainfo.FileInfo{{Length: 1, Path: []string{"payload.bin"}}},
				MetaVersion: 2,
				FileTree:    tree,
			},
			format: "hybrid",
		},
		{
			name: "single-file hybrid",
			info: metainfo.Info{
				PieceLength: 16 << 10,
				Pieces:      make([]byte, 20),
				Name:        "payload.bin",
				Length:      1,
				MetaVersion: 2,
				FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
					"payload.bin": {File: metainfo.FileTreeFile{Length: 1, PiecesRoot: root}},
				}},
			},
			format: "hybrid",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := fixtureV1(t, test.info, nil)
			got, err := inspectMetainfo(context.Background(), raw, SourceHTTPS)
			if err != nil {
				t.Fatal(err)
			}
			if got.Format != test.format || got.InfoHashV2 == "" || test.format == "hybrid" && got.InfoHashV1 == "" {
				t.Fatalf("unexpected protocol fields: %#v", got)
			}
		})
	}
}

func TestInspectV2AllowsExtensionProperties(t *testing.T) {
	root := strings.Repeat("r", 32)
	raw := fixtureV1(t, metainfo.Info{
		PieceLength: 16 << 10,
		Name:        "bundle",
		MetaVersion: 2,
		FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
			"payload.bin": {File: metainfo.FileTreeFile{Length: 1, PiecesRoot: root}},
		}},
	}, nil)
	raw = bytes.Replace(raw, []byte("d6:length"), []byte("d4:attr1:x6:length"), 1)
	if _, err := inspectMetainfo(context.Background(), raw, SourceFile); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightBencodeCancellation(t *testing.T) {
	raw := []byte("d4:infod1:al")
	raw = append(raw, bytes.Repeat([]byte("0:"), 2048)...)
	raw = append(raw, []byte("eee")...)
	ctx := &cancelAfterChecks{Context: context.Background(), remaining: 2}
	if _, err := preflightBencode(ctx, raw); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

type cancelAfterChecks struct {
	context.Context
	remaining int
}

func (c *cancelAfterChecks) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

func TestV2PiecesRootRegressionDoesNotPanic(t *testing.T) {
	tests := []struct {
		name   string
		length int64
		root   string
	}{
		{"missing", 1, ""},
		{"short", 1, "short"},
		{"empty file root", 0, strings.Repeat("r", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := metainfo.Info{
				PieceLength: 16 << 10,
				Name:        "bundle",
				MetaVersion: 2,
				FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
					"file": {File: metainfo.FileTreeFile{Length: test.length, PiecesRoot: test.root}},
				}},
			}
			_, err := inspectMetainfo(context.Background(), fixtureV1(t, info, nil), SourceFile)
			if !errors.Is(err, ErrInvalidMetainfo) {
				t.Fatalf("got %v, want ErrInvalidMetainfo", err)
			}
		})
	}
}

func TestCanonicalBencodePreflight(t *testing.T) {
	deep := append(bytes.Repeat([]byte("l"), maxBencodeDepth+1), bytes.Repeat([]byte("e"), maxBencodeDepth+1)...)
	tests := []struct {
		name string
		raw  []byte
	}{
		{"top list", []byte("le")},
		{"missing info", []byte("de")},
		{"trailing bytes", []byte("d4:infodeejunk")},
		{"unsorted keys", []byte("d4:info0:1:a0:e")},
		{"duplicate keys", []byte("d4:infod1:a0:1:a0:ee")},
		{"leading integer zero", []byte("d4:infod1:ai01eee")},
		{"leading integer plus", []byte("d4:infod1:ai+1eee")},
		{"negative zero", []byte("d4:infod1:ai-0eee")},
		{"leading string zero", []byte("d4:infod1:a00:ee")},
		{"scalar info", []byte("d4:info1:xe")},
		{"deep", append([]byte("d4:info"), append(deep, 'e')...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := preflightBencode(context.Background(), test.raw); !errors.Is(err, ErrInvalidMetainfo) {
				t.Fatalf("got %v, want ErrInvalidMetainfo", err)
			}
		})
	}
}

func TestCanonicalBencodeMemberLimit(t *testing.T) {
	raw := []byte("d4:infod1:al")
	raw = append(raw, bytes.Repeat([]byte("0:"), maxContainerMembers+1)...)
	raw = append(raw, []byte("eee")...)
	if _, err := preflightBencode(context.Background(), raw); !errors.Is(err, ErrInvalidMetainfo) {
		t.Fatalf("got %v, want ErrInvalidMetainfo", err)
	}
}

func TestRejectsHostilePaths(t *testing.T) {
	paths := [][][]string{
		{{"..", "escape"}},
		{{"dir\\escape"}},
		{{"C:"}},
		{{"Readme"}, {"README"}},
		{{"file"}, {"file", "child"}},
		{{"CON.txt"}},
		{{"CON.foo.txt"}},
		{{"NUL.archive.bin"}},
		{{"COM1.foo.txt"}},
		{{"COM¹.txt"}},
		{{"LPT³.log"}},
		{{"CONIN$.txt"}},
		{{"CONOUT$.log"}},
		{{"ſ.txt"}, {"s.txt"}},
		{{"ς.txt"}, {"σ.txt"}},
	}
	for _, filePaths := range paths {
		files := make([]metainfo.FileInfo, len(filePaths))
		pieces := make([]byte, len(filePaths)*20)
		for i, path := range filePaths {
			files[i] = metainfo.FileInfo{Length: 1, Path: path}
		}
		info := metainfo.Info{PieceLength: 1, Pieces: pieces, Name: "bundle", Files: files}
		if _, err := inspectMetainfo(context.Background(), fixtureV1(t, info, nil), SourceFile); !errors.Is(err, ErrInvalidMetainfo) {
			t.Fatalf("paths %#v: got %v, want ErrInvalidMetainfo", filePaths, err)
		}
	}
}

func TestRejectsInvalidV1Layout(t *testing.T) {
	tests := []metainfo.Info{
		{PieceLength: 16 << 10, Pieces: []byte("short"), Name: "file", Length: 1},
		{PieceLength: 16 << 10, Pieces: make([]byte, 40), Name: "file", Length: 1},
		{PieceLength: 0, Name: "file", Length: 1},
		{PieceLength: 1, Pieces: make([]byte, 20), Name: "file", Length: -1},
		{PieceLength: 1, Pieces: make([]byte, 20), Name: "bundle", Files: []metainfo.FileInfo{{Length: 1, Path: []string{"link"}, ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "l", SymlinkPath: []string{"..", "escape"}}}}},
		{PieceLength: 1, Pieces: make([]byte, 20), Name: "link", Length: 1, ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "l", SymlinkPath: []string{"..", "escape"}}},
	}
	for _, info := range tests {
		if _, err := inspectMetainfo(context.Background(), fixtureV1(t, info, nil), SourceFile); !errors.Is(err, ErrInvalidMetainfo) {
			t.Fatalf("info %#v: got %v, want ErrInvalidMetainfo", info, err)
		}
	}
}

func TestHybridRequiresV2PieceAlignment(t *testing.T) {
	root := strings.Repeat("r", 32)
	tree := metainfo.FileTree{Dir: map[string]metainfo.FileTree{
		"a": {File: metainfo.FileTreeFile{Length: 1, PiecesRoot: root}},
		"b": {File: metainfo.FileTreeFile{Length: 1, PiecesRoot: root}},
	}}
	realFiles := []metainfo.FileInfo{{Length: 1, Path: []string{"a"}}, {Length: 1, Path: []string{"b"}}}
	tests := []struct {
		name   string
		files  []metainfo.FileInfo
		pieces int
		valid  bool
	}{
		{name: "missing padding", files: realFiles, pieces: 1},
		{name: "wrong padding", files: []metainfo.FileInfo{
			realFiles[0],
			{Length: 1, Path: []string{".pad", "1"}, ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
			realFiles[1],
		}, pieces: 1},
		{name: "exact padding", files: []metainfo.FileInfo{
			realFiles[0],
			{Length: (16 << 10) - 1, Path: []string{".pad", "16383"}, ExtendedFileAttrs: metainfo.ExtendedFileAttrs{Attr: "p"}},
			realFiles[1],
		}, pieces: 2, valid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := fixtureV1(t, metainfo.Info{
				PieceLength: 16 << 10,
				Pieces:      make([]byte, test.pieces*20),
				Name:        "bundle",
				Files:       test.files,
				MetaVersion: 2,
				FileTree:    tree,
			}, nil)
			_, err := inspectMetainfo(context.Background(), raw, SourceFile)
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && !errors.Is(err, ErrInvalidMetainfo) {
				t.Fatalf("got %v, want ErrInvalidMetainfo", err)
			}
		})
	}
}

func TestV2PieceLayerCardinality(t *testing.T) {
	const pieceLength int64 = 16 << 10
	shortRoot := strings.Repeat("x", 32)
	shortInfo := metainfo.Info{PieceLength: pieceLength, FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
		"file": {File: metainfo.FileTreeFile{Length: 3 * pieceLength, PiecesRoot: shortRoot}},
	}}}
	if err := validatePieceLayers(map[string]string{shortRoot: shortRoot}, map[string]int64{shortRoot: 3 * pieceLength}, &shortInfo); !errors.Is(err, ErrInvalidMetainfo) {
		t.Fatalf("short layer got %v, want ErrInvalidMetainfo", err)
	}

	hashes := [3]string{strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)}
	left := sha256.Sum256([]byte(hashes[0] + hashes[1]))
	pad := metainfo.HashForPiecePad(pieceLength)
	rightInput := append([]byte(hashes[2]), pad[:]...)
	right := sha256.Sum256(rightInput)
	rootInput := append(left[:], right[:]...)
	rootHash := sha256.Sum256(rootInput)
	root := string(rootHash[:])
	info := metainfo.Info{PieceLength: pieceLength, FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
		"file": {File: metainfo.FileTreeFile{Length: 3 * pieceLength, PiecesRoot: root}},
	}}}
	if err := validatePieceLayers(map[string]string{root: strings.Join(hashes[:], "")}, map[string]int64{root: 3 * pieceLength}, &info); err != nil {
		t.Fatalf("valid three-hash layer: %v", err)
	}
}

func TestRejectsDisagreeingUTF8Names(t *testing.T) {
	info := metainfo.Info{
		PieceLength: 1,
		Pieces:      make([]byte, 20),
		Name:        "legacy.bin",
		NameUtf8:    "other.bin",
		Length:      1,
	}
	if _, err := inspectMetainfo(context.Background(), fixtureV1(t, info, nil), SourceFile); !errors.Is(err, ErrInvalidMetainfo) {
		t.Fatalf("got %v, want ErrInvalidMetainfo", err)
	}
}

func TestRejectsDisagreeingUTF8Paths(t *testing.T) {
	info := metainfo.Info{
		PieceLength: 1,
		Pieces:      make([]byte, 20),
		Name:        "bundle",
		Files: []metainfo.FileInfo{{
			Length:   1,
			Path:     []string{"legacy.bin"},
			PathUtf8: []string{"other.bin"},
		}},
	}
	if _, err := inspectMetainfo(context.Background(), fixtureV1(t, info, nil), SourceFile); !errors.Is(err, ErrInvalidMetainfo) {
		t.Fatalf("got %v, want ErrInvalidMetainfo", err)
	}
}

func TestRejectsMagnetAbuse(t *testing.T) {
	hash := "0123456789abcdef0123456789abcdef01234567"
	tests := []string{
		"magnet:",
		"magnet://host/?xt=urn:btih:" + hash,
		"magnet:/path?xt=urn:btih:" + hash,
		"magnet:?xt=urn:unknown:" + hash,
		"magnet:?xt=urn:btih:" + hash + "&xt=urn:btih:" + hash,
		"magnet:?xt=urn:btih:" + hash + "#fragment",
		"magnet:?xt=urn:btih:" + hash + "&dn=%1b%5b31m",
		"magnet:?xt=urn:btih:" + hash + "&tr=http%3A%2F%2F127.0.0.1%2Fannounce",
	}
	for _, raw := range tests {
		if _, err := ParseSource(raw); !errors.Is(err, ErrInvalidSource) && !errors.Is(err, ErrInvalidMetainfo) {
			t.Fatalf("ParseSource(%q) got %v", raw, err)
		}
	}
}

func TestHTTPSFetchIsExplicitAndBounded(t *testing.T) {
	source, err := ParseSource("https://example.com/file.torrent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), source, nil); !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("nil fetch got %v", err)
	}
	called := false
	_, err = Inspect(context.Background(), source, func(_ context.Context, got *url.URL, limit int64) ([]byte, error) {
		called = true
		if got.String() != source.Value || limit != MaxSourceBytes {
			t.Fatalf("fetch got %q limit %d", got, limit)
		}
		return make([]byte, MaxSourceBytes+1), nil
	})
	if !called || !errors.Is(err, ErrSourceTooLarge) {
		t.Fatalf("called=%v err=%v", called, err)
	}

	bad := Source{Kind: SourceHTTPS, Value: "https://127.0.0.1/file.torrent"}
	called = false
	_, err = Inspect(context.Background(), bad, func(context.Context, *url.URL, int64) ([]byte, error) {
		called = true
		return nil, nil
	})
	if called || !errors.Is(err, ErrInvalidSource) {
		t.Fatalf("unsafe callback called=%v err=%v", called, err)
	}
}

func TestLocalInspectionAndSourcePrivacy(t *testing.T) {
	raw := fixtureV1(t, metainfo.Info{PieceLength: 1, Pieces: make([]byte, 20), Name: "file", Length: 1}, nil)
	path := filepath.Join(t.TempDir(), "fixture.torrent")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := ParseSource(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), source, nil); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(path)) {
		t.Fatalf("source path leaked in JSON: %s", encoded)
	}
}

func TestSourceClassification(t *testing.T) {
	tests := []struct {
		input string
		kind  SourceKind
	}{
		{"relative.torrent", SourceFile},
		{"C:\\downloads\\file.torrent", SourceFile},
		{"https://example.com/file", SourceHTTPS},
	}
	for _, test := range tests {
		got, err := ParseSource(test.input)
		if err != nil || got.Kind != test.kind {
			t.Fatalf("ParseSource(%q) = %#v, %v", test.input, got, err)
		}
	}
	for _, input := range []string{"file.txt", "http://example.com/file.torrent", "file:///tmp/file.torrent", "https://localhost/file.torrent"} {
		if _, err := ParseSource(input); !errors.Is(err, ErrInvalidSource) {
			t.Fatalf("ParseSource(%q) got %v", input, err)
		}
	}
}

func TestLowercaseBase32Magnet(t *testing.T) {
	source, err := ParseSource("magnet:?xt=urn:btih:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Inspect(context.Background(), source, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.InfoHashV1 != strings.Repeat("00", 20) {
		t.Fatalf("got infohash %q", got.InfoHashV1)
	}
}

func fixtureV1(t testing.TB, info metainfo.Info, mutate func(*metainfo.MetaInfo)) []byte {
	t.Helper()
	infoBytes, err := bencode.Marshal(&info)
	if err != nil {
		t.Fatal(err)
	}
	meta := metainfo.MetaInfo{InfoBytes: infoBytes}
	if mutate != nil {
		mutate(&meta)
	}
	raw, err := bencode.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func FuzzPreflightBencode(f *testing.F) {
	f.Add([]byte("d4:infod4:name4:file6:lengthi0e12:piece lengthi16384e6:pieces0:ee"))
	f.Add([]byte("d4:info5:shorte"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 1<<20 {
			t.Skip()
		}
		_, _ = preflightBencode(context.Background(), raw)
	})
}

func FuzzParseSource(f *testing.F) {
	f.Add("magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567")
	f.Add("https://example.com/file.torrent")
	f.Add("local.torrent")
	f.Fuzz(func(t *testing.T, input string) {
		_, _ = ParseSource(input)
	})
}

func FuzzInspectMetainfo(f *testing.F) {
	f.Add([]byte("d4:infod6:lengthi0e4:name4:file12:piece lengthi16384e6:pieces0:ee"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		if int64(len(raw)) > MaxSourceBytes {
			t.Skip()
		}
		_, _ = inspectMetainfo(context.Background(), raw, SourceFile)
	})
}

func FuzzSafePathComponent(f *testing.F) {
	f.Add("payload.bin")
	f.Add("../escape")
	f.Add("COM¹.txt")
	f.Fuzz(func(t *testing.T, component string) {
		_, _ = safePathComponent(component)
	})
}

var benchmarkInspection Inspection

func BenchmarkBencodePreflight(b *testing.B) {
	raw := fixtureV1(b, metainfo.Info{PieceLength: 16 << 10, Pieces: make([]byte, 20), Name: "payload.bin", Length: 1}, nil)
	b.ReportAllocs()
	for b.Loop() {
		_, _ = preflightBencode(context.Background(), raw)
	}
}

func BenchmarkInspectMagnet(b *testing.B) {
	raw := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Public+data"
	b.ReportAllocs()
	for b.Loop() {
		benchmarkInspection, _ = inspectMagnet(raw)
	}
}

func BenchmarkInspectV1(b *testing.B) {
	raw := fixtureV1(b, metainfo.Info{PieceLength: 16 << 10, Pieces: make([]byte, 20), Name: "payload.bin", Length: 1}, nil)
	benchmarkMetainfo(b, raw)
}

func BenchmarkInspectV2(b *testing.B) {
	root := strings.Repeat("r", 32)
	raw := fixtureV1(b, metainfo.Info{
		PieceLength: 16 << 10,
		Name:        "bundle",
		MetaVersion: 2,
		FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
			"payload.bin": {File: metainfo.FileTreeFile{Length: 1, PiecesRoot: root}},
		}},
	}, nil)
	benchmarkMetainfo(b, raw)
}

func BenchmarkInspectHybrid(b *testing.B) {
	root := strings.Repeat("r", 32)
	raw := fixtureV1(b, metainfo.Info{
		PieceLength: 16 << 10,
		Pieces:      make([]byte, 20),
		Name:        "bundle",
		Files:       []metainfo.FileInfo{{Length: 1, Path: []string{"payload.bin"}}},
		MetaVersion: 2,
		FileTree: metainfo.FileTree{Dir: map[string]metainfo.FileTree{
			"payload.bin": {File: metainfo.FileTreeFile{Length: 1, PiecesRoot: root}},
		}},
	}, nil)
	benchmarkMetainfo(b, raw)
}

func BenchmarkInspectMetadataHeavy(b *testing.B) {
	files := make([]metainfo.FileInfo, 512)
	for i := range files {
		files[i] = metainfo.FileInfo{Length: 1, Path: []string{"files", strconv.Itoa(i) + ".bin"}}
	}
	raw := fixtureV1(b, metainfo.Info{
		PieceLength: 16 << 10,
		Pieces:      make([]byte, 20),
		Name:        "bundle",
		Files:       files,
	}, nil)
	benchmarkMetainfo(b, raw)
}

func benchmarkMetainfo(b *testing.B, raw []byte) {
	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		var err error
		benchmarkInspection, err = inspectMetainfo(context.Background(), raw, SourceFile)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidatePaths(b *testing.B) {
	files := []File{
		{Path: "docs/readme.txt", Size: 1},
		{Path: "media/video.mp4", Size: 2},
		{Path: "src/main.go", Size: 3},
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = validateDistinctPaths(files)
	}
}
