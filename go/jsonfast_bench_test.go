package main

import (
	"encoding/json"
	"testing"
)

var benchIssue = &Issue{
	ID: 123, Number: 45, Title: "Fix the flaky test [bench 1]", Body: "Repro steps and a longer body to make the JSON non-trivial in size.",
	State: "open", UserLogin: "adaiversen", CreatedAt: "2026-11-17T00:00:00.000Z", UpdatedAt: "2026-11-17T00:00:00.000Z",
	Labels: []IssueLabel{{Name: "bug", Color: "ff0000"}},
}

var benchIssueBody = []byte(`{"title":"Fix the flaky test [bench 1]","body":"Repro steps and a longer body to make the JSON non-trivial in size."}`)

func BenchmarkIssueEncodeOld(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = marshalJSON(issueJSON(benchIssue))
	}
}

func BenchmarkIssueEncodeFast(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_ = issueJSONBytes(benchIssue)
	}
}

func BenchmarkIssueDecodeOld(b *testing.B) {
	for i := 0; i < b.N; i++ {
		_, _ = parseJSONBody(benchIssueBody)
	}
}

func BenchmarkIssueDecodeFast(b *testing.B) {
	for i := 0; i < b.N; i++ {
		var s issueCreateInput
		_ = json.Unmarshal(benchIssueBody, &s)
	}
}
