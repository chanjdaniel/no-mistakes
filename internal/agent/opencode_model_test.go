package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

// captureOpencodeMessageBody runs one opencode invocation against a local fake
// server and returns the raw POST /session/{id}/message body it sent.
func captureOpencodeMessageBody(t *testing.T, profile agentcfg.Profile, opts RunOpts) string {
	t.Helper()
	var mu sync.Mutex
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			data, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read message body: %v", err)
			}
			mu.Lock()
			body = data
			mu.Unlock()
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","structured":{}},"parts":[{"type":"text","text":"{}"}]}`)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:     "opencode",
		server:  &managedServer{port: mustParsePort(server.URL)},
		profile: profile,
	}
	if opts.CWD == "" {
		opts.CWD = t.TempDir()
	}
	if _, err := a.Run(context.Background(), opts); err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return string(body)
}

// With agent_config's model and effort and no per-invocation model, the body is
// exactly what upstream v1.84.0 sends.
func TestOpencodeMessageBodyWithoutPerInvocationModelIsUpstreams(t *testing.T) {
	got := captureOpencodeMessageBody(t,
		agentcfg.Profile{Model: "openai/gpt-5", Effort: agentcfg.EffortHigh},
		RunOpts{Prompt: "review this code"})
	want := `{"model":{"modelID":"gpt-5","providerID":"openai"},"parts":[{"text":"review this code","type":"text"}],"role":"user","variant":"high"}`
	if got != want {
		t.Fatalf("message body =\n%s\nwant\n%s", got, want)
	}
}

func TestOpencodeMessageBodyWithoutProfileOrPerInvocationModelPinsNothing(t *testing.T) {
	got := captureOpencodeMessageBody(t, agentcfg.Profile{}, RunOpts{Prompt: "lint"})
	want := `{"parts":[{"text":"lint","type":"text"}],"role":"user"}`
	if got != want {
		t.Fatalf("message body =\n%s\nwant\n%s", got, want)
	}
}

// A per-invocation model replaces the profile's model in the body; the
// profile's effort, and everything else, is unchanged.
func TestOpencodeMessageBodyPerInvocationModelReplacesTheProfileModel(t *testing.T) {
	got := captureOpencodeMessageBody(t,
		agentcfg.Profile{Model: "openai/gpt-5", Effort: agentcfg.EffortHigh},
		RunOpts{Prompt: "review this code", Model: "deepseek/deepseek-v4-pro"})
	want := `{"model":{"modelID":"deepseek-v4-pro","providerID":"deepseek"},"parts":[{"text":"review this code","type":"text"}],"role":"user","variant":"high"}`
	if got != want {
		t.Fatalf("message body =\n%s\nwant\n%s", got, want)
	}
}

func TestOpencodeMessageBodyPerInvocationModelWithoutProfile(t *testing.T) {
	got := captureOpencodeMessageBody(t, agentcfg.Profile{}, RunOpts{
		Prompt:     "test",
		Model:      "deepseek/deepseek-flash",
		JSONSchema: json.RawMessage(`{"type":"object"}`),
	})
	var body map[string]any
	if err := json.Unmarshal([]byte(got), &body); err != nil {
		t.Fatalf("decode body: %v\n%s", err, got)
	}
	model, _ := body["model"].(map[string]any)
	if model["providerID"] != "deepseek" || model["modelID"] != "deepseek-flash" {
		t.Fatalf("body model = %#v, want deepseek/deepseek-flash", body["model"])
	}
	if _, ok := body["variant"]; ok {
		t.Fatalf("body pinned a variant with no effort configured: %s", got)
	}
	info, _ := body["info"].(map[string]any)
	if _, ok := info["format"]; !ok {
		t.Fatalf("body lost the structured-output format: %s", got)
	}
}
