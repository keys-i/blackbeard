// Package torrent validates and inspects direct BitTorrent metadata sources.
package torrent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/keys-i/blackbeard/src/internal/termtext"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const (
	MaxSourceBytes      int64 = 16 << 20
	maxMagnetBytes            = 4096
	maxMagnetParameters       = 64
	maxMagnetKeyBytes         = 64
	maxMagnetValueBytes       = 2048
	maxDisplayNameBytes       = 1024
	maxCommentBytes           = 4096
	maxCreatedByBytes         = 512
	maxTrackers               = 256
	maxWebseeds               = 256
	maxURLBytes               = 4096
	maxPathDepth              = 64
	maxPathComponent          = 255
	maxPathBytes              = 4096
	maxV2PieceLength    int64 = 16 << 20
)

var (
	ErrInvalidSource   = errors.New("invalid torrent source")
	ErrInvalidMetainfo = errors.New("invalid torrent metainfo")
	ErrSourceTooLarge  = errors.New("torrent source exceeds size limit")
)

type SourceKind string

const (
	SourceMagnet SourceKind = "magnet"
	SourceFile   SourceKind = "torrent_file"
	SourceHTTPS  SourceKind = "torrent_https"
)

type Source struct {
	Kind  SourceKind `json:"kind"`
	Value string     `json:"-"`
}

type Fetch func(context.Context, *url.URL, int64) ([]byte, error)

type Inspection struct {
	Source            SourceKind `json:"source_type"`
	MetadataAvailable bool       `json:"metadata_available"`
	Name              string     `json:"name,omitempty"`
	Format            string     `json:"format,omitempty"`
	InfoHashV1        string     `json:"infohash_v1,omitempty"`
	InfoHashV2        string     `json:"infohash_v2,omitempty"`
	TotalSize         *int64     `json:"total_size,omitempty"`
	PieceLength       *int64     `json:"piece_length,omitempty"`
	Private           *bool      `json:"private,omitempty"`
	Files             []File     `json:"files"`
	Trackers          []string   `json:"trackers"`
	Webseeds          []string   `json:"webseeds"`
	Comment           string     `json:"comment,omitempty"`
	CreatedBy         string     `json:"created_by,omitempty"`
}

type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func ParseSource(input string) (Source, error) {
	if input == "" || unsafeText(input) {
		return Source{}, invalidSource("source is empty or contains control characters")
	}
	if len(input) <= maxMagnetBytes && strings.HasPrefix(input, "magnet:") {
		if _, err := inspectMagnet(input); err != nil {
			return Source{}, err
		}
		return Source{Kind: SourceMagnet, Value: input}, nil
	}
	if strings.HasPrefix(input, "magnet:") {
		return Source{}, invalidSource("magnet URI exceeds %d bytes", maxMagnetBytes)
	}

	if parsed, err := url.Parse(input); err == nil && parsed.Scheme != "" && !isWindowsPath(input) {
		if parsed.Scheme != "https" {
			return Source{}, invalidSource("source URL must use HTTPS")
		}
		if err := validateHTTPSURL(parsed); err != nil {
			return Source{}, err
		}
		return Source{Kind: SourceHTTPS, Value: parsed.String()}, nil
	}
	if !strings.EqualFold(filepath.Ext(input), ".torrent") {
		return Source{}, invalidSource("local source must have a .torrent extension")
	}
	return Source{Kind: SourceFile, Value: input}, nil
}

func Inspect(ctx context.Context, source Source, fetch Fetch) (Inspection, error) {
	parsed, err := ParseSource(source.Value)
	if err != nil {
		return Inspection{}, err
	}
	if parsed.Kind != source.Kind {
		return Inspection{}, invalidSource("source kind %q does not match value", source.Kind)
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}

	if parsed.Kind == SourceMagnet {
		return inspectMagnet(parsed.Value)
	}

	var raw []byte
	switch parsed.Kind {
	case SourceFile:
		raw, err = readLocal(ctx, parsed.Value)
	case SourceHTTPS:
		if fetch == nil {
			return Inspection{}, invalidSource("HTTPS source requires a fetch callback")
		}
		remote, parseErr := url.Parse(parsed.Value)
		if parseErr != nil {
			return Inspection{}, invalidSource("invalid HTTPS source")
		}
		raw, err = fetch(ctx, remote, MaxSourceBytes)
		if err == nil && int64(len(raw)) > MaxSourceBytes {
			err = ErrSourceTooLarge
		}
	}
	if err != nil {
		return Inspection{}, err
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	return inspectMetainfo(ctx, raw, parsed.Kind)
}

func readLocal(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, redactedPathError("stat torrent source", err)
	}
	if !before.Mode().IsRegular() {
		return nil, invalidSource("local source is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, redactedPathError("open torrent source", err)
	}
	defer func() { _ = file.Close() }()
	stat, err := file.Stat()
	if err != nil {
		return nil, redactedPathError("stat torrent source", err)
	}
	if !stat.Mode().IsRegular() || !os.SameFile(before, stat) {
		return nil, invalidSource("local source changed or is not a regular file")
	}
	if stat.Size() > MaxSourceBytes {
		return nil, ErrSourceTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, MaxSourceBytes+1))
	if err != nil {
		return nil, redactedPathError("read torrent source", err)
	}
	if int64(len(raw)) > MaxSourceBytes {
		return nil, ErrSourceTooLarge
	}
	return raw, nil
}

func inspectMagnet(raw string) (Inspection, error) {
	if len(raw) > maxMagnetBytes || unsafeText(raw) {
		return Inspection{}, invalidSource("invalid magnet URI")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "magnet" || u.Host != "" || u.Path != "" || u.Opaque != "" || u.Fragment != "" || u.User != nil {
		return Inspection{}, invalidSource("magnet URI must not contain authority, path, or fragment components")
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return Inspection{}, invalidSource("invalid magnet query: %v", err)
	}
	count := 0
	for key, entries := range values {
		if key == "" || len(key) > maxMagnetKeyBytes || unsafeText(key) {
			return Inspection{}, invalidSource("invalid magnet parameter name")
		}
		for _, value := range entries {
			count++
			if count > maxMagnetParameters || len(value) > maxMagnetValueBytes || unsafeText(value) {
				return Inspection{}, invalidSource("magnet parameters exceed limits")
			}
		}
	}
	if len(values["dn"]) > 1 || len(values["tr"]) > maxTrackers || len(values["ws"]) > maxWebseeds {
		return Inspection{}, invalidSource("magnet contains too many repeated fields")
	}
	v1, v2, err := validateExactTopics(values["xt"])
	if err != nil {
		return Inspection{}, err
	}

	trackers, err := cleanURLs(values["tr"], maxTrackers, true)
	if err != nil {
		return Inspection{}, invalidSource("invalid magnet tracker: %v", err)
	}
	webseeds, err := cleanURLs(values["ws"], maxWebseeds, false)
	if err != nil {
		return Inspection{}, invalidSource("invalid magnet webseed: %v", err)
	}
	name := ""
	if len(values["dn"]) != 0 {
		name = values["dn"][0]
	}
	return Inspection{
		Source:            SourceMagnet,
		MetadataAvailable: false,
		Name:              termtext.Sanitize(name, maxDisplayNameBytes),
		InfoHashV1:        v1,
		InfoHashV2:        v2,
		Files:             []File{},
		Trackers:          trackers,
		Webseeds:          webseeds,
	}, nil
}

func validateExactTopics(topics []string) (string, string, error) {
	var v1, v2 string
	for _, topic := range topics {
		switch {
		case strings.HasPrefix(topic, "urn:btih:"):
			if v1 != "" {
				return "", "", invalidSource("magnet contains duplicate v1 infohashes")
			}
			encoded := strings.TrimPrefix(topic, "urn:btih:")
			var decoded []byte
			var err error
			switch len(encoded) {
			case 40:
				decoded, err = hex.DecodeString(encoded)
			case 32:
				decoded, err = base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(encoded))
			default:
				err = errors.New("wrong length")
			}
			if err != nil || len(decoded) != 20 {
				return "", "", invalidSource("invalid v1 magnet infohash")
			}
			v1 = hex.EncodeToString(decoded)
		case strings.HasPrefix(topic, "urn:btmh:"):
			if v2 != "" {
				return "", "", invalidSource("magnet contains duplicate v2 infohashes")
			}
			encoded := strings.TrimPrefix(topic, "urn:btmh:")
			decoded, err := hex.DecodeString(encoded)
			if err != nil || len(decoded) != 34 || decoded[0] != 0x12 || decoded[1] != 0x20 {
				return "", "", invalidSource("invalid v2 magnet infohash")
			}
			v2 = hex.EncodeToString(decoded[2:])
		default:
			return "", "", invalidSource("magnet contains an unrecognized exact topic")
		}
	}
	if v1 == "" && v2 == "" {
		return "", "", invalidSource("magnet is missing a recognized v1 or v2 infohash")
	}
	return v1, v2, nil
}

func inspectMetainfo(ctx context.Context, raw []byte, kind SourceKind) (Inspection, error) {
	if int64(len(raw)) > MaxSourceBytes {
		return Inspection{}, ErrSourceTooLarge
	}
	preflight, err := preflightBencode(ctx, raw)
	if err != nil {
		return Inspection{}, err
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	meta, err := metainfo.Load(bytes.NewReader(raw))
	if err != nil {
		return Inspection{}, invalidMetainfo("decode metainfo: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	if !bytes.Equal(meta.InfoBytes, raw[preflight.infoStart:preflight.infoEnd]) {
		return Inspection{}, invalidMetainfo("decoded info dictionary does not match canonical bytes")
	}
	info, err := meta.UnmarshalInfo()
	if err != nil {
		return Inspection{}, invalidMetainfo("decode info dictionary: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return Inspection{}, err
	}
	if info.MetaVersion != 0 && info.MetaVersion != 2 {
		return Inspection{}, invalidMetainfo("unsupported meta version %d", info.MetaVersion)
	}
	if preflight.shape.metaVersion != (info.MetaVersion == 2) {
		return Inspection{}, invalidMetainfo("meta version and v2 structure disagree")
	}
	if info.PieceLength <= 0 {
		return Inspection{}, invalidMetainfo("piece length must be positive")
	}
	name, err := safePathComponent(info.Name)
	if err != nil {
		return Inspection{}, fmt.Errorf("torrent name: %w", err)
	}
	if info.NameUtf8 != "" {
		utf8Name, err := safePathComponent(info.NameUtf8)
		if err != nil {
			return Inspection{}, fmt.Errorf("UTF-8 torrent name: %w", err)
		}
		if name != utf8Name {
			return Inspection{}, invalidMetainfo("legacy and UTF-8 torrent names disagree")
		}
	}

	hasV2 := info.MetaVersion == 2
	hasV1 := preflight.shape.length || preflight.shape.files || preflight.shape.pieces
	if !hasV1 && !hasV2 {
		return Inspection{}, invalidMetainfo("info dictionary has neither v1 nor v2 metadata")
	}
	if hasV2 && (!preflight.shape.fileTree || info.PieceLength < 16<<10 || info.PieceLength > maxV2PieceLength || info.PieceLength&(info.PieceLength-1) != 0) {
		return Inspection{}, invalidMetainfo("v2 piece length must be a power of two from 16 KiB through 16 MiB")
	}
	if !hasV2 && preflight.shape.fileTree {
		return Inspection{}, invalidMetainfo("v1 metainfo must not contain a v2 file tree")
	}

	var v1Files, v2Files []File
	var v1Total, v2Total int64
	if hasV1 {
		v1Files, v1Total, err = validateV1(info, preflight.shape)
		if err != nil {
			return Inspection{}, err
		}
	}
	var roots map[string]int64
	if hasV2 {
		v2Files, v2Total, roots, err = validateV2(info)
		if err != nil {
			return Inspection{}, err
		}
		if err := validatePieceLayers(meta.PieceLayers, roots, &info); err != nil {
			return Inspection{}, err
		}
	}
	if hasV1 && hasV2 {
		if err := validateHybrid(info, preflight.shape, v1Files, v2Files); err != nil {
			return Inspection{}, err
		}
	}

	files, total, format := v1Files, v1Total, "v1"
	if hasV2 {
		files, total, format = v2Files, v2Total, "v2"
		if hasV1 {
			format = "hybrid"
		}
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	trackers, err := cleanURLs(meta.UpvertedAnnounceList().DistinctValues(), maxTrackers, true)
	if err != nil {
		return Inspection{}, invalidMetainfo("invalid tracker URL: %v", err)
	}
	webseeds, err := cleanURLs(meta.UrlList, maxWebseeds, false)
	if err != nil {
		return Inspection{}, invalidMetainfo("invalid webseed URL: %v", err)
	}
	private := info.Private != nil && *info.Private
	pieceLength := info.PieceLength
	inspection := Inspection{
		Source:            kind,
		MetadataAvailable: true,
		Name:              termtext.Sanitize(info.BestName(), maxDisplayNameBytes),
		Format:            format,
		TotalSize:         &total,
		PieceLength:       &pieceLength,
		Private:           &private,
		Files:             files,
		Trackers:          trackers,
		Webseeds:          webseeds,
		Comment:           termtext.Sanitize(meta.Comment, maxCommentBytes),
		CreatedBy:         termtext.Sanitize(meta.CreatedBy, maxCreatedByBytes),
	}
	if hasV1 {
		inspection.InfoHashV1 = meta.HashInfoBytes().HexString()
	}
	if hasV2 {
		hash := sha256.Sum256(meta.InfoBytes)
		inspection.InfoHashV2 = hex.EncodeToString(hash[:])
	}
	return inspection, nil
}

func validateV1(info metainfo.Info, shape infoShape) ([]File, int64, error) {
	if !shape.pieces || shape.length == shape.files {
		return nil, 0, invalidMetainfo("v1 metadata requires pieces and exactly one of length or files")
	}
	if len(info.Pieces)%20 != 0 {
		return nil, 0, invalidMetainfo("v1 pieces length is not a multiple of 20")
	}
	files := make([]File, 0, max(1, len(info.Files)))
	var total int64
	if shape.length {
		if info.Length < 0 {
			return nil, 0, invalidMetainfo("file length must not be negative")
		}
		if strings.Contains(info.Attr, "l") || len(info.SymlinkPath) != 0 {
			return nil, 0, invalidMetainfo("symlink entries are not supported")
		}
		if strings.Contains(info.Attr, "p") {
			return nil, 0, invalidMetainfo("a single-file torrent cannot be a padding file")
		}
		path, err := safePathComponent(info.BestName())
		if err != nil {
			return nil, 0, err
		}
		files = append(files, File{Path: path, Size: info.Length})
		total = info.Length
	} else {
		if len(info.Files) == 0 || len(info.Files) > maxFiles {
			return nil, 0, invalidMetainfo("v1 file count must be within 1..%d", maxFiles)
		}
		for _, file := range info.Files {
			if file.Length < 0 {
				return nil, 0, invalidMetainfo("file length must not be negative")
			}
			if strings.Contains(file.Attr, "l") || len(file.SymlinkPath) != 0 {
				return nil, 0, invalidMetainfo("symlink entries are not supported")
			}
			path, err := chooseV1Path(file)
			if err != nil {
				return nil, 0, err
			}
			if total > math.MaxInt64-file.Length {
				return nil, 0, invalidMetainfo("total file size overflows int64")
			}
			total += file.Length
			files = append(files, File{Path: path, Size: file.Length})
		}
	}
	expectedPieces := int64(0)
	if total != 0 {
		expectedPieces = (total-1)/info.PieceLength + 1
	}
	if int64(len(info.Pieces)/20) != expectedPieces {
		return nil, 0, invalidMetainfo("v1 piece count does not match total size")
	}
	if err := validateDistinctPaths(files); err != nil {
		return nil, 0, err
	}
	return files, total, nil
}

func validateV2(info metainfo.Info) ([]File, int64, map[string]int64, error) {
	type item struct {
		tree *metainfo.FileTree
		path []string
		root bool
	}
	stack := []item{{tree: &info.FileTree, root: true}}
	files := make([]File, 0)
	roots := make(map[string]int64)
	var total int64
	for len(stack) != 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		if len(current.tree.Dir) != 0 {
			if current.tree.File.Length != 0 || current.tree.File.PiecesRoot != "" {
				return nil, 0, nil, invalidMetainfo("v2 node mixes file properties and children")
			}
			keys := make([]string, 0, len(current.tree.Dir))
			for key := range current.tree.Dir {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for i := len(keys) - 1; i >= 0; i-- {
				component, err := safePathComponent(keys[i])
				if err != nil {
					return nil, 0, nil, err
				}
				child := current.tree.Dir[keys[i]]
				path := append(slices.Clone(current.path), component)
				stack = append(stack, item{tree: &child, path: path})
			}
			continue
		}
		if current.root || len(current.path) == 0 {
			return nil, 0, nil, invalidMetainfo("v2 file tree contains an empty root")
		}
		length := current.tree.File.Length
		root := current.tree.File.PiecesRoot
		if length < 0 || length == 0 && root != "" || length > 0 && len(root) != 32 {
			return nil, 0, nil, invalidMetainfo("invalid v2 file length or pieces root")
		}
		if total > math.MaxInt64-length {
			return nil, 0, nil, invalidMetainfo("total file size overflows int64")
		}
		total += length
		path, err := safePath(current.path)
		if err != nil {
			return nil, 0, nil, err
		}
		files = append(files, File{Path: path, Size: length})
		if len(files) > maxFiles {
			return nil, 0, nil, invalidMetainfo("v2 file count exceeds %d", maxFiles)
		}
		if root != "" {
			if previous, ok := roots[root]; ok && previous != length {
				return nil, 0, nil, invalidMetainfo("one pieces root describes conflicting file sizes")
			}
			roots[root] = length
		}
	}
	if err := validateDistinctPaths(files); err != nil {
		return nil, 0, nil, err
	}
	return files, total, roots, nil
}

func validatePieceLayers(layers map[string]string, roots map[string]int64, info *metainfo.Info) error {
	if len(layers) > maxFiles {
		return invalidMetainfo("piece layer count exceeds %d", maxFiles)
	}
	for root, layer := range layers {
		length, ok := roots[root]
		if len(root) != 32 || len(layer) == 0 || len(layer)%32 != 0 || !ok || length <= info.PieceLength {
			return invalidMetainfo("invalid or unreferenced v2 piece layer")
		}
		if int64(len(layer)/32) != (length-1)/info.PieceLength+1 {
			return invalidMetainfo("v2 piece layer hash count does not match file size")
		}
	}
	if err := metainfo.ValidatePieceLayers(layers, &info.FileTree, info.PieceLength); err != nil {
		return invalidMetainfo("invalid v2 piece layers: %v", err)
	}
	return nil
}

func validateHybrid(info metainfo.Info, shape infoShape, v1, v2 []File) error {
	if shape.length {
		if len(v1) != 1 || len(v2) != 1 || v1[0] != v2[0] {
			return invalidMetainfo("hybrid v1 and v2 file lists differ")
		}
		return nil
	}

	var offset int64
	real := 0
	for _, file := range info.Files {
		if strings.Contains(file.Attr, "p") {
			gap := (info.PieceLength - offset%info.PieceLength) % info.PieceLength
			if gap == 0 || file.Length != gap || !hasLaterNonemptyFile(v2[real:]) {
				return invalidMetainfo("hybrid padding does not match v2 piece alignment")
			}
			offset += file.Length
			continue
		}
		if real >= len(v2) {
			return invalidMetainfo("hybrid v1 and v2 file lists differ")
		}
		path, err := chooseV1Path(file)
		if err != nil {
			return err
		}
		if file.Length > 0 && offset%info.PieceLength != 0 || (File{Path: path, Size: file.Length}) != v2[real] {
			return invalidMetainfo("hybrid v1 and v2 file order or alignment differs")
		}
		offset += file.Length
		real++
	}
	if real != len(v2) {
		return invalidMetainfo("hybrid v1 and v2 file lists differ")
	}
	return nil
}

func hasLaterNonemptyFile(files []File) bool {
	for _, file := range files {
		if file.Size > 0 {
			return true
		}
	}
	return false
}

func chooseV1Path(file metainfo.FileInfo) (string, error) {
	legacy, err := safePath(file.Path)
	if err != nil {
		return "", err
	}
	if len(file.PathUtf8) == 0 {
		return legacy, nil
	}
	utf8Path, err := safePath(file.PathUtf8)
	if err != nil {
		return "", err
	}
	if legacy != utf8Path {
		return "", invalidMetainfo("legacy and UTF-8 file paths disagree")
	}
	return utf8Path, nil
}

func safePath(components []string) (string, error) {
	if len(components) == 0 || len(components) > maxPathDepth {
		return "", invalidMetainfo("file path must contain 1..%d components", maxPathDepth)
	}
	clean := make([]string, len(components))
	for i, component := range components {
		value, err := safePathComponent(component)
		if err != nil {
			return "", err
		}
		clean[i] = value
	}
	path := strings.Join(clean, "/")
	if len(path) > maxPathBytes {
		return "", invalidMetainfo("file path exceeds %d bytes", maxPathBytes)
	}
	return path, nil
}

func safePathComponent(component string) (string, error) {
	if component == "" || component == "." || component == ".." || !utf8.ValidString(component) || len(component) > maxPathComponent {
		return "", invalidMetainfo("invalid file path component")
	}
	if strings.ContainsAny(component, "/\\<>:\"|?*") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") || unsafeText(component) {
		return "", invalidMetainfo("unsafe file path component %q", termtext.Sanitize(component, maxPathComponent))
	}
	base, _, _ := strings.Cut(strings.ToUpper(component), ".")
	if windowsReservedBase(base) {
		return "", invalidMetainfo("reserved file path component %q", component)
	}
	component = norm.NFC.String(component)
	if len(component) > maxPathComponent {
		return "", invalidMetainfo("normalized file path component exceeds %d bytes", maxPathComponent)
	}
	return component, nil
}

func windowsReservedBase(base string) bool {
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if !strings.HasPrefix(base, "COM") && !strings.HasPrefix(base, "LPT") {
		return false
	}
	switch base[3:] {
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
		return true
	default:
		return false
	}
}

func validateDistinctPaths(files []File) error {
	paths := make(map[string]struct{}, len(files))
	dirs := make(map[string]struct{})
	fold := cases.Fold()
	for _, file := range files {
		parts := strings.Split(file.Path, "/")
		folded := make([]string, len(parts))
		for i, part := range parts {
			folded[i] = portablePathKey(fold, part)
		}
		full := strings.Join(folded, "/")
		if _, exists := paths[full]; exists {
			return invalidMetainfo("duplicate or case-colliding file path")
		}
		if _, exists := dirs[full]; exists {
			return invalidMetainfo("file path collides with a directory")
		}
		for i := 1; i < len(folded); i++ {
			prefix := strings.Join(folded[:i], "/")
			if _, exists := paths[prefix]; exists {
				return invalidMetainfo("directory path collides with a file")
			}
			dirs[prefix] = struct{}{}
		}
		paths[full] = struct{}{}
	}
	return nil
}

func portablePathKey(fold cases.Caser, value string) string {
	if strings.IndexFunc(value, func(r rune) bool { return r >= utf8.RuneSelf }) < 0 {
		return strings.ToLower(value)
	}
	return norm.NFC.String(fold.String(value))
}

func cleanURLs(values []string, limit int, tracker bool) ([]string, error) {
	if len(values) > limit {
		return nil, fmt.Errorf("URL count exceeds %d", limit)
	}
	clean := make([]string, 0, len(values))
	for _, raw := range values {
		if len(raw) == 0 || len(raw) > maxURLBytes || unsafeText(raw) {
			return nil, errors.New("invalid tracker or webseed URL")
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return nil, errors.New("invalid tracker or webseed URL")
		}
		allowed := u.Scheme == "http" || u.Scheme == "https"
		if tracker {
			allowed = allowed || u.Scheme == "udp"
		}
		if !allowed || unsafeHost(u.Hostname()) {
			return nil, errors.New("unsafe tracker or webseed URL")
		}
		clean = append(clean, u.String())
	}
	sort.Strings(clean)
	return slices.Compact(clean), nil
}

func validateHTTPSURL(u *url.URL) error {
	if u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || len(u.String()) > maxURLBytes || unsafeHost(u.Hostname()) {
		return invalidSource("invalid or unsafe HTTPS torrent URL")
	}
	path, err := url.PathUnescape(u.EscapedPath())
	if err != nil || unsafeText(path) {
		return invalidSource("invalid HTTPS torrent URL path")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return invalidSource("invalid HTTPS torrent URL query")
	}
	for key, values := range query {
		if unsafeText(key) {
			return invalidSource("invalid HTTPS torrent URL query")
		}
		for _, value := range values {
			if unsafeText(value) {
				return invalidSource("invalid HTTPS torrent URL query")
			}
		}
	}
	return nil
}

func unsafeHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return true
	}
	host, _, _ = strings.Cut(host, "%")
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast())
}

func unsafeText(value string) bool {
	if !utf8.ValidString(value) {
		return true
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) || r == '\u2028' || r == '\u2029' {
			return true
		}
	}
	return false
}

func isWindowsPath(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' && (value[2] == '\\' || value[2] == '/')
}

func redactedPathError(operation string, err error) error {
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return fmt.Errorf("%s: %w", operation, pathError.Err)
	}
	return fmt.Errorf("%s failed", operation)
}

func invalidSource(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSource, fmt.Sprintf(format, args...))
}

func invalidMetainfo(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMetainfo, fmt.Sprintf(format, args...))
}
