package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// opencodeServedServer is a one-session opencode that streams sse and answers
// the message request with message.
func opencodeServedServer(t *testing.T, sse, message string) *opencodeAgent {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, sse)
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, message)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	return &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}
}

const opencodeIdleEvent = "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n"

var opencodeSummarySchema = json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)

func TestOpencodeAgent_RecordsTheServedModelAndReasoningTokens(t *testing.T) {
	a := opencodeServedServer(t, opencodeIdleEvent,
		`{"info":{"id":"msg1","role":"assistant","modelID":"deepseek-flash","providerID":"deepseek","structured":{"summary":"ok"},"tokens":{"input":18797,"output":2,"reasoning":7,"cache":{"read":0,"write":0}}},"parts":[]}`)

	result, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir(), JSONSchema: opencodeSummarySchema})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Model != "deepseek-flash" || result.ModelProvider != "deepseek" {
		t.Errorf("served = %q/%q, want deepseek/deepseek-flash", result.ModelProvider, result.Model)
	}
	if !result.Usage.ReasoningReported || result.Usage.ReasoningTokens != 7 {
		t.Errorf("reasoning = %d (reported %v), want 7 reported", result.Usage.ReasoningTokens, result.Usage.ReasoningReported)
	}
	if result.Usage.OutputTokens != 2 {
		t.Errorf("output tokens = %d, want opencode's own 2 (reasoning is not folded in)", result.Usage.OutputTokens)
	}
}

// A request with no per-step model is still served by some model, and that
// served model is what gets recorded.
func TestOpencodeAgent_RecordsTheServedModelForAnUnroutedTextTurn(t *testing.T) {
	a := opencodeServedServer(t, opencodeIdleEvent,
		`{"info":{"id":"msg1","role":"assistant","modelID":"deepseek-v4-pro","providerID":"deepseek","tokens":{"input":10,"output":3,"reasoning":0}},"parts":[{"type":"text","text":"done"}]}`)

	result, err := a.Run(context.Background(), RunOpts{Prompt: "fix it", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Model != "deepseek-v4-pro" || result.ModelProvider != "deepseek" {
		t.Errorf("served = %q/%q, want deepseek/deepseek-v4-pro", result.ModelProvider, result.Model)
	}
	if !result.Usage.ReasoningReported || result.Usage.ReasoningTokens != 0 {
		t.Errorf("reasoning = %d (reported %v), want a reported 0", result.Usage.ReasoningTokens, result.Usage.ReasoningReported)
	}
}

func TestOpencodeAgent_ResponseWithoutServedFieldsLeavesThemEmpty(t *testing.T) {
	a := opencodeServedServer(t, opencodeIdleEvent,
		`{"info":{"id":"msg1","role":"assistant","structured":{"summary":"ok"},"tokens":{"input":100,"output":50}},"parts":[]}`)

	result, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir(), JSONSchema: opencodeSummarySchema})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Model != "" || result.ModelProvider != "" {
		t.Errorf("served = %q/%q, want both empty", result.ModelProvider, result.Model)
	}
	if result.Usage.ReasoningReported || result.Usage.ReasoningTokens != 0 {
		t.Errorf("reasoning = %d (reported %v), want unreported", result.Usage.ReasoningTokens, result.Usage.ReasoningReported)
	}
	if !result.UsageReported || result.Usage.InputTokens != 100 {
		t.Errorf("usage = %+v, want the reported input and output kept", result.Usage)
	}
}

// The stream carries the assistant message's identity too, so a response that
// omits it does not lose what the stream reported.
func TestOpencodeAgent_ServedModelFromTheStreamWhenTheResponseOmitsIt(t *testing.T) {
	sse := "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\",\"modelID\":\"gpt-5.4\",\"providerID\":\"openai\",\"tokens\":{\"input\":40,\"output\":4,\"reasoning\":8}}}}}\n\n" + opencodeIdleEvent
	a := opencodeServedServer(t, sse, `{"info":{"id":"msg1","role":"assistant","structured":{"summary":"ok"}},"parts":[]}`)

	result, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir(), JSONSchema: opencodeSummarySchema})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Model != "gpt-5.4" || result.ModelProvider != "openai" {
		t.Errorf("served = %q/%q, want openai/gpt-5.4 from the stream", result.ModelProvider, result.Model)
	}
	if !result.Usage.ReasoningReported || result.Usage.ReasoningTokens != 8 {
		t.Errorf("reasoning = %d (reported %v), want 8 reported", result.Usage.ReasoningTokens, result.Usage.ReasoningReported)
	}
}

// A turn that failed on info.error still ran on a model and still spent its
// tokens, so the result it returns beside the error carries both.
func TestOpencodeAgent_FailedTurnStillRecordsTheServedModelAndReasoning(t *testing.T) {
	a := opencodeServedServer(t, opencodeIdleEvent,
		`{"info":{"id":"msg1","role":"assistant","modelID":"deepseek-flash","providerID":"deepseek","tokens":{"input":500,"output":20,"reasoning":12},"error":{"name":"StructuredOutputError","data":{"message":"Model did not produce structured output","retries":2}}},"parts":[]}`)

	result, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir(), JSONSchema: opencodeSummarySchema})
	if err == nil {
		t.Fatalf("Run succeeded with %+v, want the structured-output failure", result)
	}
	if result == nil {
		t.Fatalf("failed turn returned no result beside %v", err)
	}
	if result.Model != "deepseek-flash" || result.ModelProvider != "deepseek" {
		t.Errorf("served = %q/%q, want deepseek/deepseek-flash", result.ModelProvider, result.Model)
	}
	if !result.Usage.ReasoningReported || result.Usage.ReasoningTokens != 12 {
		t.Errorf("reasoning = %d (reported %v), want 12 reported", result.Usage.ReasoningTokens, result.Usage.ReasoningReported)
	}
}

// A failed turn whose only record is the stream (no token usage at all) still
// says which model it ran on.
func TestOpencodeAgent_FailedTurnWithoutUsageStillRecordsTheServedModel(t *testing.T) {
	sse := "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\",\"modelID\":\"deepseek-flash\",\"providerID\":\"deepseek\"}}}}\n\n" + opencodeIdleEvent
	a := opencodeServedServer(t, sse, `{"info":{"id":"msg1","role":"assistant","error":{"name":"ProviderAuthError","data":{"message":"invalid api key"}}},"parts":[]}`)

	result, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir()})
	if err == nil {
		t.Fatalf("Run succeeded with %+v, want the provider failure", result)
	}
	if result == nil || result.Model != "deepseek-flash" || result.ModelProvider != "deepseek" {
		t.Fatalf("result = %+v, want the served deepseek/deepseek-flash", result)
	}
	if result.UsageReported || result.Usage.ReasoningReported {
		t.Errorf("usage = %+v (reported %v), want nothing reported", result.Usage, result.UsageReported)
	}
}

// The prompt-only fallback is a second turn: the row reports the model that
// served it, and both turns' reasoning.
func TestOpencodeAgent_FallbackRecordsTheFallbackTurnsModelAndBothTurnsReasoning(t *testing.T) {
	var sessions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprintf(w, `{"id":"s%d"}`, sessions.Add(1))
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			fmt.Fprint(w, opencodeIdleEvent)
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","modelID":"native-model","providerID":"deepseek","tokens":{"input":100,"output":10,"reasoning":5},"error":{"name":"APIError","data":{"message":"Provider returned error","responseBody":"Thinking may not be enabled when tool_choice forces tool use."}}}}`)
		case r.URL.Path == "/session/s2/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg2","role":"assistant","modelID":"deepseek-flash","providerID":"deepseek","tokens":{"input":200,"output":20,"reasoning":9}},"parts":[{"type":"text","text":"{\"summary\":\"all good\"}"}]}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}
	result, err := a.Run(context.Background(), RunOpts{Prompt: "review", CWD: t.TempDir(), JSONSchema: opencodeSummarySchema})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Model != "deepseek-flash" || result.ModelProvider != "deepseek" {
		t.Errorf("served = %q/%q, want the fallback turn's deepseek/deepseek-flash", result.ModelProvider, result.Model)
	}
	if !result.Usage.ReasoningReported || result.Usage.ReasoningTokens != 14 {
		t.Errorf("reasoning = %d (reported %v), want both turns' 14", result.Usage.ReasoningTokens, result.Usage.ReasoningReported)
	}
}

func TestOpencodeTokensToUsage_Reasoning(t *testing.T) {
	var tokens opencodeTokens
	if err := json.Unmarshal([]byte(`{"input":1,"output":2,"reasoning":3}`), &tokens); err != nil {
		t.Fatal(err)
	}
	if u := opencodeTokensToUsage(&tokens); !u.ReasoningReported || u.ReasoningTokens != 3 {
		t.Errorf("usage = %+v, want 3 reasoning tokens reported", u)
	}
	tokens = opencodeTokens{}
	if err := json.Unmarshal([]byte(`{"input":1,"output":2}`), &tokens); err != nil {
		t.Fatal(err)
	}
	if u := opencodeTokensToUsage(&tokens); u.ReasoningReported {
		t.Errorf("usage = %+v, want reasoning unreported when opencode omits it", u)
	}
}
