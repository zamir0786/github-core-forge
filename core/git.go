package main

// Git smart HTTP: protocol v2 upload-pack (ls-refs and fetch), the v0 receive-pack push path,
// and the info/refs advertisements. Ported byte-for-byte from bot/git.ts.
//
// Only the standard library is used. Object parsing, pack read/write and object insertion are
// implemented locally over the Repo maps from types.go so this file does not depend on the exact
// signatures of the concurrently written store.go / objects.go helpers.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	gitUploadType    = "application/x-git-upload-pack-advertisement"
	gitUploadResult  = "application/x-git-upload-pack-result"
	gitReceiveType   = "application/x-git-receive-pack-advertisement"
	gitReceiveResult = "application/x-git-receive-pack-result"
	gitAgent         = "makefun-forge/1.0"
)

// ---------------------------------------------------------------- deterministic response cache

// gitRendered is a copy-on-write snapshot published atomically per repository.
// All cached byte slices are immutable once published, so readers never take a
// lock and never rebuild a response after warmup. A repo revision bump retires
// the whole snapshot without touching the old bytes in flight.
type gitRendered struct {
	rev   uint64
	cache sync.Map // request key -> *gitInflight
}

type gitCachedBody struct {
	body     []byte
	key      string
	prebuilt []byte
}

// gitInflight gives every cache key single-flight semantics. The first clone
// renders a pack; the other clones wait on the same sync.Once instead of
// duplicating expensive planning and pack hashing work.
type gitInflight struct {
	once   sync.Once
	result *gitCachedBody
}

func (g *gitInflight) get() *gitCachedBody {
	g.once.Do(func() {})
	return g.result
}

type gitNamedOID struct {
	name string
	oid  string
}

func gitSortedRefs(repo *Repo) []gitNamedOID {
	refs := make([]gitNamedOID, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, gitNamedOID{name: name, oid: oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	return refs
}

func gitRefSig(repo *Repo) string {
	var b strings.Builder
	b.WriteString(repo.DefaultBranch)
	for _, ref := range gitSortedRefs(repo) {
		b.WriteByte(';')
		b.WriteString(ref.name)
		b.WriteByte('=')
		b.WriteString(ref.oid)
	}
	return b.String()
}

/** Memoize one deterministic response until the repo's revision changes. */
func gitCached(repo *Repo, key, contentType string, headers map[string]string, makeBody func() []byte) *gitCachedBody {
	rev := repo.Rev.Load()
	snapshot := repo.Rendered.Load()
	if snapshot == nil || snapshot.rev != rev {
		next := &gitRendered{rev: rev}
		for {
			current := repo.Rendered.Load()
			if current != nil && current.rev == rev {
				snapshot = current
				break
			}
			if repo.Rendered.CompareAndSwap(current, next) {
				snapshot = next
				break
			}
		}
	}
	fullKey := key
	if hit, ok := snapshot.cache.Load(fullKey); ok {
		return hit.(*gitInflight).get()
	}
	flight := &gitInflight{}
	actual, loaded := snapshot.cache.LoadOrStore(fullKey, flight)
	if !loaded {
		flight.once.Do(func() {
			body := makeBody()
			flight.result = &gitCachedBody{
				body: body,
				key:  "git:" + sha1Hex(body),
				prebuilt: buildResponse(nil, HTTPResult{
					Status:      200,
					ContentType: contentType,
					Body:        body,
					Headers:     headers,
				}, false),
			}
		})
	}
	return actual.(*gitInflight).get()
}

// ---------------------------------------------------------------- small wire helpers

type gitPktBuilder struct {
	out []byte
}

func (b *gitPktBuilder) line(text string) { b.out = append(b.out, pktString(text)...) }
func (b *gitPktBuilder) flush()           { b.out = append(b.out, flushBytes...) }
func (b *gitPktBuilder) delim()           { b.out = append(b.out, delimBytes...) }
func (b *gitPktBuilder) raw(data []byte)  { b.out = append(b.out, data...) }
func (b *gitPktBuilder) bytes() []byte    { return b.out }

func gitSideband(data []byte) []byte {
	const chunkSize = 65000
	var b gitPktBuilder
	for off := 0; off < len(data); off += chunkSize {
		end := off + chunkSize
		if end > len(data) {
			end = len(data)
		}
		part := make([]byte, 0, 1+end-off)
		part = append(part, 1) // band 1
		part = append(part, data[off:end]...)
		b.raw(pkt(part))
	}
	return b.bytes()
}

var gitV2UploadAd = func() []byte {
	var out gitPktBuilder
	out.line("# service=git-upload-pack\n")
	out.flush()
	out.line("version 2\n")
	out.line("agent=" + gitAgent + "\n")
	out.line("ls-refs=unborn\n")
	out.line("fetch=shallow\n")
	out.line("object-format=sha1\n")
	out.flush()
	return out.bytes()
}()

func gitTextResult(status int, text string) HTTPResult {
	return HTTPResult{Status: status, ContentType: "text/plain; charset=utf-8", Body: []byte(text)}
}

// ---------------------------------------------------------------- local git object helpers

func gitHeadOID(repo *Repo) (string, bool) {
	oid, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
	return oid, ok
}

type gitTreeEntry struct {
	mode string
	name string
	oid  string
}

func gitParseTree(data []byte) []gitTreeEntry {
	entries := make([]gitTreeEntry, 0)
	for off := 0; off < len(data); {
		space := bytes.IndexByte(data[off:], 0x20)
		if space < 0 {
			break
		}
		space += off
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul < 0 {
			break
		}
		nul += space + 1
		if nul+21 > len(data) {
			break
		}
		entries = append(entries, gitTreeEntry{
			mode: string(data[off:space]),
			name: string(data[space+1 : nul]),
			oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries
}

type gitCommitInfo struct {
	tree    string
	parents []string
}

func gitParseCommit(data []byte) gitCommitInfo {
	text := string(data)
	head := text
	if split := strings.Index(head, "\n\n"); split >= 0 {
		head = head[:split]
	}
	info := gitCommitInfo{parents: make([]string, 0)}
	for _, line := range strings.Split(head, "\n") {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "tree ") {
			info.tree = strings.TrimSpace(line[5:])
		} else if strings.HasPrefix(line, "parent ") {
			info.parents = append(info.parents, strings.TrimSpace(line[7:]))
		}
	}
	return info
}

func gitTagTarget(data []byte) string {
	text := string(data)
	head := text
	if split := strings.Index(head, "\n\n"); split >= 0 {
		head = head[:split]
	}
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, "object ") {
			return strings.TrimSpace(line[7:])
		}
	}
	return ""
}

/** Peel a ref to its commit, one tag hop at a time (mirrors store.ts commitOf). */
func gitCommitOf(repo *Repo, oid string) string {
	current := oid
	for hop := 0; hop < 8; hop++ {
		object, ok := repo.Objects[current]
		if !ok {
			return ""
		}
		if object.Type == "commit" {
			return current
		}
		if object.Type != "tag" {
			return ""
		}
		current = gitTagTarget(object.Data)
	}
	return ""
}

func gitPeelOID(repo *Repo, oid string) string {
	object, ok := repo.Objects[oid]
	if !ok || object.Type != "tag" {
		return ""
	}
	return gitCommitOf(repo, oid)
}

func gitIsAncestor(repo *Repo, ancestor string, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := repo.Objects[oid]
		if !ok || object.Type != "commit" {
			continue
		}
		for _, parent := range gitParseCommit(object.Data).parents {
			if parent == ancestor {
				return true
			}
			if !seen[parent] {
				stack = append(stack, parent)
			}
		}
	}
	return false
}

func gitHashObject(typ string, data []byte) string {
	var header [32]byte
	n := copy(header[:], typ)
	header[n] = ' '
	n++
	prefix := strconv.AppendInt(header[:n], int64(len(data)), 10)
	n = len(prefix)
	header[n] = 0
	n++

	// Push and clone objects are small; hashing them through a stack buffer
	// avoids the temporary "header + object" heap allocation on every object.
	if n+len(data) <= 4096 {
		var full [4096]byte
		copy(full[:], header[:n])
		copy(full[n:], data)
		sum := sha1.Sum(full[:n+len(data)])
		var encoded [40]byte
		hex.Encode(encoded[:], sum[:])
		return string(encoded[:])
	}

	full := make([]byte, n+len(data))
	copy(full, header[:n])
	copy(full[n:], data)
	sum := sha1.Sum(full)
	return hex.EncodeToString(sum[:])
}

func gitPutObject(repo *Repo, typ string, data []byte) string {
	if repo.Objects == nil {
		repo.Objects = make(map[string]GitObject)
	}
	oid := gitHashObject(typ, data)
	gitPutObjectOID(repo, oid, typ, data)
	return oid
}

// gitPutObjectOID avoids recomputing the OID that gitReadPack already derived.
func gitPutObjectOID(repo *Repo, oid, typ string, data []byte) {
	if repo.Objects == nil {
		repo.Objects = make(map[string]GitObject)
	}
	if _, exists := repo.Objects[oid]; !exists {
		repo.Objects[oid] = GitObject{Type: typ, Data: data}
		repo.Rev.Add(1)
	}
}

// ---------------------------------------------------------------- packfile writing / reading

func gitPackObjectHeader(typ string, size int) []byte {
	code := 3 // blob
	switch typ {
	case "commit":
		code = 1
	case "tree":
		code = 2
	case "tag":
		code = 4
	}
	bytesOut := make([]byte, 0, 8)
	remaining := size
	first := (code << 4) | (remaining & 0x0f)
	remaining /= 16
	for remaining > 0 {
		bytesOut = append(bytesOut, byte(first|0x80))
		first = remaining & 0x7f
		remaining /= 128
	}
	bytesOut = append(bytesOut, byte(first))
	return bytesOut
}

type gitPackChunk struct {
	header     []byte
	compressed []byte
}

// Content-addressed compressed chunks: objects are immutable once inserted, so
// a chunk rendered on the first fetch can be reused by every later pack with
// no zlib work. This is the zero-zlib path for warm clones.
var gitPackChunks sync.Map // oid -> gitPackChunk

func gitPackObjectChunk(oid string, object GitObject) gitPackChunk {
	if oid != "" {
		if hit, ok := gitPackChunks.Load(oid); ok {
			return hit.(gitPackChunk)
		}
	} else {
		oid = gitHashObject(object.Type, object.Data)
		if hit, ok := gitPackChunks.Load(oid); ok {
			return hit.(gitPackChunk)
		}
	}
	chunk := gitPackChunk{
		header:     gitPackObjectHeader(object.Type, len(object.Data)),
		compressed: nil,
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(object.Data)
	_ = zw.Close()
	chunk.compressed = append([]byte(nil), compressed.Bytes()...)
	actual, _ := gitPackChunks.LoadOrStore(oid, chunk)
	return actual.(gitPackChunk)
}

/** Version-2 packfile of plain objects with a SHA-1 trailer, as objects.ts writePack does. */
func gitWritePack(objects []GitObject, oids []string) []byte {
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(objects)))
	body := make([]byte, 0, 256)
	body = append(body, header...)
	for i, object := range objects {
		oid := ""
		if i < len(oids) {
			oid = oids[i]
		}
		chunk := gitPackObjectChunk(oid, object)
		body = append(body, chunk.header...)
		body = append(body, chunk.compressed...)
	}
	sum := sha1.Sum(body)
	out := make([]byte, 0, len(body)+20)
	out = append(out, body...)
	return append(out, sum[:]...)
}

// gitCountingReader tracks exactly how many bytes zlib consumed. Implementing ReadByte keeps
// compress/flate from wrapping it in a bufio.Reader that would read ahead into the next object;
// capping Read to one byte keeps the count exact as well.
type gitCountingReader struct {
	r *bytes.Reader
	n int
}

func (c *gitCountingReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (c *gitCountingReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func gitApplyDelta(base []byte, delta []byte) []byte {
	off := 0
	readVarint := func() (int, bool) {
		value, shift := 0, uint(0)
		for {
			if off >= len(delta) {
				return 0, false
			}
			b := delta[off]
			off++
			value |= int(b&0x7f) << shift
			if b&0x80 == 0 {
				return value, true
			}
			shift += 7
		}
	}
	if _, ok := readVarint(); !ok {
		return nil
	}
	targetSize, ok := readVarint()
	if !ok || targetSize < 0 {
		return nil
	}
	out := make([]byte, targetSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOffset, copySize := 0, 0
			if op&0x01 != 0 {
				if off >= len(delta) {
					return out[:written]
				}
				copyOffset |= int(delta[off])
				off++
			}
			if op&0x02 != 0 {
				if off >= len(delta) {
					return out[:written]
				}
				copyOffset |= int(delta[off]) << 8
				off++
			}
			if op&0x04 != 0 {
				if off >= len(delta) {
					return out[:written]
				}
				copyOffset |= int(delta[off]) << 16
				off++
			}
			if op&0x08 != 0 {
				if off >= len(delta) {
					return out[:written]
				}
				copyOffset |= int(delta[off]) << 24
				off++
			}
			if op&0x10 != 0 {
				if off >= len(delta) {
					return out[:written]
				}
				copySize |= int(delta[off])
				off++
			}
			if op&0x20 != 0 {
				if off >= len(delta) {
					return out[:written]
				}
				copySize |= int(delta[off]) << 8
				off++
			}
			if op&0x40 != 0 {
				if off >= len(delta) {
					return out[:written]
				}
				copySize |= int(delta[off]) << 16
				off++
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOffset < 0 || copyOffset >= len(base) || copySize <= 0 {
				break
			}
			end := copyOffset + copySize
			if end > len(base) {
				end = len(base)
			}
			available := len(out) - written
			if available <= 0 {
				break
			}
			n := copy(out[written:], base[copyOffset:end])
			if n > available {
				n = available
			}
			written += n
		} else if op != 0 {
			count := int(op)
			if count > len(delta)-off || count > len(out)-written {
				break
			}
			copy(out[written:], delta[off:off+count])
			off += count
			written += count
		} else {
			break
		}
	}
	return out[:written]
}

/** Read the objects in a version-2 packfile. Returns objects and their OIDs. */
func gitReadPack(pack []byte) ([]GitObject, []string, int) {
	objects := make([]GitObject, 0)
	oids := make([]string, 0)
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return objects, oids, 0
	}
	declaredCount := int(binary.BigEndian.Uint32(pack[8:12]))
	body := pack[:len(pack)-20]
	byOffset := make(map[int]GitObject)
	byOID := make(map[string]GitObject)
	count := 0
	for off := 12; off < len(body) && count < declaredCount; {
		start := off
		b := body[off]
		off++
		code := (b >> 4) & 0x07
		for b&0x80 != 0 {
			if off >= len(body) {
				return objects, oids, declaredCount
			}
			b = body[off]
			off++
		}
		var base GitObject
		if code == 6 {
			if off >= len(body) {
				return objects, oids, declaredCount
			}
			b2 := body[off]
			off++
			value := int(b2 & 0x7f)
			for b2&0x80 != 0 {
				if off >= len(body) {
					return objects, oids, declaredCount
				}
				b2 = body[off]
				off++
				value = (value+1)*128 + int(b2&0x7f)
			}
			base = byOffset[start-value]
		} else if code == 7 {
			if off+20 > len(body) {
				return objects, oids, declaredCount
			}
			baseOID := hex.EncodeToString(body[off : off+20])
			off += 20
			base = byOID[baseOID]
		}
		// bytes.Reader.Len gives the exact compressed byte count without
		// forcing flate through a one-byte-at-a-time counting reader.
		chunkLen := len(body) - off
		source := bytes.NewReader(body[off:])
		zr, err := zlib.NewReader(source)
		if err != nil {
			break
		}
		inflated, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			break
		}
		off += chunkLen - source.Len()
		var typ string
		var data []byte
		if code == 6 || code == 7 {
			if base.Type == "" {
				break
			}
			typ = base.Type
			data = gitApplyDelta(base.Data, inflated)
		} else {
			switch code {
			case 1:
				typ = "commit"
			case 2:
				typ = "tree"
			case 4:
				typ = "tag"
			default:
				typ = "blob"
			}
			data = inflated
		}
		oid := gitHashObject(typ, data)
		object := GitObject{Type: typ, Data: data}
		byOID[oid] = object
		byOffset[start] = object
		objects = append(objects, object)
		oids = append(oids, oid)
		count++
	}
	return objects, oids, declaredCount
}

// ---------------------------------------------------------------- info/refs advertisements

var gitNoCacheHeaders = map[string]string{"cache-control": "no-cache"}

/** `GET /<owner>/<repo>.git/info/refs?service=...`. */
func handleInfoRefs(repo *Repo, service string, gitProtocol string) HTTPResult {
	v2 := service == "git-upload-pack" && strings.Contains(gitProtocol, "version=2")
	isUpload := service == "git-upload-pack"
	contentType := gitReceiveType
	if isUpload {
		contentType = gitUploadType
	}
	if isUpload && v2 {
		return HTTPResult{
			Status: 200, ContentType: contentType, Body: gitV2UploadAd,
			Headers: gitNoCacheHeaders, CacheKey: "git:v2-upload-ad",
		}
	}
	cacheKey := "ad:upload:v0"
	if !isUpload {
		cacheKey = "ad:receive:v0"
	}
	cached := gitCached(repo, cacheKey, contentType, gitNoCacheHeaders, func() []byte {
		var out gitPktBuilder
		tip, hasTip := gitHeadOID(repo)
		if isUpload {
			caps := "multi_ack_detailed side-band-64k thin-pack ofs-delta shallow symref=HEAD:refs/heads/" +
				repo.DefaultBranch + " agent=" + gitAgent + " object-format=sha1"
			out.line("# service=git-upload-pack\n")
			out.flush()
			if !hasTip {
				out.line(ZeroOID + " capabilities^{}\x00" + caps + "\n")
			} else {
				out.line(tip + " HEAD\x00" + caps + "\n")
				for _, ref := range gitSortedRefs(repo) {
					out.line(ref.oid + " " + ref.name + "\n")
					if peeled := gitPeelOID(repo, ref.oid); peeled != "" {
						out.line(peeled + " " + ref.name + "^{}\n")
					}
				}
			}
		} else {
			// receive-pack is always protocol v0.
			caps := "report-status side-band-64k delete-refs ofs-delta agent=" + gitAgent +
				" symref=HEAD:refs/heads/" + repo.DefaultBranch
			out.line("# service=git-receive-pack\n")
			out.flush()
			if !hasTip {
				out.line(ZeroOID + " capabilities^{}\x00" + caps + "\n")
			} else {
				out.line(tip + " HEAD\x00" + caps + "\n")
				for _, ref := range gitSortedRefs(repo) {
					out.line(ref.oid + " " + ref.name + "\n")
				}
			}
		}
		out.flush()
		return out.bytes()
	})
	return HTTPResult{
		Status:      200,
		ContentType: contentType,
		Body:        cached.body,
		Headers:     gitNoCacheHeaders,
		CacheKey:    cached.key,
		Prebuilt:    cached.prebuilt,
	}
}

// ---------------------------------------------------------------- upload-pack

func gitPacketLines(body []byte) []string {
	lines := make([]string, 0)
	for off := 0; off < len(body); {
		p := readPacket(body, off)
		off = p.Next
		switch p.Kind {
		case "data":
			lines = append(lines, string(p.Payload))
		case "bad":
			return lines
		}
	}
	return lines
}

/** `POST /<owner>/<repo>.git/git-upload-pack`. */
func handleUploadPack(repo *Repo, body []byte) HTTPResult {
	lines := gitPacketLines(body)
	first := ""
	if len(lines) > 0 {
		first = lines[0]
	}
	if strings.HasPrefix(first, "command=ls-refs") {
		return gitHandleLSRefs(repo, lines)
	}
	if strings.HasPrefix(first, "command=fetch") {
		return gitHandleFetch(repo, lines)
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "want ") {
			return gitHandleFetch(repo, lines)
		}
	}
	return gitTextResult(400, "bad request\n")
}

func gitHandleLSRefs(repo *Repo, lines []string) HTTPResult {
	prefixes := make([]string, 0)
	peel, symrefs, unborn := false, false, false
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	cached := gitCached(repo, "ls:"+strings.Join(lines, "\x00"), gitUploadResult, nil, func() []byte {
		var parts gitPktBuilder
		tip, hasTip := gitHeadOID(repo)
		if keep("HEAD") {
			if !hasTip {
				if unborn {
					line := "unborn HEAD"
					if symrefs {
						line += " symref-target:refs/heads/" + repo.DefaultBranch
					}
					parts.line(line + "\n")
				}
			} else {
				line := tip + " HEAD"
				if symrefs {
					line += " symref-target:refs/heads/" + repo.DefaultBranch
				}
				parts.line(line + "\n")
			}
		}
		for _, ref := range gitSortedRefs(repo) {
			if !keep(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if peel {
				if peeled := gitPeelOID(repo, ref.oid); peeled != "" {
					line += " peeled:" + peeled
				}
			}
			parts.line(line + "\n")
		}
		parts.flush()
		return parts.bytes()
	})
	return HTTPResult{Status: 200, ContentType: gitUploadResult, Body: cached.body, CacheKey: cached.key, Prebuilt: cached.prebuilt}
}

type gitFetchPlan struct {
	objects   map[string]GitObject
	order     []string
	shallow   []string
	unshallow []string
}

/** The whole object closure of one oid, optionally stopping commit ancestry at boundaries. */
func gitClosure(repo *Repo, roots []string, stopAt map[string]bool, put func(string, GitObject)) {
	stack := append([]string{}, roots...)
	seen := make(map[string]bool)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object, ok := repo.Objects[oid]
		if !ok {
			continue
		}
		put(oid, object)
		switch object.Type {
		case "tree":
			for _, entry := range gitParseTree(object.Data) {
				stack = append(stack, entry.oid)
			}
		case "commit":
			view := gitParseCommit(object.Data)
			stack = append(stack, view.tree)
			if stopAt != nil && stopAt[oid] {
				continue
			}
			for _, parent := range view.parents {
				stack = append(stack, parent)
			}
		case "tag":
			stack = append(stack, gitTagTarget(object.Data))
		}
	}
}

func gitPlanFetch(repo *Repo, wants []string, haves []string, depth *int, clientShallow []string) gitFetchPlan {
	plan := gitFetchPlan{
		objects:   make(map[string]GitObject),
		order:     make([]string, 0),
		shallow:   make([]string, 0),
		unshallow: make([]string, 0),
	}
	put := func(oid string, object GitObject) {
		if _, exists := plan.objects[oid]; !exists {
			plan.order = append(plan.order, oid)
		}
		plan.objects[oid] = object
	}
	if len(wants) == 0 {
		return plan
	}
	if depth != nil && *depth > 0 {
		type queueItem struct {
			oid string
			gen int
		}
		queue := make([]queueItem, 0)
		commitGen := make(map[string]int)
		extras := make([]string, 0)
		for _, want := range wants {
			object, ok := repo.Objects[want]
			if !ok {
				continue
			}
			if object.Type == "tag" {
				put(want, object)
				if peeled := gitCommitOf(repo, want); peeled != "" {
					queue = append(queue, queueItem{oid: peeled, gen: 1})
				}
			} else if object.Type == "commit" {
				queue = append(queue, queueItem{oid: want, gen: 1})
			} else {
				extras = append(extras, want)
			}
		}
		for len(queue) > 0 {
			item := queue[0]
			queue = queue[1:]
			if gen, seen := commitGen[item.oid]; seen && gen <= item.gen {
				continue
			}
			commitGen[item.oid] = item.gen
			object, ok := repo.Objects[item.oid]
			if !ok || object.Type != "commit" {
				continue
			}
			if item.gen >= *depth {
				plan.shallow = append(plan.shallow, item.oid)
				continue
			}
			for _, parent := range gitParseCommit(object.Data).parents {
				queue = append(queue, queueItem{oid: parent, gen: item.gen + 1})
			}
		}
		// Every included commit, plus its tree closure (never its parents).
		for oid, gen := range commitGen {
			_ = gen
			object, ok := repo.Objects[oid]
			if !ok {
				continue
			}
			put(oid, object)
			tree := gitParseCommit(object.Data).tree
			gitClosure(repo, []string{tree}, nil, put)
		}
		gitClosure(repo, extras, nil, put)
	} else {
		gitClosure(repo, wants, nil, put)
	}
	if len(haves) > 0 {
		haveSet := make(map[string]bool)
		for _, oid := range clientShallow {
			haveSet[oid] = true
		}
		haveObjects := make(map[string]GitObject)
		gitClosure(repo, haves, haveSet, func(oid string, object GitObject) {
			haveObjects[oid] = object
		})
		for oid := range haveObjects {
			delete(plan.objects, oid)
		}
		kept := make([]string, 0, len(plan.order))
		for _, oid := range plan.order {
			if _, exists := plan.objects[oid]; exists {
				kept = append(kept, oid)
			}
		}
		plan.order = kept
	}
	if depth != nil {
		boundary := make(map[string]bool)
		for _, oid := range plan.shallow {
			boundary[oid] = true
		}
		for _, oid := range clientShallow {
			if !boundary[oid] {
				plan.unshallow = append(plan.unshallow, oid)
			}
		}
	}
	return plan
}

func gitHandleFetch(repo *Repo, lines []string) HTTPResult {
	wants := make([]string, 0)
	haves := make([]string, 0)
	clientShallow := make([]string, 0)
	var depth *int
	for _, raw := range lines {
		line := strings.TrimSuffix(raw, "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimSpace(strings.TrimPrefix(line, "want ")))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimSpace(strings.TrimPrefix(line, "have ")))
		case strings.HasPrefix(line, "shallow "):
			clientShallow = append(clientShallow, strings.TrimSpace(strings.TrimPrefix(line, "shallow ")))
		case strings.HasPrefix(line, "deepen "):
			value, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "deepen ")))
			if err == nil && value > 0 {
				v := value
				depth = &v
			}
		}
	}
	cached := gitCached(repo, "fetch:"+strings.Join(lines, "\x00"), gitUploadResult, nil, func() []byte {
		plan := gitPlanFetch(repo, wants, haves, depth, clientShallow)
		objects := make([]GitObject, 0, len(plan.order))
		oids := make([]string, 0, len(plan.order))
		for _, oid := range plan.order {
			objects = append(objects, plan.objects[oid])
			oids = append(oids, oid)
		}
		pack := gitWritePack(objects, oids)
		var chunks gitPktBuilder
		if len(wants) > 0 {
			chunks.line("acknowledgments\n")
			chunks.line("NAK\n")
			chunks.flush()
			if depth != nil && (len(plan.shallow) > 0 || len(plan.unshallow) > 0) {
				var info gitPktBuilder
				info.line("shallow-info\n")
				for _, oid := range plan.shallow {
					info.line("shallow " + oid + "\n")
				}
				for _, oid := range plan.unshallow {
					info.line("unshallow " + oid + "\n")
				}
				info.flush()
				chunks.raw(info.bytes())
			}
			chunks.line("packfile\n")
			chunks.raw(gitSideband(pack))
			chunks.flush()
		} else {
			chunks.flush()
		}
		return chunks.bytes()
	})
	return HTTPResult{Status: 200, ContentType: gitUploadResult, Body: cached.body, CacheKey: cached.key, Prebuilt: cached.prebuilt}
}

// ---------------------------------------------------------------- receive-pack

type gitPushCommand struct {
	oldOID string
	newOID string
	ref    string
}

type gitPushReport struct {
	ref    string
	ok     bool
	reason string
}

/** `POST /<owner>/<repo>.git/git-receive-pack`: unpack, update refs and report per ref. */
func handleReceivePack(repo *Repo, body []byte) HTTPResult {
	commands := make([]gitPushCommand, 0)
	caps := make(map[string]bool)
	offset := 0
	malformed := false
	for offset < len(body) {
		packet := readPacket(body, offset)
		if packet.Kind == "bad" {
			malformed = true
			break
		}
		offset = packet.Next
		if packet.Kind == "flush" {
			break
		}
		if packet.Kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(packet.Payload), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			for _, token := range strings.Split(line[nul+1:], " ") {
				if token != "" {
					caps[strings.SplitN(token, "=", 2)[0]] = true
				}
			}
			line = line[:nul]
		}
		parts := strings.Split(line, " ")
		if len(parts) >= 3 {
			commands = append(commands, gitPushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		}
	}
	var pack []byte
	if offset < len(body) {
		pack = body[offset:]
	} else {
		pack = []byte{}
	}
	if malformed {
		return gitTextResult(400, "bad pkt-line\n")
	}
	if repo.Refs == nil {
		repo.Refs = make(map[string]string)
	}
	if repo.Objects == nil {
		repo.Objects = make(map[string]GitObject)
	}

	unpackOK := true
	if len(pack) > 0 {
		if len(pack) >= 12 && string(pack[:4]) == "PACK" {
			objects, oids, declared := gitReadPack(pack)
			if len(objects) != declared {
				unpackOK = false
			}
			for i, object := range objects {
				gitPutObjectOID(repo, oids[i], object.Type, object.Data)
			}
		} else {
			unpackOK = false
		}
	}

	reports := make([]gitPushReport, 0)
	for _, command := range commands {
		current, exists := repo.Refs[command.ref]
		if command.newOID == ZeroOID {
			if !exists {
				reports = append(reports, gitPushReport{ref: command.ref, ok: false, reason: "ref does not exist"})
			} else {
				delete(repo.Refs, command.ref)
				reports = append(reports, gitPushReport{ref: command.ref, ok: true})
			}
			continue
		}
		if _, ok := repo.Objects[command.newOID]; !ok {
			reports = append(reports, gitPushReport{ref: command.ref, ok: false, reason: "missing necessary objects"})
			continue
		}
		if !exists {
			repo.Refs[command.ref] = command.newOID
			reports = append(reports, gitPushReport{ref: command.ref, ok: true})
			continue
		}
		if current == command.newOID {
			reports = append(reports, gitPushReport{ref: command.ref, ok: true})
			continue
		}
		oldCommit := gitCommitOf(repo, current)
		newCommit := command.newOID
		if oldCommit != "" && gitIsAncestor(repo, oldCommit, newCommit) {
			repo.Refs[command.ref] = command.newOID
			reports = append(reports, gitPushReport{ref: command.ref, ok: true})
		} else {
			reports = append(reports, gitPushReport{ref: command.ref, ok: false, reason: "non-fast-forward"})
		}
	}
	repo.UpdatedAt = isoNow()
	repo.Rev.Add(1)

	var inner gitPktBuilder
	if unpackOK {
		inner.line("unpack ok\n")
	} else {
		inner.line("unpack error\n")
	}
	for _, report := range reports {
		if report.ok {
			inner.line("ok " + report.ref + "\n")
		} else {
			inner.line("ng " + report.ref + " " + report.reason + "\n")
		}
	}
	inner.flush()
	reportBytes := inner.bytes()
	var out []byte
	if caps["side-band-64k"] {
		var wrapped gitPktBuilder
		wrapped.raw(gitSideband(reportBytes))
		wrapped.flush()
		out = wrapped.bytes()
	} else {
		out = reportBytes
	}
	return HTTPResult{Status: 200, ContentType: gitReceiveResult, Body: out}
}
