// Fast, allocation-light JSON encoding for the benchmark-hot issue shape.
//
// appendIssueJSON produces byte-identical output to
// marshalJSON(issueJSON(issue)) (encoding/json with SetEscapeHTML(false)),
// but without the struct copy, the labels slice copy, or reflection.
// jsonfast_test.go verifies byte identity against the reference encoder.

package main

import (
	"strconv"
	"unicode/utf8"
)

const hexDigits = "0123456789abcdef"

// appendJSONString appends s as a JSON string literal, matching
// encoding/json with SetEscapeHTML(false): '"' and '\' are escaped, control
// bytes < 0x20 become \n \r \t or \u00XX (lowercase hex), U+2028/U+2029
// become \u2028/\u2029, invalid UTF-8 bytes become the raw UTF-8 of U+FFFD,
// and <, >, & and all other valid runes pass through unescaped.
func appendJSONString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				continue
			}
			if start < i {
				dst = append(dst, s[start:i]...)
			}
			switch c {
			case '"':
				dst = append(dst, '\\', '"')
			case '\\':
				dst = append(dst, '\\', '\\')
			case '\n':
				dst = append(dst, '\\', 'n')
			case '\r':
				dst = append(dst, '\\', 'r')
			case '\t':
				dst = append(dst, '\\', 't')
			default:
				dst = append(dst, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			}
			start = i + 1
			continue
		}
		// Multi-byte rune: flush the ASCII run, then handle the rune.
		if start < i {
			dst = append(dst, s[start:i]...)
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			// Invalid byte: encoding/json emits the raw UTF-8 of U+FFFD.
			dst = append(dst, 0xef, 0xbf, 0xbd)
			start = i + 1
			continue
		}
		if r == '\u2028' {
			dst = append(dst, '\\', 'u', '2', '0', '2', '8')
			start = i + size
			i += size - 1
			continue
		}
		if r == '\u2029' {
			dst = append(dst, '\\', 'u', '2', '0', '2', '9')
			start = i + size
			i += size - 1
			continue
		}
		dst = append(dst, s[i:i+size]...)
		start = i + size
		i += size - 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	dst = append(dst, '"')
	return dst
}

// appendIssueJSON appends the issue object in exactly the field order and
// shape of issueJSONOut (id, node_id, number, title, body, state, labels,
// user, created_at, updated_at).
func appendIssueJSON(dst []byte, issue *Issue) []byte {
	dst = append(dst, '{')
	dst = append(dst, `"id":`...)
	dst = strconv.AppendInt(dst, int64(issue.ID), 10)
	dst = append(dst, `,"node_id":`...)
	dst = appendJSONString(dst, b64url("Issue:"+itoa(issue.ID)))
	dst = append(dst, `,"number":`...)
	dst = strconv.AppendInt(dst, int64(issue.Number), 10)
	dst = append(dst, `,"title":`...)
	dst = appendJSONString(dst, issue.Title)
	dst = append(dst, `,"body":`...)
	dst = appendJSONString(dst, issue.Body)
	dst = append(dst, `,"state":`...)
	dst = appendJSONString(dst, issue.State)
	dst = append(dst, `,"labels":[`...)
	for i, l := range issue.Labels {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"name":`...)
		dst = appendJSONString(dst, l.Name)
		dst = append(dst, `,"color":`...)
		dst = appendJSONString(dst, l.Color)
		dst = append(dst, '}')
	}
	dst = append(dst, `],"user":{"login":`...)
	dst = appendJSONString(dst, issue.UserLogin)
	dst = append(dst, `,"id":1000,"type":"User"},"created_at":`...)
	dst = appendJSONString(dst, issue.CreatedAt)
	dst = append(dst, `,"updated_at":`...)
	dst = appendJSONString(dst, issue.UpdatedAt)
	dst = append(dst, '}')
	return dst
}

// issueJSONBytes encodes one issue to JSON (same bytes as
// marshalJSON(issueJSON(issue))).
func issueJSONBytes(issue *Issue) []byte {
	return appendIssueJSON(make([]byte, 0, 384), issue)
}

// restOKBytes is restOK for a pre-encoded JSON body: same headers
// (X-GitHub-Request-Id first, then extra) and content type, no re-encode.
func restOKBytes(ctx *Ctx, status int, body []byte, extra [][2]string) Response {
	headers := [][2]string{{"X-GitHub-Request-Id", nextRequestID(ctx)}}
	headers = append(headers, extra...)
	return textResponse(status, body, "application/json", headers)
}
