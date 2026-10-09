package gateway

import (
	"net/http"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	"github.com/ifandonlyif-io/iff-apostille-local/internal/evidence"
)

// Capabilities describe this API contract and operator configuration. Readiness
// is not proof of hardware qualification or model quality. Do not expose paths,
// project names, backend endpoints, keys or models outside the caller's allowlist.
func (g *Gateway) capabilities(w http.ResponseWriter, r *http.Request, p config.Project) {
	models := []any{}
	m := g.c.Active()
	if permitted(p, m.ID) && g.ready(r.Context()) {
		models = append(models, map[string]any{
			"id": m.ID, "max_context": m.MaxContext, "max_output_tokens": m.MaxTokens,
			"max_concurrent_requests": min(m.MaxConcurrent, p.MaxConcurrent),
			"features": map[string]bool{"text": true, "streaming": true, "stream_usage": true,
				"json_schema": true, "tool_calling": m.ToolCallParser != ""},
		})
	}
	jsonReply(w, 200, map[string]any{
		"object": "apostille_local.capabilities", "contract_version": "1",
		"api_family": "chat_completions", "gateway_version": Version, "models": models,
		"evidence":       map[string]any{"metadata": g.store != nil, "retention_seconds": int(evidence.Retention.Seconds())},
		"tool_execution": "client", "unsupported": []string{"responses", "embeddings", "multimodal", "server_tool_execution"},
	})
}
