package main

import (
	"bytes"
	"testing"
)

func TestAppendIssueJSONByteIdentical(t *testing.T) {
	cases := []*Issue{
		{ID: 1, Number: 1, Title: "hello", Body: "world", State: "open", UserLogin: "adaiversen", CreatedAt: "2026-11-17T00:00:00.000Z", UpdatedAt: "2026-11-17T00:00:00.000Z"},
		{ID: 42, Number: 7, Title: "", Body: "", State: "closed", UserLogin: "x", CreatedAt: "t", UpdatedAt: "t"},
		{ID: 3, Number: 3, Title: "quote \" backslash \\ slash / <tag> & amp", Body: "line1\nline2\rline3\ttab\x01\x1f\x7f", State: "open", UserLogin: "u", CreatedAt: "t", UpdatedAt: "t", Labels: []IssueLabel{{Name: "bug", Color: "ff0000"}, {Name: "a<b>&\"c\"", Color: ""}}},
		{ID: 4, Number: 4, Title: "héllo → ☃", Body: "  ", State: "open", UserLogin: "u", CreatedAt: "t", UpdatedAt: "t"},
		{ID: 5, Number: 5, Title: string([]byte{'a', 0xff, 0xfe, 'b'}), Body: string([]byte{0x80, 'x'}), State: "open", UserLogin: "u", CreatedAt: "t", UpdatedAt: "t"},
		{ID: 123456, Number: 999, Title: "long title with a lot of ordinary ascii text to exercise the fast path", Body: "body body body", State: "open", UserLogin: "adaiversen", CreatedAt: "2026-11-17T00:00:00.000Z", UpdatedAt: "2026-11-17T00:00:01.000Z", Labels: []IssueLabel{{Name: "enhancement", Color: "00ff00"}}},
	}
	for i, iss := range cases {
		want := marshalJSON(issueJSON(iss))
		got := issueJSONBytes(iss)
		if !bytes.Equal(got, want) {
			t.Fatalf("case %d:\n got %s\nwant %s", i, got, want)
		}
	}
}

func TestAppendJSONStringMatchesEncodingJSON(t *testing.T) {
	inputs := []string{
		"", "abc", "a\"b", "a\\b", "a\nb\rc\td", "\x00\x01\x1f\x7f",
		"a<b>&c", "  ", "héllo→", string([]byte{'a', 0xff, 'b'}),
		"tab\there", "quote\"",
	}
	for _, in := range inputs {
		want := marshalJSON(in)
		got := appendJSONString(nil, in)
		if !bytes.Equal(got, want) {
			t.Fatalf("input %q:\n got %s\nwant %s", in, got, want)
		}
	}
}
