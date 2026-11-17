// Raw HTTP/1.1 server on net + bufio (no net/http: smaller binary, less
// overhead), plus the shared HTTP plumbing: auth, JSON bodies, request ids,
// pagination. One connection is one goroutine; dispatch is serialized by a
// single mutex in main.go, so the store never sees a race.

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Ctx is the per-process request context.
type Ctx struct {
	Forge  *Forge
	Token  string
	ReqSeq int
}

// Request is one parsed HTTP request.
type Request struct {
	Method  string
	Target  string
	Path    string // raw path, percent-encoded as sent
	Query   url.Values
	Headers map[string]string // lowercased names; duplicates joined with ", "
	Body    []byte
	Host    string
	Version string // "HTTP/1.1" or "HTTP/1.0"
}

// Response is one reply: status, headers in order, body.
type Response struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

func statusText(code int) string {
	switch code {
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted"
	case 204:
		return "No Content"
	case 400:
		return "Bad Request"
	case 401:
		return "Unauthorized"
	case 404:
		return "Not Found"
	case 405:
		return "Method Not Allowed"
	case 409:
		return "Conflict"
	case 422:
		return "Unprocessable Entity"
	case 500:
		return "Internal Server Error"
	default:
		return "Status"
	}
}

// ---------------------------------------------------------------- JSON

// marshalJSON encodes like Node's JSON.stringify: no HTML escaping, no
// trailing newline.
func marshalJSON(v interface{}) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// parseJSONBody parses a JSON body; an empty body is {} and malformed JSON fails.
func parseJSONBody(buf []byte) (interface{}, bool) {
	if len(buf) == 0 {
		return map[string]interface{}{}, true
	}
	dec := json.NewDecoder(bytes.NewReader(buf))
	var v interface{}
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// isRecord reports whether v is a JSON object.
func isRecord(v interface{}) (map[string]interface{}, bool) {
	m, ok := v.(map[string]interface{})
	return m, ok
}

// asString extracts a string field, or "".
func asString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// asInt extracts an integer from a JSON number (float64) or a numeric string.
func asInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case string:
		i, err := strconv.Atoi(n)
		if err == nil {
			return i, true
		}
		f, err := strconv.ParseFloat(n, 64)
		if err == nil {
			return int(f), true
		}
	case int:
		return n, true
	}
	return 0, false
}

// asBool extracts a bool, or false.
func asBool(v interface{}) bool {
	b, _ := v.(bool)
	return b
}

// ---------------------------------------------------------------- plumbing

// nextRequestID returns a deterministic counter-based request id.
func nextRequestID(ctx *Ctx) string {
	ctx.ReqSeq++
	return "req-" + itoa(ctx.ReqSeq)
}

// checkAuth is true when the request carries the trial's bearer <redacted>
func checkAuth(req *Request, ctx *Ctx) bool {
	header := req.Headers["authorization"]
	if ctx.Token == "" {
		return strings.HasPrefix(header, "Bearer ") && len(header) > 7
	}
	return header == "Bearer "+ctx.Token
}

// PageParams holds page/per_page from the query, clamped to sane bounds.
type PageParams struct {
	Page    int
	PerPage int
}

func pageParams(query url.Values, defaultPerPage int) PageParams {
	page := 1
	if raw := query.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n != 0 {
			page = n
		}
	}
	if page < 1 {
		page = 1
	}
	perPage := defaultPerPage
	if raw := query.Get("per_page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			if n < 1 {
				n = 1
			}
			if n > 100 {
				n = 100
			}
			perPage = n
		}
	}
	return PageParams{Page: page, PerPage: perPage}
}

// paginate slices items to one page and builds the Link header (same-origin
// absolute URL) when another page follows.
func paginate[T any](items []T, params PageParams, host, pathname string, query url.Values) (page []T, link string) {
	start := (params.Page - 1) * params.PerPage
	if start > len(items) {
		start = len(items)
	}
	end := start + params.PerPage
	if end > len(items) {
		end = len(items)
	}
	page = items[start:end]
	if start+params.PerPage < len(items) {
		next := cloneValues(query)
		next.Set("page", itoa(params.Page+1))
		next.Set("per_page", itoa(params.PerPage))
		link = "<http://" + host + pathname + "?" + next.Encode() + ">; rel=\"next\""
	}
	return page, link
}

func cloneValues(v url.Values) url.Values {
	out := url.Values{}
	for k, vals := range v {
		out[k] = append([]string(nil), vals...)
	}
	return out
}

// pathSegments splits a pathname into decoded segments (empty segments dropped).
func pathSegments(pathname string) []string {
	raw := strings.Split(pathname, "/")
	out := []string{}
	for _, segment := range raw {
		if segment == "" {
			continue
		}
		if decoded, err := url.PathUnescape(segment); err == nil {
			segment = decoded
		}
		out = append(out, segment)
	}
	return out
}

// jsonResponse builds an application/json response.
func jsonResponse(status int, body interface{}, extra [][2]string) Response {
	text := marshalJSON(body)
	headers := [][2]string{
		{"Content-Type", "application/json"},
		{"Content-Length", itoa(len(text))},
	}
	headers = append(headers, extra...)
	return Response{Status: status, Headers: headers, Body: text}
}

// textResponse builds a response with an explicit content type.
func textResponse(status int, body []byte, contentType string, extra [][2]string) Response {
	headers := [][2]string{
		{"Content-Type", contentType},
		{"Content-Length", itoa(len(body))},
	}
	headers = append(headers, extra...)
	return Response{Status: status, Headers: headers, Body: body}
}

// ---------------------------------------------------------------- server

func serve(ln net.Listener, ctx *Ctx, handle func(*Ctx, *Request) Response) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go serveConn(conn, ctx, handle)
	}
}

func serveConn(conn net.Conn, ctx *Ctx, handle func(*Ctx, *Request) Response) {
	defer conn.Close()
	reader := bufio.NewReaderSize(conn, 65536)
	for {
		req, keepAlive, err := readRequest(reader)
		if err != nil {
			return
		}
		resp := func() (r Response) {
			defer func() {
				if rec := recover(); rec != nil {
					fmt.Fprintf(os.Stderr, "[entry] handler error: %v\n", rec)
					r = jsonResponse(500, map[string]interface{}{"message": "Internal Server Error"}, [][2]string{{"X-GitHub-Request-Id", nextRequestID(ctx)}})
				}
			}()
			return handle(ctx, req)
		}()
		if err := writeResponse(conn, resp, keepAlive); err != nil {
			return
		}
		if !keepAlive {
			return
		}
	}
}

func writeResponse(conn net.Conn, resp Response, keepAlive bool) error {
	var buf bytes.Buffer
	buf.WriteString("HTTP/1.1 " + itoa(resp.Status) + " " + statusText(resp.Status) + "\r\n")
	for _, h := range resp.Headers {
		buf.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	if keepAlive {
		buf.WriteString("Connection: keep-alive\r\n")
	} else {
		buf.WriteString("Connection: close\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(resp.Body)
	_, err := conn.Write(buf.Bytes())
	return err
}

// readRequest reads one request from the connection. keepAlive reports whether
// the connection may be reused for the next request.
func readRequest(reader *bufio.Reader) (*Request, bool, error) {
	line, err := readLine(reader)
	if err != nil {
		return nil, false, err
	}
	if line == "" {
		return nil, false, io.EOF
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return nil, false, fmt.Errorf("bad request line %q", line)
	}
	method, target, version := parts[0], parts[1], parts[2]
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		return nil, false, fmt.Errorf("unsupported version %q", version)
	}
	headers := map[string]string{}
	for {
		hline, err := readLine(reader)
		if err != nil {
			return nil, false, err
		}
		if hline == "" {
			break
		}
		colon := strings.IndexByte(hline, ':')
		if colon < 0 {
			return nil, false, fmt.Errorf("bad header line %q", hline)
		}
		name := strings.ToLower(strings.TrimSpace(hline[:colon]))
		value := strings.TrimSpace(hline[colon+1:])
		if prev, ok := headers[name]; ok {
			headers[name] = prev + ", " + value
		} else {
			headers[name] = value
		}
	}

	// Split the target into path and query (origin-form or absolute-form).
	path := target
	rawQuery := ""
	if idx := strings.IndexByte(target, '?'); idx >= 0 {
		path = target[:idx]
		rawQuery = target[idx+1:]
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		if u, err := url.Parse(path); err == nil {
			path = u.EscapedPath()
			if path == "" {
				path = "/"
			}
			rawQuery = u.RawQuery
		}
	}
	query, _ := url.ParseQuery(rawQuery)

	keepAlive := version == "HTTP/1.1"
	if connHeader := headers["connection"]; connHeader != "" {
		lower := strings.ToLower(connHeader)
		if strings.Contains(lower, "close") {
			keepAlive = false
		} else if strings.Contains(lower, "keep-alive") {
			keepAlive = true
		}
	}

	var body []byte
	if te := strings.ToLower(headers["transfer-encoding"]); strings.Contains(te, "chunked") {
		body, err = readChunked(reader)
		if err != nil {
			return nil, false, err
		}
	} else if cl := headers["content-length"]; cl != "" {
		n, err := strconv.Atoi(strings.TrimSpace(cl))
		if err != nil || n < 0 {
			return nil, false, fmt.Errorf("bad content-length %q", cl)
		}
		if n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(reader, body); err != nil {
				return nil, false, err
			}
		}
	}
	return &Request{
		Method:  method,
		Target:  target,
		Path:    path,
		Query:   query,
		Headers: headers,
		Body:    body,
		Host:    firstNonEmpty(headers["host"], "127.0.0.1"),
		Version: version,
	}, keepAlive, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	if len(line) > 65536 {
		return "", fmt.Errorf("header line too long")
	}
	return line, nil
}

func readChunked(reader *bufio.Reader) ([]byte, error) {
	var out bytes.Buffer
	for {
		line, err := readLine(reader)
		if err != nil {
			return nil, err
		}
		sizeStr := line
		if idx := strings.IndexByte(sizeStr, ';'); idx >= 0 {
			sizeStr = sizeStr[:idx]
		}
		size, err := strconv.ParseUint(strings.TrimSpace(sizeStr), 16, 64)
		if err != nil {
			return nil, fmt.Errorf("bad chunk size %q", line)
		}
		if size == 0 {
			// Trailers until an empty line.
			for {
				trailer, err := readLine(reader)
				if err != nil {
					return nil, err
				}
				if trailer == "" {
					break
				}
			}
			return out.Bytes(), nil
		}
		chunk := make([]byte, size)
		if _, err := io.ReadFull(reader, chunk); err != nil {
			return nil, err
		}
		out.Write(chunk)
		// Trailing CRLF after the chunk.
		if _, err := readLine(reader); err != nil {
			return nil, err
		}
	}
}
