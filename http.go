// Raw TCP HTTP/1.1 server: one goroutine per connection, a reusable read
// buffer per connection, a single Write per response, zero allocations on
// the common paths.
package main

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
)

type request struct {
	method      string
	path        string
	query       string
	body        []byte
	host        string
	auth        string
	gitProtocol string
	contentType string
	connClose   bool
	expect100   bool
	contentLen  int
	// segments of path, without empty entries
	seg []string
}

type response struct {
	status int
	ctype  string // empty => application/json
	body   []byte
	// extra headers, e.g. Link
	extra [][2]string
	// noBody omits the body bytes (204 responses).
	noBody bool
}

var statusLines = map[int]string{
	200: "HTTP/1.1 200 OK\r\n",
	201: "HTTP/1.1 201 Created\r\n",
	202: "HTTP/1.1 202 Accepted\r\n",
	204: "HTTP/1.1 204 No Content\r\n",
	400: "HTTP/1.1 400 Bad Request\r\n",
	401: "HTTP/1.1 401 Unauthorized\r\n",
	404: "HTTP/1.1 404 Not Found\r\n",
	405: "HTTP/1.1 405 Method Not Allowed\r\n",
	409: "HTTP/1.1 409 Conflict\r\n",
	422: "HTTP/1.1 422 Unprocessable Entity\r\n",
	500: "HTTP/1.1 500 Internal Server Error\r\n",
}

var reqCounter uint64

func nextReqID() string {
	n := atomic.AddUint64(&reqCounter, 1)
	var b [24]byte
	b[0] = 'r'
	b[1] = 'e'
	b[2] = 'q'
	b[3] = '-'
	i := 4
	// hex, most significant first
	shift := uint(60)
	started := false
	for shift <= 60 {
		d := (n >> shift) & 0xf
		if d != 0 || started || shift == 0 {
			started = true
			b[i] = hexDigits[d]
			i++
		}
		if shift == 0 {
			break
		}
		shift -= 4
	}
	return string(b[:i])
}

// connState is the per-connection scratch.
type connState struct {
	c    net.Conn
	buf  []byte // read buffer (grows as needed, reused)
	rpos int    // start of unread data
	rend int    // end of buffered data
	wbuf []byte // response buffer (reused)
}

func serveConn(c net.Conn) {
	defer c.Close()
	cs := &connState{
		c:    c,
		buf:  make([]byte, 0, 65536),
		wbuf: make([]byte, 0, 8192),
	}
	for {
		req, ok := cs.readRequest()
		if !ok {
			return
		}
		resp := dispatch(req)
		cs.writeResponse(req, resp)
		if req.connClose {
			return
		}
	}
}

// readRequest reads one full request (headers + body) into cs.buf.
// The returned body aliases cs.buf and is valid until the next read.
func (cs *connState) readRequest() (*request, bool) {
	// Compact any leftover bytes to the front.
	if cs.rpos > 0 {
		copy(cs.buf, cs.buf[cs.rpos:cs.rend])
		cs.rend -= cs.rpos
		cs.rpos = 0
	}
	for {
		// Find the end of the headers.
		if idx := bytes.Index(cs.buf[:cs.rend], []byte("\r\n\r\n")); idx >= 0 {
			hdrEnd := idx + 4
			req, ok := parseHeaders(cs.buf[:hdrEnd])
			if !ok {
				return nil, false
			}
			// Read the body, if any.
			need := hdrEnd + req.contentLen
			for cs.rend < need {
				if cap(cs.buf) < need {
					nb := make([]byte, 0, need*2+1024)
					nb = append(nb, cs.buf[:cs.rend]...)
					cs.buf = nb
				}
				n, err := cs.c.Read(cs.buf[cs.rend:cap(cs.buf)])
				if n <= 0 || err != nil {
					return nil, false
				}
				cs.rend += n
			}
			req.body = cs.buf[hdrEnd:need]
			cs.rpos = need
			if req.expect100 {
				cs.c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
			}
			return req, true
		}
		if cs.rend == cap(cs.buf) {
			if cap(cs.buf) >= 1<<20 {
				return nil, false
			}
			nb := make([]byte, 0, cap(cs.buf)*2)
			nb = append(nb, cs.buf[:cs.rend]...)
			cs.buf = nb
		}
		n, err := cs.c.Read(cs.buf[cs.rend:cap(cs.buf)])
		if n <= 0 || err != nil {
			return nil, false
		}
		cs.rend += n
	}
}

// headerEq reports whether a header name equals s, case-insensitively.
func headerEq(name []byte, s string) bool {
	if len(name) != len(s) {
		return false
	}
	for i := range name {
		c := name[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != s[i] {
			return false
		}
	}
	return true
}

// parseHeaders parses the request line and headers from a byte slice that
// ends with \r\n\r\n. It never allocates except for the segment list.
func parseHeaders(b []byte) (*request, bool) {
	// Request line.
	lineEnd := bytes.IndexByte(b, '\n')
	if lineEnd < 0 {
		return nil, false
	}
	line := b[:lineEnd]
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	sp1 := bytes.IndexByte(line, ' ')
	if sp1 < 0 {
		return nil, false
	}
	sp2 := bytes.IndexByte(line[sp1+1:], ' ')
	if sp2 < 0 {
		return nil, false
	}
	req := &request{
		method: string(line[:sp1]),
	}
	target := string(line[sp1+1 : sp1+1+sp2])
	if q := strings.IndexByte(target, '?'); q >= 0 {
		req.path = target[:q]
		req.query = target[q+1:]
	} else {
		req.path = target
	}
	// Segments.
	for _, s := range strings.Split(req.path, "/") {
		if s != "" {
			req.seg = append(req.seg, s)
		}
	}
	// Headers.
	pos := lineEnd + 1
	for pos < len(b)-2 {
		eol := bytes.IndexByte(b[pos:], '\n')
		if eol < 0 {
			break
		}
		hline := b[pos : pos+eol]
		pos += eol + 1
		if len(hline) > 0 && hline[len(hline)-1] == '\r' {
			hline = hline[:len(hline)-1]
		}
		if len(hline) == 0 {
			break
		}
		colon := bytes.IndexByte(hline, ':')
		if colon < 0 {
			continue
		}
		name := hline[:colon]
		value := hline[colon+1:]
		for len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		// Header names arrive in any case (Node's fetch lowercases them);
		// match case-insensitively.
		switch {
		case headerEq(name, "content-length"):
			if n, err := strconv.Atoi(string(value)); err == nil && n >= 0 {
				req.contentLen = n
			}
		case headerEq(name, "authorization"):
			req.auth = string(value)
		case headerEq(name, "host"):
			req.host = string(value)
		case headerEq(name, "git-protocol"):
			req.gitProtocol = string(value)
		case headerEq(name, "content-type"):
			req.contentType = string(value)
		case headerEq(name, "connection"):
			if strings.EqualFold(string(value), "close") {
				req.connClose = true
			}
		case headerEq(name, "expect"):
			if strings.EqualFold(string(value), "100-continue") {
				req.expect100 = true
			}
		}
	}
	return req, true
}

// writeResponse builds the whole response in cs.wbuf and sends it in one
// Write call.
func (cs *connState) writeResponse(req *request, resp *response) {
	w := cs.wbuf[:0]
	statusLine, ok := statusLines[resp.status]
	if !ok {
		statusLine = "HTTP/1.1 500 Internal Server Error\r\n"
	}
	w = append(w, statusLine...)
	ctype := resp.ctype
	if ctype == "" {
		ctype = "application/json"
	}
	w = append(w, "Content-Type: "...)
	w = append(w, ctype...)
	w = append(w, "\r\nX-GitHub-Request-Id: "...)
	w = append(w, nextReqID()...)
	w = append(w, "\r\n"...)
	for _, h := range resp.extra {
		w = append(w, h[0]...)
		w = append(w, ": "...)
		w = append(w, h[1]...)
		w = append(w, "\r\n"...)
	}
	bodyLen := len(resp.body)
	if resp.noBody {
		bodyLen = 0
	}
	w = append(w, "Content-Length: "...)
	w = appendInt(w, bodyLen)
	w = append(w, "\r\nConnection: "...)
	if req.connClose {
		w = append(w, "close\r\n\r\n"...)
	} else {
		w = append(w, "keep-alive\r\n\r\n"...)
	}
	if !resp.noBody {
		w = append(w, resp.body...)
	}
	cs.c.Write(w)
	cs.wbuf = w[:0]
}

// --- routing --------------------------------------------------------------

// dispatch routes one request. Auth is enforced on every API route; only
// /healthz is open.
func dispatch(req *request) *response {
	if req.method == "GET" && req.path == "/healthz" {
		return &response{status: 200, body: []byte(`{"ok":true}`)}
	}
	if !authorized(req) {
		return &response{status: 401, body: jsonError("Requires authentication")}
	}
	if len(req.seg) >= 1 {
		switch req.seg[0] {
		case "api":
			if len(req.seg) >= 2 {
				switch req.seg[1] {
				case "v3":
					return handleREST(req)
				case "graphql":
					return handleGraphQL(req)
				}
			}
		case "mcp":
			return handleMCP(req)
		}
	}
	// Git smart HTTP: /{owner}/{repo}.git/info/refs and
	// /{owner}/{repo}.git/git-upload-pack | git-receive-pack. The client
	// always appends the .git suffix; strip it before lookup.
	if len(req.seg) == 4 && req.seg[2] == "info" && req.seg[3] == "refs" {
		req.seg[1] = strings.TrimSuffix(req.seg[1], ".git")
		return handleInfoRefs(req)
	}
	if len(req.seg) == 3 && (req.seg[2] == "git-upload-pack" || req.seg[2] == "git-receive-pack") {
		req.seg[1] = strings.TrimSuffix(req.seg[1], ".git")
		return handleGitService(req)
	}
	return &response{status: 404, body: jsonError("Not Found")}
}

func authorized(req *request) bool {
	if cfg.token == "" {
		return true // dev mode: any token accepted
	}
	return req.auth == "Bearer "+cfg.token
}

// queryParam returns the value of key in a URL query string.
func queryParam(q, key string) (string, bool) {
	for len(q) > 0 {
		kv := q
		if i := strings.IndexByte(q, '&'); i >= 0 {
			kv = q[:i]
			q = q[i+1:]
		} else {
			q = ""
		}
		if i := strings.IndexByte(kv, '='); i >= 0 {
			if kv[:i] == key {
				return unescapeQuery(kv[i+1:]), true
			}
		} else if kv == key {
			return "", true
		}
	}
	return "", false
}

func unescapeQuery(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '+' {
			b.WriteByte(' ')
		} else if c == '%' && i+2 < len(s) {
			if hi, ok1 := hexVal(s[i+1]); ok1 {
				if lo, ok2 := hexVal(s[i+2]); ok2 {
					b.WriteByte(hi<<4 | lo)
					i += 2
					continue
				}
			}
			b.WriteByte(c)
		} else {
			b.WriteByte(c)
		}
	}
	return b.String()
}

// paginate slices items for ?page=&per_page= and renders a Link header
// when more pages exist. Returns the page slice and extra headers.
func paginate[T any](items []T, req *request, base string) ([]T, [][2]string) {
	perPage := 30
	if v, ok := queryParam(req.query, "per_page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			if n > 100 {
				n = 100
			}
			perPage = n
		}
	}
	page := 1
	if v, ok := queryParam(req.query, "page"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			page = n
		}
	}
	start := (page - 1) * perPage
	if start >= len(items) {
		return nil, nil
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	var extra [][2]string
	if end < len(items) {
		origin := "http://" + req.host
		next := origin + base + "?per_page=" + itoa(perPage) + "&page=" + itoa(page+1)
		extra = append(extra, [2]string{"Link", "<" + next + `>; rel="next"`})
	}
	return items[start:end], extra
}
