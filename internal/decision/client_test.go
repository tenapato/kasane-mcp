package decision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestConfiguration(t *testing.T) {
	if c, e := New("", ""); c != nil || e != nil {
		t.Fatal(c, e)
	}
	for _, url := range []string{"file:///etc/passwd", "http://host/path", "http://user:pass@host", "https://host?q=x", "https://host#x", ""} {
		if _, e := New(url, strings.Repeat("s", 32)); e == nil {
			t.Fatalf("accepted %s", url)
		}
	}
	if _, e := New("http://service:8080", "short"); e == nil {
		t.Fatal("short token accepted")
	}
}
func TestAuthenticatedRoundTripAndFailures(t *testing.T) {
	mode := "good"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(200)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("s", 32) {
			t.Error("missing token")
		}
		var in Request
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Paths == nil || in.Rules[0].Paths == nil {
			t.Error("invalid payload")
		}
		switch mode {
		case "redirect":
			http.Redirect(w, r, "http://127.0.0.1:1", 302)
			return
		case "error":
			w.WriteHeader(500)
			_, _ = w.Write([]byte("private token and source"))
			return
		case "invalid":
			w.WriteHeader(400)
			return
		case "large":
			_, _ = w.Write([]byte(strings.Repeat("x", (1<<20)+1)))
			return
		}
		result := Response{DecisionID: "d", Mode: "baseline", Coverage: "complete", Decisions: []Decision{{RuleID: "r", Include: true, Reason: "required"}}}
		if mode == "wrong-id" {
			result.Decisions[0].RuleID = "another"
		}
		if mode == "suppressed" {
			result.Decisions[0].Include = false
		}
		if mode == "probability" {
			p := .8
			result.Decisions[0].Probability = &p
		}
		_ = json.NewEncoder(w).Encode(result)
	}))
	defer srv.Close()
	c, e := New(srv.URL, strings.Repeat("s", 32))
	if e != nil {
		t.Fatal(e)
	}
	in := Request{Task: "task", Rules: []Rule{{ID: "r", Text: "never log secrets", Required: true}}}
	if c.Status(context.Background()) != "ready" {
		t.Fatal("not ready")
	}
	if out, e := c.Relevance(context.Background(), in); e != nil || out.DecisionID != "d" {
		t.Fatal(out, e)
	}
	for _, m := range []string{"redirect", "error", "large", "wrong-id", "suppressed", "probability"} {
		mode = m
		if _, e := c.Relevance(context.Background(), in); e != ErrUnavailable {
			t.Fatalf("%s: %v", m, e)
		}
	}
	mode = "invalid"
	if _, e := c.Relevance(context.Background(), in); e != ErrInvalid {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.Relevance(ctx, in); e != ErrUnavailable {
		t.Fatal(e)
	}
	var disabled *Client
	if disabled.Status(context.Background()) != "disabled" {
		t.Fatal("disabled health")
	}
	if _, e := disabled.Relevance(context.Background(), in); e != ErrUnavailable {
		t.Fatal(e)
	}
}
func TestDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer srv.Close()
	c, _ := New(srv.URL, strings.Repeat("s", 32))
	c.http.Timeout = 20 * time.Millisecond
	if _, e := c.Relevance(context.Background(), Request{Task: "task", Rules: []Rule{{ID: "r", Text: "rule"}}}); e != ErrUnavailable {
		t.Fatal(e)
	}
}

func TestMalformedResponseContract(t *testing.T) {
	in := Request{Task: "task", Rules: []Rule{{ID: "r", Text: "rule"}}}
	base := Response{DecisionID: "d", Mode: "baseline", Coverage: "complete", Decisions: []Decision{{RuleID: "r", Include: true, Reason: "keyword"}}}
	if !validResponse(base, in) {
		t.Fatal("valid response rejected")
	}
	for _, mutate := range []func(*Response){
		func(r *Response) { r.Coverage = "partial" },
		func(r *Response) { r.Decisions[0].Reason = "model" },
		func(r *Response) { r.Decisions[0].Reason = "abstained" },
		func(r *Response) { r.Decisions[0].Reason = "scope_match"; r.Decisions[0].Include = false },
	} {
		candidate := base
		candidate.Decisions = append([]Decision(nil), base.Decisions...)
		mutate(&candidate)
		if validResponse(candidate, in) {
			t.Fatalf("malformed response accepted: %+v", candidate)
		}
	}
	raw, _ := json.Marshal(base)
	if !completeResponse(raw) {
		t.Fatal("complete response rejected")
	}
	var object map[string]any
	_ = json.Unmarshal(raw, &object)
	delete(object, "calibrated")
	broken, _ := json.Marshal(object)
	if completeResponse(broken) {
		t.Fatal("missing calibrated accepted")
	}
	_ = json.Unmarshal(raw, &object)
	object["decisions"].([]any)[0].(map[string]any)["include"] = nil
	broken, _ = json.Marshal(object)
	if completeResponse(broken) {
		t.Fatal("null include accepted")
	}
	if _, e := New("http://service", strings.Repeat("a", 31)+"\x00"); e == nil {
		t.Fatal("control character token accepted")
	}
}
