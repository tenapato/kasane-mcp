// Package decision connects to an administrator-configured decision service.
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrUnavailable = errors.New("decision service unavailable; no relevance decision was made")
var ErrInvalid = errors.New("decision request rejected; check input sizes, paths and scopes")

type Rule struct {
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Required bool     `json:"required"`
	Paths    []string `json:"paths"`
}
type Request struct {
	Task    string   `json:"task"`
	Paths   []string `json:"paths"`
	Context *string  `json:"context"`
	Rules   []Rule   `json:"rules"`
}
type Decision struct {
	RuleID      string   `json:"rule_id"`
	Include     bool     `json:"include"`
	Reason      string   `json:"reason"`
	Probability *float64 `json:"probability"`
}
type Response struct {
	DecisionID         string     `json:"decision_id"`
	Mode               string     `json:"mode"`
	ModelVersion       *string    `json:"model_version"`
	CalibrationVersion *string    `json:"calibration_version"`
	Calibrated         bool       `json:"calibrated"`
	Coverage           string     `json:"coverage"`
	Decisions          []Decision `json:"decisions"`
}
type Client struct {
	origin, token string
	http          *http.Client
}

func New(origin, token string) (*Client, error) {
	if origin == "" && token == "" {
		return nil, nil
	}
	u, e := url.Parse(origin)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("DECISION_API_URL must be an http(s) origin")
	}
	if len(token) < 32 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return nil, errors.New("DECISION_API_TOKEN must be 32–4096 bytes without whitespace")
	}
	for _, b := range []byte(token) {
		if b < 33 || b > 126 {
			return nil, errors.New("DECISION_API_TOKEN must contain printable ASCII bytes")
		}
	}
	return &Client{strings.TrimRight(origin, "/"), token, &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Status(ctx context.Context) string {
	if c == nil {
		return "disabled"
	}
	r, e := http.NewRequestWithContext(ctx, http.MethodGet, c.origin+"/readyz", nil)
	if e != nil {
		return "unavailable"
	}
	res, e := c.http.Do(r)
	if e != nil {
		return "unavailable"
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "unavailable"
	}
	return "ready"
}

// Check sends a fixed, public request that proves authenticated relevance works.
// It never includes a workspace, saved practice, user text, or local file content.
func (c *Client) Check(ctx context.Context) (status, mode, decisionID string) {
	if c == nil {
		return "disabled", "", ""
	}
	in := Request{
		Task:  "Diagnostic: retain the required convention for this synthetic request.",
		Rules: []Rule{{ID: "kaname-diagnostic-required", Text: "Keep this required diagnostic convention.", Required: true}},
	}
	out, state := c.relevance(ctx, in)
	if state == "unauthorized" {
		return "unauthorized", "", ""
	}
	if state == "invalid_response" {
		return "invalid_response", "", ""
	}
	if state != "ready" {
		return "unavailable", "", ""
	}
	// A response ID is opaque metadata, never a channel for upstream text.
	if !safeDiagnosticID(out.DecisionID) {
		return "invalid_response", "", ""
	}
	return "ready", out.Mode, out.DecisionID
}

func safeDiagnosticID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func (c *Client) Relevance(ctx context.Context, in Request) (Response, error) {
	out, state := c.relevance(ctx, in)
	switch state {
	case "ready":
		return out, nil
	case "invalid_request":
		return Response{}, ErrInvalid
	default:
		return Response{}, ErrUnavailable
	}
}

// relevance is shared by the workspace API and synthetic diagnostic. The public
// Relevance method keeps its existing two-error contract.
func (c *Client) relevance(ctx context.Context, in Request) (Response, string) {
	var out Response
	if c == nil {
		return out, "unavailable"
	}
	if strings.TrimSpace(in.Task) == "" || len(in.Task) > 16384 || len(in.Paths) > 64 || len(in.Rules) == 0 || len(in.Rules) > 64 || (in.Context != nil && len(*in.Context) > 65536) {
		return out, "invalid_request"
	}
	if in.Paths == nil {
		in.Paths = []string{}
	}
	seen := map[string]bool{}
	for i := range in.Rules {
		r := &in.Rules[i]
		if r.Paths == nil {
			r.Paths = []string{}
		}
		if seen[r.ID] || r.ID == "" || len(r.ID) > 256 || strings.TrimSpace(r.Text) == "" || len(r.Text) > 8192 || len(r.Paths) > 64 {
			return out, "invalid_request"
		}
		seen[r.ID] = true
	}
	body, e := json.Marshal(in)
	if e != nil || len(body) > 1<<20 {
		return out, "invalid_request"
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+"/v1/relevance", bytes.NewReader(body))
	if e != nil {
		return out, "unavailable"
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, e := c.http.Do(req)
	if e != nil {
		return out, "unavailable"
	}
	defer res.Body.Close()
	if res.StatusCode == 401 || res.StatusCode == 403 {
		return out, "unauthorized"
	}
	if res.StatusCode == 400 || res.StatusCode == 413 {
		return out, "invalid_request"
	}
	if res.StatusCode != 200 {
		return out, "unavailable"
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, (1<<20)+1))
	if e != nil || len(b) > 1<<20 {
		return out, "invalid_response"
	}
	if json.Unmarshal(b, &out) != nil || !completeResponse(b) || !validResponse(out, in) {
		return Response{}, "invalid_response"
	}
	return out, "ready"
}
func validResponse(out Response, in Request) bool {
	if out.DecisionID == "" || (out.Mode != "baseline" && out.Mode != "model") || (out.Coverage != "complete" && out.Coverage != "partial") || len(out.Decisions) != len(in.Rules) {
		return false
	}
	if out.Mode == "baseline" && (out.ModelVersion != nil || out.CalibrationVersion != nil || out.Calibrated || out.Coverage != "complete") {
		return false
	}
	for i, d := range out.Decisions {
		if d.RuleID != in.Rules[i].ID || (in.Rules[i].Required && (!d.Include || d.Reason != "required")) {
			return false
		}
		switch d.Reason {
		case "required", "scope_match":
			if !d.Include {
				return false
			}
		case "abstained":
			if d.Include {
				return false
			}
		case "model", "keyword":
		default:
			return false
		}
		if d.Probability != nil && (math.IsNaN(*d.Probability) || *d.Probability < 0 || *d.Probability > 1) {
			return false
		}
		if out.Mode == "baseline" && (d.Probability != nil || d.Reason == "model") {
			return false
		}
	}
	return true
}

// Missing boolean/null-valued fields must not silently acquire Go zero values.
func completeResponse(b []byte) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return false
	}
	for _, key := range []string{"decision_id", "mode", "model_version", "calibration_version", "calibrated", "coverage", "decisions"} {
		if _, ok := fields[key]; !ok {
			return false
		}
	}
	if string(fields["calibrated"]) != "true" && string(fields["calibrated"]) != "false" {
		return false
	}
	var decisions []map[string]json.RawMessage
	if json.Unmarshal(fields["decisions"], &decisions) != nil {
		return false
	}
	for _, d := range decisions {
		for _, key := range []string{"rule_id", "include", "reason", "probability"} {
			if _, ok := d[key]; !ok {
				return false
			}
		}
		if string(d["include"]) != "true" && string(d["include"]) != "false" {
			return false
		}
	}
	return true
}
