package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ifandonlyif-io/iff-apostille-local/internal/config"
	schema "github.com/santhosh-tekuri/jsonschema/v6"
)

// dialect holds the only endpoint-specific parts of the inference pipeline.
type dialect struct {
	parse  func([]byte, config.Model) (Request, *schema.Schema, error)
	fail   func(http.ResponseWriter, int, string)
	render func(id string, request Request, res completionResult) ([]byte, bool)
	sink   func(w http.ResponseWriter, id string, request Request) streamSink
}

var chatDialect = dialect{
	parse:  parseRequest,
	fail:   fail,
	render: func(_ string, _ Request, res completionResult) ([]byte, bool) { return res.data, true },
	sink:   func(w http.ResponseWriter, _ string, _ Request) streamSink { return &chatSink{w: w} },
}

type chatSink struct{ w http.ResponseWriter }

func (s *chatSink) emit(data string) bool {
	_, e := fmt.Fprintf(s.w, "data: %s\n\n", strings.ReplaceAll(data, "\n", "\ndata: "))
	s.w.(http.Flusher).Flush()
	return e == nil
}
func (s *chatSink) start() bool                               { return true }
func (s *chatSink) chunk(data, _ string) bool                 { return s.emit(data) }
func (s *chatSink) usage(data string, _ json.RawMessage) bool { return s.emit(data) }
func (s *chatSink) ready(string, json.RawMessage) bool        { return true }
func (s *chatSink) commit(string, []ToolCall, json.RawMessage) bool {
	return s.emit("[DONE]")
}
func (s *chatSink) fail() {
	s.emit(`{"error":{"code":"invalid_runtime_stream","message":"invalid_runtime_stream","type":"apostille_local_error"}}`)
}
