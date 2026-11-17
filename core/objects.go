package main

// objects.go — port of bot/objects.ts (git object model, trees/commits/tags, packfiles).
// Standard library only. Behaviour, ordering, IDs and strings mirror the TypeScript source.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf16"
)

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

// hashObject returns the SHA-1 of "<type> <len>\x00"+data (TS hashObject).
func hashObject(typ string, data []byte) string {
	return sha1Hex(objectKey("", typ, data))
}

// objectKey returns "<type> <len>\x00"+data.
// The oid argument is intentionally unused: the TS objectKey also ignores its first
// parameter (it only exists to mirror the call shape used by the range helpers).
func objectKey(oid, typ string, data []byte) []byte {
	_ = oid
	out := make([]byte, 0, len(typ)+16+len(data))
	out = append(out, []byte(fmt.Sprintf("%s %d\x00", typ, len(data)))...)
	out = append(out, data...)
	return out
}

// jsStringLess compares strings the way JavaScript's < does: by UTF-16 code unit.
// (Go's native string comparison is UTF-8 byte order, which differs for
// supplementary characters.)
func jsStringLess(a, b string) bool {
	au := utf16.Encode([]rune(a))
	bu := utf16.Encode([]rune(b))
	n := len(au)
	if len(bu) < n {
		n = len(bu)
	}
	for i := 0; i < n; i++ {
		if au[i] != bu[i] {
			return au[i] < bu[i]
		}
	}
	return len(au) < len(bu)
}

// looseHexDecode mirrors Buffer.from(text, 'hex'): it stops at the first
// non-hex nibble and ignores a trailing odd nibble.
func looseHexDecode(s string) []byte {
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		b, err := hex.DecodeString(s[i : i+2])
		if err != nil {
			break
		}
		out = append(out, b[0])
	}
	return out
}

// parseTree reads a sequence of "<mode> <name>\0<20 raw bytes>".
func parseTree(data []byte) []TreeEntry {
	entries := make([]TreeEntry, 0)
	offset := 0
	for offset < len(data) {
		relSpace := bytes.IndexByte(data[offset:], 0x20)
		if relSpace < 0 {
			break
		}
		space := offset + relSpace
		relNul := bytes.IndexByte(data[space+1:], 0x00)
		if relNul < 0 {
			break
		}
		nul := space + 1 + relNul
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, TreeEntry{
			Mode: string(data[offset:space]),
			Name: string(data[space+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		offset = nul + 21
	}
	return entries
}

func treeSortKey(entry TreeEntry) string {
	if entry.Mode == "40000" {
		return entry.Name + "/"
	}
	return entry.Name
}

// buildTree sorts entries like git (directories compare as name+'/') and
// serializes them. Sorting is stable, as Array.prototype.sort is in modern JS.
func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.SliceStable(sorted, func(i, j int) bool {
		return jsStringLess(treeSortKey(sorted[i]), treeSortKey(sorted[j]))
	})
	out := make([]byte, 0, len(sorted)*32)
	for _, entry := range sorted {
		out = append(out, []byte(entry.Mode+" "+entry.Name+"\x00")...)
		out = append(out, looseHexDecode(entry.OID)...)
	}
	return out
}

// parseCommit parses the header block and message of a commit object.
func parseCommit(data []byte) CommitView {
	text := string(data) // byte-preserving, equivalent to Buffer.toString('binary')
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split >= 0 {
		head = text[:split]
		message = string(data[split+2:])
	}
	headers := make([]string, 0)
	for _, line := range strings.Split(head, "\n") {
		if line != "" {
			headers = append(headers, line)
		}
	}
	tree := ""
	parents := make([]string, 0)
	author := ""
	committer := ""
	for _, line := range headers {
		switch {
		case strings.HasPrefix(line, "tree "):
			tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			parents = append(parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			author = line[7:]
		case strings.HasPrefix(line, "committer "):
			committer = line[10:]
		}
	}
	return CommitView{Tree: tree, Parents: parents, Author: author, Committer: committer, Message: message, Headers: headers}
}

// buildCommit emits a canonical commit object, always ending the message in '\n'.
func buildCommit(f CommitFields) []byte {
	lines := []string{"tree " + f.Tree}
	for _, parent := range f.Parents {
		lines = append(lines, "parent "+parent)
	}
	lines = append(lines, "author "+f.Author, "committer "+f.Committer)
	message := f.Message
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return []byte(strings.Join(lines, "\n") + "\n\n" + message)
}

// parseTag parses an annotated tag object.
func parseTag(data []byte) TagView {
	text := string(data)
	split := strings.Index(text, "\n\n")
	head := text
	message := ""
	if split >= 0 {
		head = text[:split]
		message = string(data[split+2:])
	}
	result := TagView{}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			result.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			result.Type = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "tag "):
			result.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			result.Tagger = line[7:]
		}
	}
	result.Message = message
	return result
}

// flattenTree expands a tree recursively into path -> FlatEntry, stopping at depth 40.
func flattenTree(objects map[string]GitObject, treeOID, prefix string, out map[string]FlatEntry, depth int) map[string]FlatEntry {
	if out == nil {
		out = make(map[string]FlatEntry)
	}
	if depth > 40 {
		return out
	}
	tree, ok := objects[treeOID]
	if !ok || tree.Type != "tree" {
		return out
	}
	for _, entry := range parseTree(tree.Data) {
		path := entry.Name
		if prefix != "" {
			path = prefix + "/" + entry.Name
		}
		if entry.Mode == "40000" {
			flattenTree(objects, entry.OID, path, out, depth+1)
		} else {
			out[path] = FlatEntry{Mode: entry.Mode, OID: entry.OID}
		}
	}
	return out
}

type flatNode struct {
	files map[string]FlatEntry
	dirs  map[string]*flatNode
}

func newFlatNode() *flatNode {
	return &flatNode{files: make(map[string]FlatEntry), dirs: make(map[string]*flatNode)}
}

// buildTreeFromFlat builds nested trees for a flat path map and returns the root tree oid.
// Newly created tree objects are inserted into objects (only when absent).
func buildTreeFromFlat(objects map[string]GitObject, files map[string]FlatEntry) string {
	if objects == nil {
		objects = make(map[string]GitObject)
	}
	root := newFlatNode()
	for path, entry := range files {
		segments := make([]string, 0)
		for _, part := range strings.Split(path, "/") {
			if part != "" {
				segments = append(segments, part)
			}
		}
		if len(segments) == 0 {
			continue
		}
		node := root
		for i := 0; i < len(segments)-1; i++ {
			name := segments[i]
			child := node.dirs[name]
			if child == nil {
				child = newFlatNode()
				node.dirs[name] = child
			}
			node = child
		}
		node.files[segments[len(segments)-1]] = entry
	}
	put := func(typ string, data []byte) string {
		oid := hashObject(typ, data)
		if _, exists := objects[oid]; !exists {
			objects[oid] = GitObject{Type: typ, Data: data}
		}
		return oid
	}
	var build func(*flatNode) string
	build = func(node *flatNode) string {
		entries := make([]TreeEntry, 0, len(node.files)+len(node.dirs))
		for name, entry := range node.files {
			entries = append(entries, TreeEntry{Mode: entry.Mode, Name: name, OID: entry.OID})
		}
		for name, child := range node.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return put("tree", buildTree(entries))
	}
	return build(root)
}

// jsSliceBounds applies Buffer.subarray semantics (negative indices count from the end).
func jsSliceBounds(length, start, end int) (int, int) {
	if start < 0 {
		start += length
		if start < 0 {
			start = 0
		}
	}
	if start > length {
		start = length
	}
	if end < 0 {
		end += length
		if end < 0 {
			end = 0
		}
	}
	if end > length {
		end = length
	}
	if end < start {
		end = start
	}
	return start, end
}

// applyDelta applies a git delta: two size varints, then copy/insert opcodes.
func applyDelta(base, delta []byte) []byte {
	offset := 0
	readVarint := func() uint64 {
		value := uint64(0)
		shift := uint(0)
		for {
			b := byte(0)
			if offset < len(delta) {
				b = delta[offset]
			}
			offset++
			value += uint64(b&0x7f) << shift
			if b&0x80 == 0 {
				break
			}
			shift += 7
			if shift > 63 {
				break
			}
		}
		return value
	}
	_ = readVarint() // base size is not used
	targetSize := int(readVarint())
	if targetSize < 0 {
		targetSize = 0
	}
	out := make([]byte, targetSize)
	written := 0
	for offset < len(delta) {
		opcode := delta[offset]
		offset++
		if opcode&0x80 != 0 {
			copyOffset := int32(0)
			copySize := int32(0)
			if opcode&0x01 != 0 {
				if offset < len(delta) {
					copyOffset |= int32(delta[offset])
				}
				offset++
			}
			if opcode&0x02 != 0 {
				if offset < len(delta) {
					copyOffset |= int32(delta[offset]) << 8
				}
				offset++
			}
			if opcode&0x04 != 0 {
				if offset < len(delta) {
					copyOffset |= int32(delta[offset]) << 16
				}
				offset++
			}
			if opcode&0x08 != 0 {
				if offset < len(delta) {
					copyOffset |= int32(delta[offset]) << 24
				}
				offset++
			}
			if opcode&0x10 != 0 {
				if offset < len(delta) {
					copySize |= int32(delta[offset])
				}
				offset++
			}
			if opcode&0x20 != 0 {
				if offset < len(delta) {
					copySize |= int32(delta[offset]) << 8
				}
				offset++
			}
			if opcode&0x40 != 0 {
				if offset < len(delta) {
					copySize |= int32(delta[offset]) << 16
				}
				offset++
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			start, end := jsSliceBounds(len(base), int(copyOffset), int(copyOffset)+int(copySize))
			chunk := base[start:end]
			if written < len(out) {
				written += copy(out[written:], chunk)
			} else {
				written += len(chunk)
			}
		} else if opcode != 0 {
			count := int(opcode)
			if offset+count > len(delta) {
				count = len(delta) - offset
			}
			if count < 0 {
				count = 0
			}
			if written < len(out) {
				written += copy(out[written:], delta[offset:offset+count])
			} else {
				written += count
			}
			offset += count
		} else {
			break
		}
	}
	if written >= len(out) {
		return out
	}
	return out[:written]
}

// packCountingReader tracks exactly how many input bytes zlib consumed; it
// implements io.ByteReader so flate does not read ahead past the stream.
type packCountingReader struct {
	r *bytes.Reader
	n int
}

func (p *packCountingReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += n
	return n, err
}

func (p *packCountingReader) ReadByte() (byte, error) {
	b, err := p.r.ReadByte()
	if err == nil {
		p.n++
	}
	return b, err
}

func packResult(entries []PackEntry, declared int, errMsg string) PackReadResult {
	ok := len(entries) == declared
	if errMsg == "" && !ok {
		errMsg = "truncated pack"
	}
	return PackReadResult{Objects: entries, OK: ok, Error: errMsg}
}

// readPack reads a version-2 pack, resolving OFS_DELTA and REF_DELTA.
// `external` supplies REF_DELTA bases that are not in the pack.
//
// Signature note: the shared PackReadResult/PackEntry structs (types.go) cannot
// represent TS's version, declaredCount, deltaCount, PackEntry.offset or
// PackEntry.delta. OK mirrors git.ts's `read.entries.length === read.declaredCount`
// (len(Objects) == declaredCount); Error is an extra diagnostic (TS has none).
func readPack(pack []byte, external func(string) (GitObject, bool)) PackReadResult {
	// TS returns version=0/declaredCount=0/entries=[], so the mirrored equality is true.
	empty := PackReadResult{Objects: []PackEntry{}, OK: true}
	if len(pack) < 32 || string(pack[0:4]) != "PACK" {
		return empty
	}
	declaredCount := int(binary.BigEndian.Uint32(pack[8:12]))
	body := pack[:len(pack)-20] // drop the trailing SHA-1 checksum
	entries := make([]PackEntry, 0)
	objects := make(map[string]GitObject)
	byOffset := make(map[int]GitObject)
	offset := 12
	for index := 0; index < declaredCount && offset < len(body); index++ {
		start := offset
		b := body[offset]
		offset++
		code := (b >> 4) & 0x07
		shift := uint(4)
		for b&0x80 != 0 {
			if offset >= len(body) {
				return packResult(entries, declaredCount, "")
			}
			b = body[offset]
			offset++
			shift += 7
			if shift > 63 {
				shift = 63
			}
		}
		var baseObject GitObject
		haveBase := false
		if code == 6 {
			// OFS_DELTA: negative offset varint pointing at an earlier object.
			if offset >= len(body) {
				return packResult(entries, declaredCount, "")
			}
			b2 := body[offset]
			offset++
			value := uint64(b2 & 0x7f)
			for b2&0x80 != 0 {
				if offset >= len(body) {
					return packResult(entries, declaredCount, "")
				}
				b2 = body[offset]
				offset++
				value = (value+1)*128 + uint64(b2&0x7f)
			}
			baseObject, haveBase = byOffset[start-int(value)]
		} else if code == 7 {
			// REF_DELTA: full 20-byte oid of the base object.
			if offset+20 > len(body) {
				return packResult(entries, declaredCount, "")
			}
			baseOID := hex.EncodeToString(body[offset : offset+20])
			offset += 20
			if obj, ok := objects[baseOID]; ok {
				baseObject, haveBase = obj, true
			} else if external != nil {
				baseObject, haveBase = external(baseOID)
			}
		}
		if offset > len(body) {
			return packResult(entries, declaredCount, "")
		}
		reader := &packCountingReader{r: bytes.NewReader(body[offset:])}
		zr, err := zlib.NewReader(reader)
		if err != nil {
			break
		}
		inflated, err := io.ReadAll(zr)
		closeErr := zr.Close()
		if err != nil || closeErr != nil {
			break
		}
		offset += reader.n

		var typ string
		var data []byte
		if code == 6 || code == 7 {
			if !haveBase {
				break
			}
			typ = baseObject.Type
			data = applyDelta(baseObject.Data, inflated)
		} else {
			typ = codeType[int(code)]
			if typ == "" {
				typ = "blob"
			}
			data = inflated
		}
		oid := hashObject(typ, data)
		obj := GitObject{Type: typ, Data: data}
		objects[oid] = obj
		byOffset[start] = obj
		entries = append(entries, PackEntry{OID: oid, Type: typ, Data: data})
	}
	return packResult(entries, declaredCount, "")
}

// writeObjectHeader emits the variable-length type/size header of a packed object.
func writeObjectHeader(typ string, size int) []byte {
	out := make([]byte, 0, 8)
	remaining := size
	first := (typeCode[typ] << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	out = append(out, byte(first))
	return out
}

// writePack builds a version-2 packfile of plain objects (no deltas) with a SHA-1 trailer.
// Object IDs and the pack header/count are identical to TS; the compressed bytes (and
// therefore the pack trailer) can differ from Node's native zlib because Go's
// compress/zlib is a different deflate implementation.
func writePack(objects []GitObject) []byte {
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[0:4], 2)
	binary.BigEndian.PutUint32(header[4:8], uint32(len(objects)))
	body := make([]byte, 0, 64+len(objects)*64)
	body = append(body, []byte("PACK")...)
	body = append(body, header...)
	for _, object := range objects {
		body = append(body, writeObjectHeader(object.Type, len(object.Data))...)
		var buf bytes.Buffer
		zw := zlib.NewWriter(&buf)
		_, _ = zw.Write(object.Data)
		_ = zw.Close()
		body = append(body, buf.Bytes()...)
	}
	return append(body, looseHexDecode(sha1Hex(body))...)
}

// verifyPackChecksum recomputes a pack's trailer; returns ok, expected, actual.
func verifyPackChecksum(pack []byte) (bool, string, string) {
	if len(pack) < 20 {
		return false, "", ""
	}
	expected := sha1Hex(pack[:len(pack)-20])
	actual := hex.EncodeToString(pack[len(pack)-20:])
	return expected == actual, expected, actual
}
