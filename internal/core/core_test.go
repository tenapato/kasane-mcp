package core

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRememberValidation(t *testing.T) {
	for _, in := range []RememberInput{{Title: "x"}, {Content: "x"}, {Title: "x", Content: strings.Repeat("a", 65537)}, {ID: "not-uuid", Title: "x", Content: "x"}} {
		if err := in.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("invalid input accepted: %v", err)
		}
	}
	in := RememberInput{Title: "  Deployment ", Content: " exact text\n", Tags: []string{" Docker ", "docker", "", "Go"}}
	if err := in.Validate(); err != nil {
		t.Fatal(err)
	}
	if in.Title != "Deployment" || in.Content != " exact text\n" || strings.Join(in.Tags, ",") != "docker,go" {
		t.Fatalf("normalization lost content or tags: %+v", in)
	}
}

func TestContextBudgetPreservesUnicodeAndProvenance(t *testing.T) {
	ms := []Memory{{ID: "abc", Title: "Tokyo", Source: "session", Content: strings.Repeat("東京", 100)}}
	got := PackContext(ms, 80)
	if got.Characters > 80 || !utf8.ValidString(got.Text) || !got.Truncated {
		t.Fatalf("invalid budget result %+v", got)
	}
	if !strings.Contains(got.Text, "abc") || !strings.Contains(got.Text, "session") {
		t.Fatal("missing provenance")
	}
	if len(got.MemoryIDs) != 1 {
		t.Fatal("missing IDs")
	}
}

func TestSearchRejectsUnboundedRequests(t *testing.T) {
	for _, in := range []SearchInput{{Limit: 51}, {Offset: -1}, {Query: strings.Repeat("x", 2049)}} {
		if !errors.Is(in.Validate(), ErrInvalid) {
			t.Fatal("invalid search accepted")
		}
	}
	in := SearchInput{Query: " docker "}
	if err := in.Validate(); err != nil || in.Limit != 10 || in.Query != "docker" {
		t.Fatalf("bad defaults: %+v %v", in, err)
	}
}
