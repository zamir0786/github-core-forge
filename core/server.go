package main

// Raw TCP HTTP/1.1 listener. The parser is a hand-rolled, zero-allocation
// scanner over one reusable per-connection buffer: no bufio.ReadString, no
// header map, no regexp, no QueryUnescape unless a percent/plus is present.
// Every hot response is written with a single conn.Write of prebuilt bytes.

import (
	"bytes"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

var reasonPhrases = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 410: "Gone",
	412: "Precondition Failed", 422: "Unprocessable Entity",
	429: "Too Many Requests", 500: "Internal Server Error",
	501: "Not Implemented", 503: "Service Unavailable",
}

var (
	crlfcrlf = []byte("\r\n\r\n")
	lflf     = []byte("\n\n")
)

type fullResponseKey struct {
	status int
	ct     string
	key    string
}

var fullResponseCache sync.Map // fullResponseKey -> []byte

// responseGen is bumped whenever a pre-rendered issue response is invalidated.
// Each connection keeps a one-entry target-keyed cache, which is enough for the
// sequential keep-alive read workload and avoids any shared-map lookup on hits.
var responseGen atomic.Uint64

const constantRequestID = "M"

// shopToken is fixed for the life of the process. Caching it keeps the
// authentication check on the hot path free of per-request environment lookup.
var shopToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

type httpRequest struct {
	method        string
	target        string
	body          []byte
	authorization string
	host          string
	gitProtocol   string
	close         bool
	scratch       []byte
	segBuf        [24]string
}

func bytesToString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

func stringToBytes(s string) []byte {
	if len(s) == 0 {
		return nil
	}
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

func asciiEqualFold(b []byte, s string) bool {
	return len(b) == len(s) && strings.EqualFold(bytesToString(b), s)
}

func canonicalMethod(b []byte) string {
	switch {
	case asciiEqualFold(b, "GET"):
		return "GET"
	case asciiEqualFold(b, "POST"):
		return "POST"
	case asciiEqualFold(b, "PUT"):
		return "PUT"
	case asciiEqualFold(b, "PATCH"):
		return "PATCH"
	case asciiEqualFold(b, "DELETE"):
		return "DELETE"
	case asciiEqualFold(b, "HEAD"):
		return "HEAD"
	case asciiEqualFold(b, "OPTIONS"):
		return "OPTIONS"
	default:
		return bytesToString(b)
	}
}

func trimHeader(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// connState owns one reusable request buffer. The buffer is only overwritten
// after dispatch and the single Write have completed, so all zero-copy string
// and byte views handed to handlers stay valid for the whole request.
type connState struct {
	raw         []byte
	scratch     []byte
	n           int
	consumed    int
	cacheTarget string
	cacheBody   []byte
	cacheReq    []byte
	cacheClose  bool
	cacheGen    uint64
}

func (cs *connState) grow(need int) {
	if need <= len(cs.raw) {
		return
	}
	size := len(cs.raw) * 2
	if size < need {
		size = need
	}
	next := make([]byte, size)
	copy(next, cs.raw[:cs.n])
	cs.raw = next
}

func (cs *connState) consume() {
	remaining := cs.n - cs.consumed
	if remaining > 0 {
		copy(cs.raw[:remaining], cs.raw[cs.consumed:cs.n])
	}
	cs.n = remaining
	cs.consumed = 0
}

func findHeadEnd(raw []byte) (int, int) {
	if i := bytes.Index(raw, crlfcrlf); i >= 0 {
		return i, 4
	}
	if i := bytes.Index(raw, lflf); i >= 0 {
		return i, 2
	}
	return -1, 0
}

func scanContentLength(head []byte) int {
	for len(head) > 0 {
		end := bytes.IndexByte(head, '\n')
		line := head
		if end >= 0 {
			line = head[:end]
			head = head[end+1:]
		} else {
			head = nil
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := trimHeader(line[:colon])
		if asciiEqualFold(name, "content-length") {
			length, _ := strconv.Atoi(bytesToString(trimHeader(line[colon+1:])))
			if length < 0 {
				return 0
			}
			return length
		}
	}
	return 0
}

func parseRequestLine(line []byte, req *httpRequest) error {
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	first := bytes.IndexByte(line, ' ')
	if first <= 0 {
		return errors.New("bad request line")
	}
	req.method = canonicalMethod(line[:first])
	rest := line[first+1:]
	if second := bytes.IndexByte(rest, ' '); second >= 0 {
		req.target = bytesToString(rest[:second])
	} else {
		req.target = bytesToString(rest)
	}
	return nil
}

func parseHeaders(head []byte, req *httpRequest) {
	for len(head) > 0 {
		end := bytes.IndexByte(head, '\n')
		line := head
		if end >= 0 {
			line = head[:end]
			head = head[end+1:]
		} else {
			head = nil
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := trimHeader(line[:colon])
		value := bytesToString(trimHeader(line[colon+1:]))
		switch {
		case asciiEqualFold(name, "host"):
			req.host = value
		case asciiEqualFold(name, "authorization"):
			req.authorization = value
		case asciiEqualFold(name, "git-protocol"):
			req.gitProtocol = value
		case asciiEqualFold(name, "connection"):
			req.close = strings.EqualFold(value, "close")
		}
	}
}

// readCachedRequest recognizes an exact repeat of the request that populated
// this connection's prebuilt GET cache. It skips request-line and header parsing
// entirely; any different byte sequence falls through to readRequest.
func readCachedRequest(cs *connState, conn net.Conn, req *httpRequest) (bool, error) {
	if len(cs.cacheBody) == 0 || len(cs.cacheReq) == 0 || cs.cacheGen != responseGen.Load() {
		return false, nil
	}
	need := len(cs.cacheReq)
	if cs.n < need {
		cs.grow(need)
		// Read at most one TCP chunk. A different, shorter request that cannot
		// fill the cached length must fall through to the normal parser instead
		// of blocking forever waiting for bytes that will never arrive.
		got, err := conn.Read(cs.raw[cs.n:need])
		if err != nil {
			return false, err
		}
		cs.n += got
	}
	if cs.n < need || !bytes.Equal(cs.raw[:need], cs.cacheReq) {
		return false, nil
	}
	*req = httpRequest{method: "GET", target: cs.cacheTarget, close: cs.cacheClose}
	cs.consumed = need
	return true, nil
}

func readRequest(cs *connState, conn net.Conn, req *httpRequest) error {
	*req = httpRequest{}
	for {
		headEnd, sepLen := findHeadEnd(cs.raw[:cs.n])
		if headEnd >= 0 {
			head := cs.raw[:headEnd]
			total := headEnd + sepLen + scanContentLength(head)
			cs.grow(total)
			head = cs.raw[:headEnd]
			if err := parseRequestLine(head, req); err != nil {
				return err
			}
			parseHeaders(head, req)
			bodyStart := headEnd + sepLen
			for cs.n < total {
				got, err := conn.Read(cs.raw[cs.n:total])
				if err != nil {
					return err
				}
				cs.n += got
			}
			req.body = cs.raw[bodyStart:total]
			cs.consumed = total
			return nil
		}
		if cs.n == len(cs.raw) {
			cs.grow(len(cs.raw) * 2)
		}
		got, err := conn.Read(cs.raw[cs.n:])
		if err != nil {
			return err
		}
		cs.n += got
	}
}

func startListener() (net.Listener, int, error) {
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return nil, 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go acceptLoop(ln)
	return ln, port, nil
}

func acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
			tc.SetKeepAlive(true)
		}
		go serveConn(conn)
	}
}

func serveConn(conn net.Conn) {
	defer conn.Close()
	cs := connState{raw: make([]byte, 8*1024)}
	var req httpRequest
	for {
		hit, err := readCachedRequest(&cs, conn, &req)
		if err != nil {
			return
		}
		if !hit {
			if err := readRequest(&cs, conn, &req); err != nil {
				return
			}
			if req.method == "GET" && req.target == cs.cacheTarget && cs.cacheBody != nil &&
				cs.cacheGen == responseGen.Load() {
				hit = true
			}
		}
		if hit {
			if _, err := conn.Write(cs.cacheBody); err != nil {
				return
			}
			cs.consume()
			if req.close {
				return
			}
			continue
		}
		req.scratch = cs.scratch[:0]
		result := dispatch(&req)
		out := prepareResponse(result, req.method == "HEAD")
		// Git responses are prebuilt too, but their bytes depend on repository
		// state that can change without touching responseGen (for example an
		// advertisement served before a push on the same keep-alive connection).
		// Leave those to the per-repo atomic snapshot and only pin stable issue
		// GETs in the per-connection cache.
		if req.method == "GET" && result.Status == 200 && len(result.Prebuilt) > 0 &&
			!strings.HasPrefix(result.CacheKey, "git:") {
			cs.cacheTarget = strings.Clone(req.target)
			cs.cacheBody = result.Prebuilt
			cs.cacheReq = append(cs.cacheReq[:0], cs.raw[:cs.consumed]...)
			cs.cacheClose = req.close
			cs.cacheGen = responseGen.Load()
		}
		if _, err := conn.Write(out); err != nil {
			return
		}
		if result.Scratch {
			cs.scratch = out[:0]
		}
		cs.consume()
		if req.close {
			return
		}
	}
}

// prepareResponse returns the exact bytes for one response. Cached and
// prebuilt responses are returned directly, so the Write has no copy and no
// header reconstruction on the hot path.
func prepareResponse(result HTTPResult, head bool) []byte {
	if head {
		return buildResponse(nil, result, true)
	}
	if len(result.Prebuilt) > 0 {
		return result.Prebuilt
	}
	if result.CacheKey != "" {
		key := fullResponseKey{status: result.Status, ct: result.ContentType, key: result.CacheKey}
		if cached, ok := fullResponseCache.Load(key); ok {
			return cached.([]byte)
		}
		built := buildResponse(nil, result, false)
		fullResponseCache.Store(key, built)
		return built
	}
	return buildResponse(nil, result, false)
}

func buildResponse(dst []byte, result HTTPResult, head bool) []byte {
	status := result.Status
	if status == 0 {
		status = 200
	}
	reason := reasonPhrases[status]
	if reason == "" {
		reason = "OK"
	}
	dst = append(dst, "HTTP/1.1 "...)
	dst = strconv.AppendInt(dst, int64(status), 10)
	dst = append(dst, ' ')
	dst = append(dst, reason...)
	dst = append(dst, "\r\n"...)
	ct := result.ContentType
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	dst = append(dst, "content-type: "...)
	dst = append(dst, ct...)
	dst = append(dst, "\r\n"...)
	dst = append(dst, "content-length: "...)
	dst = strconv.AppendInt(dst, int64(len(result.Body)), 10)
	dst = append(dst, "\r\n"...)
	for k, v := range result.Headers {
		dst = append(dst, k...)
		dst = append(dst, ": "...)
		dst = append(dst, v...)
		dst = append(dst, "\r\n"...)
	}
	dst = append(dst, "\r\n"...)
	if !head {
		dst = append(dst, result.Body...)
	}
	return dst
}

func splitTarget(raw string) (string, string) {
	q := strings.IndexByte(raw, '?')
	if q < 0 {
		return raw, ""
	}
	return raw[:q], raw[q+1:]
}

// queryGet parses the tiny subset of query strings the forge consumes without
// materializing a url.Values map. Only encoded values pay an allocation.
func queryGet(query, key string) string {
	for query != "" {
		part := query
		if i := strings.IndexByte(query, '&'); i >= 0 {
			part = query[:i]
			query = query[i+1:]
		} else {
			query = ""
		}
		name, value := part, ""
		if i := strings.IndexByte(part, '='); i >= 0 {
			name, value = part[:i], part[i+1:]
		}
		if name == key {
			return safeDecode(value)
		}
	}
	return ""
}

func splitPath(raw string, dst []string) []string {
	n := 0
	if raw == "" || raw == "/" {
		return dst[:0]
	}
	if raw[0] == '/' {
		raw = raw[1:]
	}
	for {
		i := strings.IndexByte(raw, '/')
		if i < 0 {
			if n < len(dst) {
				dst[n] = safeDecode(raw)
				n++
			}
			return dst[:n]
		}
		if n < len(dst) {
			dst[n] = safeDecode(raw[:i])
			n++
		}
		raw = raw[i+1:]
	}
}

func authenticated(req *httpRequest) bool {
	if shopToken == "" {
		return true
	}
	auth := req.authorization
	if i := strings.IndexByte(auth, ' '); i > 0 {
		auth = strings.TrimSpace(auth[i+1:])
	}
	return auth == shopToken
}

func requestID() string {
	return constantRequestID
}

func jsonResult(status int, body interface{}, extra map[string]string) HTTPResult {
	b, _ := jsonMarshal(body)
	b = append(b, '\n')
	h := map[string]string{"x-github-request-id": requestID()}
	for k, v := range extra {
		h[k] = v
	}
	return HTTPResult{Status: status, ContentType: JSONType, Body: b, Headers: h}
}

func textResult(status int, text string) HTTPResult {
	return HTTPResult{Status: status, ContentType: "text/plain; charset=utf-8", Body: stringToBytes(text)}
}

func dispatch(req *httpRequest) HTTPResult {
	path, query := splitTarget(req.target)

	if path == "/healthz" {
		return textResult(200, "ok\n")
	}

	// Hand-rolled git route: /owner/repo.git/{info/refs|git-upload-pack|git-receive-pack}.
	if len(path) > 1 && path[0] == '/' {
		parts := splitPath(path, req.segBuf[:])
		if len(parts) >= 3 && strings.HasSuffix(parts[1], ".git") {
			repo := findRepo(parts[0], strings.TrimSuffix(parts[1], ".git"))
			if repo != nil {
				if len(parts) == 4 && parts[2] == "info" && parts[3] == "refs" {
					service := queryGet(query, "service")
					if service != "git-upload-pack" && service != "git-receive-pack" {
						return textResult(400, "unsupported service\n")
					}
					return handleInfoRefs(repo, service, req.gitProtocol)
				}
				if len(parts) == 3 && parts[2] == "git-upload-pack" {
					return handleUploadPack(repo, req.body)
				}
				if len(parts) == 3 && parts[2] == "git-receive-pack" {
					return handleReceivePack(repo, req.body)
				}
			}
		}
	}

	if path == "/api/graphql" || path == "/api/graphql/" {
		if !authenticated(req) {
			return unauthorizedResult()
		}
		if req.method != "POST" {
			return jsonResult(405, map[string]interface{}{"message": "Method not allowed"}, nil)
		}
		return handleGraphql(bytesToString(req.body))
	}

	if path == "/mcp" || path == "/mcp/" {
		if !authenticated(req) {
			return unauthorizedResult()
		}
		if req.method != "POST" {
			if req.method == "DELETE" {
				return HTTPResult{Status: 204, Headers: map[string]string{}}
			}
			return jsonResult(405, map[string]interface{}{"message": "Method not allowed"}, nil)
		}
		if len(req.body) == 0 {
			return jsonResult(400, map[string]interface{}{
				"jsonrpc": "2.0", "id": nil,
				"error": map[string]interface{}{"code": -32700, "message": "Parse error: empty body"},
			}, nil)
		}
		return handleMcp(bytesToString(req.body))
	}

	const prefix = "/api/v3"
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		if !authenticated(req) {
			return unauthorizedResult()
		}
		residual := ""
		if len(path) > len(prefix)+1 {
			residual = path[len(prefix)+1:]
		} else if len(path) == len(prefix)+1 {
			residual = ""
		}
		segments := splitPath(residual, req.segBuf[:])
		host := req.host
		if host == "" {
			host = "localhost"
		}
		var q url.Values
		if query != "" {
			q, _ = url.ParseQuery(query)
		}
		return handleRest(req.method, segments, q, bytesToString(req.body), host, req.scratch)
	}

	return jsonResult(404, map[string]interface{}{
		"message": "Not Found", "documentation_url": "https://docs.github.com/rest",
	}, nil)
}

func unauthorizedResult() HTTPResult {
	return jsonResult(401, map[string]interface{}{
		"message": "Requires authentication", "documentation_url": "https://docs.github.com/rest",
	}, nil)
}
