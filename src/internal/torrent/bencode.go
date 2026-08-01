package torrent

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
)

const (
	maxBencodeDepth     = 64
	maxContainerMembers = 100_000
	maxBencodeValues    = 1_000_000
	maxFiles            = 100_000
	maxPathComponents   = 1_000_000
)

type infoShape struct {
	length      bool
	files       bool
	pieces      bool
	fileTree    bool
	metaVersion bool
}

type preflightResult struct {
	infoStart   int
	infoEnd     int
	pieceLayers bool
	shape       infoShape
}

type bencodeContext uint8

const (
	contextOther bencodeContext = iota
	contextTop
	contextInfo
	contextFileTreeRoot
	contextFileTree
	contextFileProperties
)

type bencodeFrame struct {
	kind         byte
	context      bencodeContext
	start        int
	members      int
	expectingKey bool
	key          []byte
	lastKey      []byte
	hasProperty  bool
	hasChild     bool
	hasLength    bool
	length       int64
	hasRoot      bool
	rootLength   int
}

type bencodeValue struct {
	kind       byte
	start      int
	end        int
	integer    int64
	stringData []byte
}

type bencodeParser struct {
	data           []byte
	position       int
	values         int
	infoFields     int
	fileCount      int
	pathComponents int
	stack          []bencodeFrame
	result         preflightResult
	rootComplete   bool
}

func preflightBencode(ctx context.Context, data []byte) (preflightResult, error) {
	p := bencodeParser{data: data}
	if len(data) == 0 {
		return preflightResult{}, invalidMetainfo("empty bencode input")
	}

	for !p.rootComplete {
		if p.values&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return preflightResult{}, err
			}
		}
		if p.position >= len(data) {
			return preflightResult{}, invalidMetainfo("unterminated bencode value")
		}

		if len(p.stack) != 0 {
			top := &p.stack[len(p.stack)-1]
			if top.kind == 'd' && top.expectingKey {
				if p.data[p.position] == 'e' {
					if err := p.closeContainer(); err != nil {
						return preflightResult{}, err
					}
					continue
				}
				if err := p.readKey(top); err != nil {
					return preflightResult{}, err
				}
				continue
			}
			if top.kind == 'l' && p.data[p.position] == 'e' {
				if err := p.closeContainer(); err != nil {
					return preflightResult{}, err
				}
				continue
			}
			if top.kind == 'd' && p.data[p.position] == 'e' {
				return preflightResult{}, invalidMetainfo("dictionary key has no value")
			}
		}

		value, complete, err := p.readValue()
		if err != nil {
			return preflightResult{}, err
		}
		if complete {
			if err := p.completeValue(value); err != nil {
				return preflightResult{}, err
			}
		}
	}

	if p.position != len(data) {
		return preflightResult{}, invalidMetainfo("trailing bytes after top-level dictionary")
	}
	if p.infoFields != 1 {
		return preflightResult{}, invalidMetainfo("top-level dictionary must contain exactly one info field")
	}
	if err := ctx.Err(); err != nil {
		return preflightResult{}, err
	}
	return p.result, nil
}

func (p *bencodeParser) readValue() (bencodeValue, bool, error) {
	start := p.position
	switch p.data[p.position] {
	case 'd', 'l':
		kind := p.data[p.position]
		context, err := p.childContext(kind)
		if err != nil {
			return bencodeValue{}, false, err
		}
		if err := p.addValue(); err != nil {
			return bencodeValue{}, false, err
		}
		p.position++
		p.stack = append(p.stack, bencodeFrame{
			kind:         kind,
			context:      context,
			start:        start,
			expectingKey: kind == 'd',
		})
		if len(p.stack) > maxBencodeDepth {
			return bencodeValue{}, false, invalidMetainfo("bencode nesting exceeds %d", maxBencodeDepth)
		}
		return bencodeValue{}, false, nil
	case 'i':
		integer, end, err := parseCanonicalInteger(p.data, p.position)
		if err != nil {
			return bencodeValue{}, false, err
		}
		if err := p.addValue(); err != nil {
			return bencodeValue{}, false, err
		}
		p.position = end
		return bencodeValue{kind: 'i', start: start, end: end, integer: integer}, true, nil
	case 'e':
		return bencodeValue{}, false, invalidMetainfo("unexpected bencode terminator")
	default:
		contents, end, err := parseCanonicalString(p.data, p.position)
		if err != nil {
			return bencodeValue{}, false, err
		}
		if err := p.addValue(); err != nil {
			return bencodeValue{}, false, err
		}
		p.position = end
		return bencodeValue{kind: 's', start: start, end: end, stringData: contents}, true, nil
	}
}

func (p *bencodeParser) childContext(kind byte) (bencodeContext, error) {
	if len(p.stack) == 0 {
		if kind != 'd' || p.position != 0 {
			return 0, invalidMetainfo("top-level bencode value must be a dictionary")
		}
		return contextTop, nil
	}

	parent := &p.stack[len(p.stack)-1]
	switch parent.context {
	case contextTop:
		if bytes.Equal(parent.key, []byte("info")) {
			if kind != 'd' {
				return 0, invalidMetainfo("info must be a dictionary")
			}
			return contextInfo, nil
		}
	case contextInfo:
		if bytes.Equal(parent.key, []byte("file tree")) {
			if kind != 'd' {
				return 0, invalidMetainfo("file tree must be a dictionary")
			}
			return contextFileTreeRoot, nil
		}
	case contextFileTreeRoot, contextFileTree:
		if kind != 'd' {
			return 0, invalidMetainfo("file tree entries must be dictionaries")
		}
		if len(parent.key) == 0 {
			return contextFileProperties, nil
		}
		return contextFileTree, nil
	}
	return contextOther, nil
}

func (p *bencodeParser) readKey(frame *bencodeFrame) error {
	key, end, err := parseCanonicalString(p.data, p.position)
	if err != nil {
		return fmt.Errorf("dictionary key: %w", err)
	}
	if err := p.addValue(); err != nil {
		return err
	}
	if frame.lastKey != nil && bytes.Compare(frame.lastKey, key) >= 0 {
		return invalidMetainfo("dictionary keys are not unique and strictly sorted")
	}
	frame.lastKey = key
	frame.key = key
	frame.expectingKey = false
	p.position = end

	switch frame.context {
	case contextTop:
		if bytes.Equal(key, []byte("info")) {
			p.infoFields++
		}
	case contextFileTreeRoot, contextFileTree:
		if len(key) == 0 {
			frame.hasProperty = true
		} else {
			if _, err := safePathComponent(string(key)); err != nil {
				return fmt.Errorf("file tree component: %w", err)
			}
			frame.hasChild = true
			p.pathComponents++
			if p.pathComponents > maxPathComponents {
				return invalidMetainfo("file tree exceeds %d path components", maxPathComponents)
			}
		}
	}
	return nil
}

func (p *bencodeParser) completeValue(value bencodeValue) error {
	if len(p.stack) == 0 {
		if value.kind != 'd' || value.start != 0 {
			return invalidMetainfo("top-level bencode value must be a dictionary")
		}
		p.rootComplete = true
		return nil
	}

	parent := &p.stack[len(p.stack)-1]
	if parent.kind == 'd' && parent.expectingKey {
		return invalidMetainfo("dictionary value appeared without a key")
	}
	if err := p.validateField(parent, value); err != nil {
		return err
	}
	parent.members++
	if parent.members > maxContainerMembers {
		return invalidMetainfo("container exceeds %d members", maxContainerMembers)
	}
	if parent.kind == 'd' {
		if parent.context == contextTop && bytes.Equal(parent.key, []byte("info")) {
			p.result.infoStart, p.result.infoEnd = value.start, value.end
		}
		parent.expectingKey = true
		parent.key = nil
	}
	return nil
}

func (p *bencodeParser) validateField(parent *bencodeFrame, value bencodeValue) error {
	if parent.kind != 'd' {
		return nil
	}

	switch parent.context {
	case contextTop:
		if bytes.Equal(parent.key, []byte("info")) {
			if value.kind != 'd' {
				return invalidMetainfo("info must be a dictionary")
			}
		} else if bytes.Equal(parent.key, []byte("piece layers")) {
			if value.kind != 'd' {
				return invalidMetainfo("piece layers must be a dictionary")
			}
			p.result.pieceLayers = true
		}
	case contextInfo:
		name := string(parent.key)
		expected := byte(0)
		switch name {
		case "length", "meta version", "piece length", "private":
			expected = 'i'
		case "name", "name.utf-8", "pieces":
			expected = 's'
		case "files":
			expected = 'l'
		case "file tree":
			expected = 'd'
		}
		if expected != 0 && value.kind != expected {
			return invalidMetainfo("info field %q has the wrong bencode type", name)
		}
		switch name {
		case "length":
			p.result.shape.length = true
		case "files":
			p.result.shape.files = true
		case "pieces":
			p.result.shape.pieces = true
		case "file tree":
			p.result.shape.fileTree = true
		case "meta version":
			p.result.shape.metaVersion = true
		case "private":
			if value.integer != 0 && value.integer != 1 {
				return invalidMetainfo("private must be 0 or 1")
			}
		}
	case contextFileProperties:
		switch string(parent.key) {
		case "length":
			if value.kind != 'i' || value.integer < 0 {
				return invalidMetainfo("v2 file length must be a nonnegative integer")
			}
			parent.hasLength, parent.length = true, value.integer
		case "pieces root":
			if value.kind != 's' {
				return invalidMetainfo("v2 pieces root must be a byte string")
			}
			parent.hasRoot, parent.rootLength = true, len(value.stringData)
		}
	case contextFileTreeRoot, contextFileTree:
		if value.kind != 'd' {
			return invalidMetainfo("file tree entries must be dictionaries")
		}
	}
	return nil
}

func (p *bencodeParser) closeContainer() error {
	last := len(p.stack) - 1
	frame := p.stack[last]
	if frame.context == contextFileProperties {
		p.fileCount++
		if p.fileCount > maxFiles {
			return invalidMetainfo("v2 file count exceeds %d", maxFiles)
		}
	}
	if err := validateClosedFrame(frame); err != nil {
		return err
	}
	p.position++
	p.stack = p.stack[:last]
	return p.completeValue(bencodeValue{kind: frame.kind, start: frame.start, end: p.position})
}

func validateClosedFrame(frame bencodeFrame) error {
	switch frame.context {
	case contextFileTreeRoot:
		if frame.hasProperty || !frame.hasChild {
			return invalidMetainfo("v2 file tree root must contain files or directories")
		}
	case contextFileTree:
		if frame.hasProperty == frame.hasChild {
			return invalidMetainfo("v2 file tree node must be exactly one file or directory (property=%t children=%t)", frame.hasProperty, frame.hasChild)
		}
	case contextFileProperties:
		if !frame.hasLength {
			return invalidMetainfo("v2 file entry is missing length")
		}
		if frame.length == 0 && frame.hasRoot {
			return invalidMetainfo("empty v2 file must not contain pieces root")
		}
		if frame.length > 0 && (!frame.hasRoot || frame.rootLength != 32) {
			return invalidMetainfo("nonempty v2 file pieces root must be exactly 32 bytes")
		}
	}
	return nil
}

func (p *bencodeParser) addValue() error {
	p.values++
	if p.values > maxBencodeValues {
		return invalidMetainfo("bencode input exceeds %d values", maxBencodeValues)
	}
	return nil
}

func parseCanonicalInteger(data []byte, start int) (int64, int, error) {
	endOffset := bytes.IndexByte(data[start+1:], 'e')
	if endOffset < 0 {
		return 0, 0, invalidMetainfo("unterminated bencode integer")
	}
	end := start + 1 + endOffset
	digits := data[start+1 : end]
	if len(digits) == 0 || digits[0] == '+' || len(digits) > 1 && digits[0] == '0' || len(digits) > 1 && digits[0] == '-' && digits[1] == '0' {
		return 0, 0, invalidMetainfo("non-canonical bencode integer")
	}
	value, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return 0, 0, invalidMetainfo("invalid bencode integer")
	}
	return value, end + 1, nil
}

func parseCanonicalString(data []byte, start int) ([]byte, int, error) {
	if start >= len(data) || data[start] < '0' || data[start] > '9' {
		return nil, 0, invalidMetainfo("expected bencode byte string")
	}
	colonOffset := bytes.IndexByte(data[start:], ':')
	if colonOffset < 0 {
		return nil, 0, invalidMetainfo("unterminated bencode string length")
	}
	colon := start + colonOffset
	digits := data[start:colon]
	if len(digits) > 1 && digits[0] == '0' {
		return nil, 0, invalidMetainfo("non-canonical bencode string length")
	}
	length, err := strconv.Atoi(string(digits))
	if err != nil || length > len(data)-colon-1 {
		return nil, 0, invalidMetainfo("invalid bencode string length")
	}
	end := colon + 1 + length
	return data[colon+1 : end], end, nil
}
