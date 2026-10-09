package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/evidence"
	core "github.com/ifandonlyif-io/iff-apostille/apostille"
)

const Version = "0.1.0-alpha.2"
const maxResponse = 2 << 20

type Gateway struct {
	c        config.Config
	client   *http.Client
	store    *evidence.Store
	slots    chan struct{}
	projects map[string]chan struct{}
	draining atomic.Bool
}

func New(c config.Config, store *evidence.Store) (*Gateway, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// Non-streaming runtimes may wait for generation before sending headers.
	// Each operation supplies its own deadline (2s readiness, configured inference).
	tr := &http.Transport{Proxy: nil, MaxIdleConns: 16, MaxIdleConnsPerHost: 8, DisableCompression: true}
	g := &Gateway{c: c, store: store, client: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect_forbidden") }}, slots: make(chan struct{}, c.Active().MaxConcurrent), projects: map[string]chan struct{}{}}
	for _, p := range c.Projects {
		g.projects[p.ID] = make(chan struct{}, p.MaxConcurrent)
	}
	return g, nil
}
func (g *Gateway) Drain() { g.draining.Store(true) }
func (g *Gateway) Close() { g.client.CloseIdleConnections() }
func jsonReply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code string) {
	jsonReply(w, status, map[string]any{"error": map[string]string{"code": code, "message": code, "type": "apostille_local_error"}})
}
func (g *Gateway) auth(r *http.Request) (config.Project, bool) {
	raw := r.Header.Get("Authorization")
	if len(raw) < 39 || len(raw) > 256 || !strings.HasPrefix(raw, "Bearer ") {
		return config.Project{}, false
	}
	return g.projectByToken(strings.TrimPrefix(raw, "Bearer "))
}

// authMessages accepts the same project token from exactly one of the headers
// the Anthropic SDK may send. Presenting both is ambiguous and is rejected.
func (g *Gateway) authMessages(r *http.Request) (config.Project, bool) {
	keys, bearer := r.Header.Values("X-Api-Key"), r.Header.Values("Authorization")
	if len(keys) > 0 && len(bearer) > 0 {
		return config.Project{}, false
	}
	if len(keys) == 0 {
		return g.auth(r)
	}
	key := keys[0]
	if len(keys) != 1 || len(key) < 32 || len(key) > 249 {
		return config.Project{}, false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return config.Project{}, false
		}
	}
	return g.projectByToken(key)
}

func (g *Gateway) projectByToken(token string) (config.Project, bool) {
	h := sha256.Sum256([]byte(token))
	for _, p := range g.c.Projects {
		want, _ := hex.DecodeString(p.APIKeySHA256)
		if subtle.ConstantTimeCompare(h[:], want) == 1 {
			return p, true
		}
	}
	return config.Project{}, false
}
func permitted(p config.Project, id string) bool {
	for _, s := range p.Models {
		if s == id {
			return true
		}
	}
	return false
}
func (g *Gateway) ready(ctx context.Context) bool {
	if g.draining.Load() {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(g.c.RuntimeURL, "/")+"/v1/models", nil)
	resp, err := g.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(b) > 65536 {
		return false
	}
	var data struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(b, &data) != nil {
		return false
	}
	for _, m := range data.Data {
		if m.ID == g.c.ActiveModel {
			return true
		}
	}
	return false
}
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.RawQuery != "" {
		fail(w, 400, "query_not_supported")
		return
	}
	if r.URL.Path == "/healthz" && r.Method == "GET" {
		jsonReply(w, 200, map[string]string{"status": "alive"})
		return
	}
	if r.URL.Path == "/readyz" && r.Method == "GET" {
		if !g.ready(r.Context()) {
			fail(w, 503, "runtime_unavailable")
			return
		}
		jsonReply(w, 200, map[string]string{"status": "ready"})
		return
	}
	if r.URL.Path == "/v1/messages" {
		g.messages(w, r)
		return
	}
	p, ok := g.auth(r)
	if !ok {
		fail(w, 401, "unauthorized")
		return
	}
	switch {
	case r.URL.Path == "/v1/models" && r.Method == "GET":
		data := []any{}
		if permitted(p, g.c.ActiveModel) && g.ready(r.Context()) {
			data = append(data, map[string]any{"id": g.c.ActiveModel, "object": "model", "owned_by": "local", "created": 0})
		}
		jsonReply(w, 200, map[string]any{"object": "list", "data": data})
	case r.URL.Path == "/local/v1/capabilities" && r.Method == "GET":
		g.capabilities(w, r, p)
	case r.URL.Path == "/v1/chat/completions" && r.Method == "POST":
		g.chat(w, r, p)
	case strings.HasPrefix(r.URL.Path, "/local/v1/runs/") && strings.HasSuffix(r.URL.Path, "/evidence") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/local/v1/runs/"), "/evidence")
		if !core.ValidID(id) || g.store == nil {
			fail(w, 404, "not_found")
			return
		}
		rec, err := g.store.Get(p.ID, id)
		if err != nil {
			if errors.Is(err, evidence.ErrNotFound) {
				fail(w, 404, "not_found")
			} else {
				fail(w, 503, "evidence_unavailable")
			}
			return
		}
		if rec.Status == "ready" {
			bundle, err := core.Canonical(rec.Bundle)
			if err != nil {
				fail(w, 503, "evidence_unavailable")
				return
			}
			// JSON object re-serialization can change signed artifact bytes. Supply
			// the original bytes for SDK export without another JCS implementation.
			jsonReply(w, 200, map[string]any{
				"receipt_status": rec.Status, "manifest": rec.Manifest, "bundle": rec.Bundle,
				"manifest_base64": base64.StdEncoding.EncodeToString(rec.Manifest),
				"bundle_base64":   base64.StdEncoding.EncodeToString(bundle),
			})
		} else {
			jsonReply(w, 200, rec)
		}
	default:
		fail(w, 404, "not_found")
	}
}
func (g *Gateway) chat(w http.ResponseWriter, r *http.Request, p config.Project) {
	g.infer(w, r, p, chatDialect)
}

// infer is the single inference pipeline shared by every public API family.
// A dialect only parses its request, shapes errors and renders validated output.
func (g *Gateway) infer(w http.ResponseWriter, r *http.Request, p config.Project, d dialect) {
	// Bound writes as well as runtime reads, including clients that stop reading SSE.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Duration(g.c.TimeoutSeconds+2) * time.Second))
	if g.draining.Load() {
		d.fail(w, 503, "draining")
		return
	}
	if ct := strings.Split(r.Header.Get("Content-Type"), ";")[0]; ct != "application/json" {
		d.fail(w, 415, "json_required")
		return
	}
	// Admission covers body parsing and schema compilation as well as inference.
	// Rejected requests must not spend unbounded CPU before acquiring capacity.
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
	default:
		d.fail(w, 429, "capacity_exceeded")
		return
	}
	select {
	case g.projects[p.ID] <- struct{}{}:
		defer func() { <-g.projects[p.ID] }()
	default:
		d.fail(w, 429, "project_capacity_exceeded")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		d.fail(w, 413, "request_too_large")
		return
	}
	model := g.c.Active()
	request, compiled, err := d.parse(raw, model)
	if err != nil {
		d.fail(w, 400, err.Error())
		return
	}
	if !permitted(p, request.Model) {
		d.fail(w, 403, "model_forbidden")
		return
	}
	if request.Model != model.ID {
		d.fail(w, 409, "model_inactive")
		return
	}
	record := r.Header.Get("X-Apostille-Record")
	if record != "" && record != "metadata" {
		d.fail(w, 400, "invalid_record_mode")
		return
	}
	if record != "" && g.store == nil {
		d.fail(w, 503, "evidence_disabled")
		return
	}
	if g.draining.Load() {
		d.fail(w, 503, "draining")
		return
	}
	id, err := core.NewID()
	if err != nil {
		d.fail(w, 503, "id_unavailable")
		return
	}
	w.Header().Set("X-Apostille-Run-ID", id)
	start := time.Now().UTC()
	if record != "" {
		if err = g.store.Begin(p.ID, id); err != nil {
			d.fail(w, 503, "evidence_unavailable")
			return
		}
	}
	complete := false
	defer func() {
		if record != "" && !complete {
			_ = g.store.Fail(p.ID, id)
		}
	}()
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(g.c.TimeoutSeconds)*time.Second)
	defer cancel()
	body, _ := json.Marshal(request)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(g.c.RuntimeURL, "/")+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			d.fail(w, 504, "inference_timeout")
		} else {
			d.fail(w, 502, "runtime_unavailable")
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		d.fail(w, 502, "runtime_rejected")
		return
	}
	finish := func(reason string) {
		if record == "" {
			complete = true
			return
		}
		m := evidence.NewManifest(model)
		m.RunID = id
		m.GatewayVersion = Version
		m.StartedAt = start.Format(time.RFC3339Nano)
		m.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
		m.FinishReason = reason
		if g.store.Complete(p.ID, id, m) == nil {
			complete = true
		}
	}
	if request.Stream {
		g.stream(w, r.WithContext(ctx), resp.Body, request, compiled, finish, model.RuntimeProfile, d.sink(w, id, request))
		return
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(data) > maxResponse {
		d.fail(w, 502, "invalid_runtime_response")
		return
	}
	res, ok := validateCompletion(data, request, model.RuntimeProfile)
	if !ok || !validateOutput(compiled, res.content) {
		d.fail(w, 502, "invalid_runtime_response")
		return
	}
	out, ok := d.render(id, request, res)
	if !ok {
		d.fail(w, 502, "invalid_runtime_response")
		return
	}
	if ctx.Err() != nil {
		d.fail(w, 504, "inference_timeout")
		return
	}
	finish(res.reason)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	_, _ = w.Write(out)
}
func checkCompletion(data []byte, request Request) (string, string, bool) {
	_, content, reason, ok := checkCompletionForRuntime(data, request, "")
	return content, reason, ok
}

func checkCompletionForRuntime(data []byte, request Request, profile string) ([]byte, string, string, bool) {
	res, ok := validateCompletion(data, request, profile)
	return res.data, res.content, res.reason, ok
}

// completionResult is the one validated view of a non-streaming runtime reply.
type completionResult struct {
	data    []byte
	content string
	reason  string
	calls   []ToolCall
	usage   json.RawMessage
}

func validateCompletion(data []byte, request Request, profile string) (completionResult, bool) {
	var r struct {
		Model   string `json:"model"`
		Choices []struct {
			Index   int `json:"index"`
			Message struct {
				Role      string          `json:"role"`
				Content   *string         `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Error json.RawMessage `json:"error"`
		Usage json.RawMessage `json:"usage"`
	}
	if validJSON(data) != nil || !runtimeShape(data, false) || json.Unmarshal(data, &r) != nil || r.Error != nil || len(r.Choices) != 1 || r.Model != request.Model || !validUsage(r.Usage, false) {
		return completionResult{}, false
	}
	c := r.Choices[0]
	var calls []ToolCall
	if len(c.Message.ToolCalls) > 0 && !bytes.Equal(bytes.TrimSpace(c.Message.ToolCalls), []byte("null")) {
		// Validate exact keys before decoding: encoding/json is case-insensitive,
		// while the customer executes the original wire representation.
		if !callsShape(c.Message.ToolCalls) || json.Unmarshal(c.Message.ToolCalls, &calls) != nil {
			return completionResult{}, false
		}
	}
	reason, ok := normalizeRuntimeFinish(profile, request, calls, c.Finish)
	if c.Index != 0 || c.Message.Role != "assistant" || !ok || (c.Message.Content == nil && len(calls) == 0) {
		return completionResult{}, false
	}
	if reason != c.Finish {
		if data, ok = rewriteRuntimeFinish(data, reason); !ok {
			return completionResult{}, false
		}
	}
	content := ""
	if c.Message.Content != nil {
		content = *c.Message.Content
	}
	return completionResult{data: data, content: content, reason: reason, calls: calls, usage: r.Usage}, true
}

func validFinish(request Request, calls []ToolCall, reason string) bool {
	if !validateCalls(request, calls) {
		return false
	}
	if len(calls) > 0 {
		// A truncated call must never become an executable call or success receipt.
		return reason == "tool_calls"
	}
	return reason == "stop" || reason == "length"
}

func validUsage(raw json.RawMessage, required bool) bool {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return !required
	}
	var u struct {
		Prompt     *int64 `json:"prompt_tokens"`
		Completion *int64 `json:"completion_tokens"`
		Total      *int64 `json:"total_tokens"`
	}
	if json.Unmarshal(raw, &u) != nil || u.Prompt == nil || u.Completion == nil || u.Total == nil {
		return false
	}
	return *u.Prompt >= 0 && *u.Prompt <= 1<<31 && *u.Completion >= 0 && *u.Completion <= 1<<31 && *u.Total == *u.Prompt+*u.Completion
}
