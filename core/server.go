package main

// Raw TCP HTTP/1.1 listener. No framework, one write per response, keep-alive,
// and a synchronous dispatcher so a request never leaves the accept goroutine.

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

var reasonPhrases = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content",
	400: "Bad Request", 401: "Unauthorized", 404: "Not Found",
	405: "Method Not Allowed", 409: "Conflict", 410: "Gone",
	412: "Precondition Failed", 422: "Unprocessable Entity",
	429: "Too Many Requests", 500: "Internal Server Error",
	501: "Not Implemented", 503: "Service Unavailable",
}

var bufferPool = sync.Pool{New: func() interface{} { b := make([]byte, 0, 64*1024); return &b }}

var gitRouteRegex = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(info/refs|git-upload-pack|git-receive-pack)$`)

type fullResponseKey struct {
	status int
	ct     string
	key    string
}

var fullResponseCache sync.Map // fullResponseKey -> []byte

const constantRequestID = "MF-00000001"

type httpRequest struct {
	method  string
	target  string
	headers map[string]string
	body    []byte
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
	reader := bufio.NewReaderSize(conn, 64*1024)
	bp := bufferPool.Get().(*[]byte)
	out := *bp
	defer func() { *bp = out[:0]; bufferPool.Put(bp) }()

	for {
		req, err := readRequest(reader)
		if err != nil {
			return
		}
		result := dispatch(&req)
		out = out[:0]
		out = appendResponse(out, result, req.method == "HEAD")
		if _, err := conn.Write(out); err != nil {
			return
		}
		if strings.EqualFold(req.headers["connection"], "close") {
			return
		}
	}
}

func readRequest(r *bufio.Reader) (httpRequest, error) {
	var req httpRequest
	line, err := r.ReadString('\n')
	if err != nil {
		return req, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return req, io.EOF
	}
	parts := strings.Split(line, " ")
	if len(parts) < 2 {
		return req, fmt.Errorf("bad request line")
	}
	req.method = strings.ToUpper(parts[0])
	req.target = parts[1]
	req.headers = make(map[string]string, 12)
	for {
		h, err := r.ReadString('\n')
		if err != nil {
			return req, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		colon := strings.IndexByte(h, ':')
		if colon > 0 {
			req.headers[strings.ToLower(strings.TrimSpace(h[:colon]))] = strings.TrimSpace(h[colon+1:])
		}
	}
	length := 0
	if v, ok := req.headers["content-length"]; ok {
		length, _ = strconv.Atoi(v)
		if length < 0 {
			length = 0
		}
	}
	if length > 0 {
		req.body = make([]byte, length)
		if _, err := io.ReadFull(r, req.body); err != nil {
			return req, err
		}
	} else {
		req.body = nil
	}
	return req, nil
}

func appendResponse(dst []byte, result HTTPResult, head bool) []byte {
	if !head && result.CacheKey != "" {
		key := fullResponseKey{status: result.Status, ct: result.ContentType, key: result.CacheKey}
		if cached, ok := fullResponseCache.Load(key); ok {
			return append(dst, cached.([]byte)...)
		}
		start := len(dst)
		dst = buildResponse(dst, result, false)
		fullResponseCache.Store(key, append([]byte(nil), dst[start:]...))
		return dst
	}
	return buildResponse(dst, result, head)
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
	dst = append(dst, "connection: keep-alive\r\n\r\n"...)
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

func authenticated(headers map[string]string) bool {
	expected := os.Getenv("MAKEFUN_SHOP_TOKEN")
	if expected == "" {
		return true
	}
	auth := headers["authorization"]
	if i := strings.IndexByte(auth, ' '); i > 0 {
		auth = strings.TrimSpace(auth[i+1:])
	}
	return auth == expected
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
	return HTTPResult{Status: status, ContentType: "text/plain; charset=utf-8", Body: []byte(text)}
}

func dispatch(req *httpRequest) HTTPResult {
	path, query := splitTarget(req.target)
	bodyText := string(req.body)

	if path == "/healthz" {
		return textResult(200, "ok\n")
	}

	if m := gitRouteRegex.FindStringSubmatch(path); m != nil {
		repo := findRepo(safeDecode(m[1]), safeDecode(m[2]))
		if repo == nil {
			return textResult(404, "repository not found\n")
		}
		action := m[3]
		if action == "info/refs" {
			v, _ := url.ParseQuery(query)
			service := v.Get("service")
			if service != "git-upload-pack" && service != "git-receive-pack" {
				return textResult(400, "unsupported service\n")
			}
			return handleInfoRefs(repo, service, req.headers["git-protocol"])
		}
		if action == "git-upload-pack" {
			return handleUploadPack(repo, req.body, req.headers["git-protocol"])
		}
		return handleReceivePack(repo, req.body)
	}

	if path == "/api/graphql" || path == "/api/graphql/" {
		if !authenticated(req.headers) {
			return unauthorizedResult()
		}
		if req.method != "POST" {
			return jsonResult(405, map[string]interface{}{"message": "Method not allowed"}, nil)
		}
		return handleGraphql(bodyText)
	}

	if path == "/mcp" || path == "/mcp/" {
		if !authenticated(req.headers) {
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
		return handleMcp(bodyText)
	}

	const prefix = "/api/v3"
	if path == prefix || strings.HasPrefix(path, prefix+"/") {
		if !authenticated(req.headers) {
			return unauthorizedResult()
		}
		residual := strings.TrimPrefix(path, prefix)
		residual = strings.TrimPrefix(residual, "/")
		segments := make([]string, 0, 8)
		if residual != "" {
			for _, s := range strings.Split(residual, "/") {
				segments = append(segments, safeDecode(s))
			}
		}
		q, _ := url.ParseQuery(query)
		host := req.headers["host"]
		if host == "" {
			host = "localhost"
		}
		return handleRest(req.method, segments, q, bodyText, host)
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
