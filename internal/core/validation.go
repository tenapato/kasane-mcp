package core

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

func UUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func ValidID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	return err == nil
}

func (in *RememberInput) Validate() error {
	in.Title = strings.TrimSpace(in.Title)
	if in.Kind == "" {
		in.Kind = "memory"
	}
	if in.Kind != "memory" && in.Kind != "stack" && in.Kind != "practice" {
		return fmt.Errorf("%w: unknown memory kind", ErrInvalid)
	}
	if in.Title == "" || len(in.Title) > 240 || strings.TrimSpace(in.Content) == "" || len(in.Content) > 65536 || !utf8.ValidString(in.Content) || !utf8.ValidString(in.Title) {
		return fmt.Errorf("%w: title (1–240 bytes) and content (1–65536 bytes) required", ErrInvalid)
	}
	if !strings.ContainsFunc(in.Title+in.Content, unicode.IsLetter) && !strings.ContainsFunc(in.Title+in.Content, unicode.IsNumber) {
		return fmt.Errorf("%w: memory must contain searchable words", ErrInvalid)
	}
	if len(in.Source) > 2048 || len(in.Tags) > 32 || len(in.IdempotencyKey) > 128 {
		return fmt.Errorf("%w: metadata too long", ErrInvalid)
	}
	if in.ID != "" && (!ValidID(in.ID) || in.ExpectedRevision < 1) {
		return fmt.Errorf("%w: updates require ID and expected_revision", ErrInvalid)
	}
	seen := map[string]bool{}
	tags := []string{}
	for _, tag := range in.Tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if len(tag) > 64 {
			return fmt.Errorf("%w: tag too long", ErrInvalid)
		}
		if tag != "" && !seen[tag] {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}
	in.Tags = tags
	return nil
}

func (in *SearchInput) Validate() error {
	in.Query = strings.TrimSpace(in.Query)
	in.Tag = strings.ToLower(strings.TrimSpace(in.Tag))
	if in.Limit == 0 {
		in.Limit = 10
	}
	if in.Limit < 1 || in.Limit > 50 || in.Offset < 0 || in.Offset > 10000 || len(in.Query) > 2048 || len(in.Tag) > 64 {
		return fmt.Errorf("%w: search exceeds allowed bounds", ErrInvalid)
	}
	if in.Kind != "" && in.Kind != "memory" && in.Kind != "stack" && in.Kind != "practice" {
		return fmt.Errorf("%w: unknown kind", ErrInvalid)
	}
	return nil
}

func PackContext(memories []Memory, budget int) ContextResult {
	out := ContextResult{MemoryIDs: []string{}}
	if budget < 1 {
		return out
	}
	var text strings.Builder
	for _, m := range memories {
		header := fmt.Sprintf("[%s] %s\nSource: %s\n", m.ID, m.Title, m.Source)
		remaining := budget - utf8.RuneCountInString(text.String())
		h := utf8.RuneCountInString(header)
		if remaining <= h {
			out.Truncated = true
			break
		}
		body := []rune(m.Content + "\n\n")
		if len(body) > remaining-h {
			body = body[:remaining-h]
			out.Truncated = true
		}
		text.WriteString(header)
		text.WriteString(string(body))
		out.MemoryIDs = append(out.MemoryIDs, m.ID)
		if out.Truncated {
			break
		}
	}
	out.Text = text.String()
	out.Characters = utf8.RuneCountInString(out.Text)
	return out
}
