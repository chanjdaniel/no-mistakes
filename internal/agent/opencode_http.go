package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func (a *opencodeAgent) ensureServer(ctx context.Context, cwd string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.server != nil {
		return a.server.baseURL(), nil
	}
	port, err := getAvailablePort()
	if err != nil {
		return "", fmt.Errorf("opencode port: %w", err)
	}
	args := buildOpencodeServeArgs(a.extraArgs, port)
	srv, err := startServerWithPort(ctx, "opencode", a.bin, args, cwd, "/global/health", port)
	if err != nil {
		return "", fmt.Errorf("opencode server: %w", err)
	}
	a.server = srv
	return srv.baseURL(), nil
}

// buildOpencodeServeArgs builds `opencode serve`'s argv with user-supplied
// extras inserted after the "serve" subcommand and before the managed flags.
func buildOpencodeServeArgs(extraArgs []string, port int) []string {
	args := make([]string, 0, len(extraArgs)+6)
	args = append(args, "serve")
	args = append(args, extraArgs...)
	args = append(args, "--hostname", "127.0.0.1", "--port", fmt.Sprintf("%d", port), "--print-logs")
	return args
}

func (a *opencodeAgent) createSession(ctx context.Context, baseURL, cwd string) (string, error) {
	body := map[string]any{
		"directory": cwd,
		"permission": []map[string]string{
			{"permission": "*", "pattern": "*", "action": "allow"},
		},
	}
	resp, err := doJSON(ctx, http.MethodPost, baseURL+"/session", nil, body)
	if err != nil {
		return "", fmt.Errorf("opencode create session: %w", err)
	}

	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(resp, &result); err != nil {
		return "", fmt.Errorf("opencode create session parse: %w", err)
	}
	return result.ID, nil
}

func (a *opencodeAgent) connectEventStream(ctx context.Context, baseURL string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/global/event", nil)
	if err != nil {
		return nil, fmt.Errorf("opencode event stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode event stream: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("opencode event stream failed with %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

// parseModelString splits a "provider/model" string into providerID and
// modelID. An empty string is valid (no override). A string without a "/"
// is rejected because both provider and model must be specified per
// opencode's API schema.
func parseModelString(model string) (providerID, modelID string, err error) {
	if model == "" {
		return "", "", nil
	}
	idx := strings.Index(model, "/")
	if idx < 0 {
		return "", "", fmt.Errorf("model must be in 'provider/model' format, got %q", model)
	}
	providerID = model[:idx]
	modelID = model[idx+1:]
	if providerID == "" || modelID == "" {
		return "", "", fmt.Errorf("model must be in 'provider/model' format with both parts non-empty, got %q", model)
	}
	return providerID, modelID, nil
}

func (a *opencodeAgent) sendMessage(ctx context.Context, baseURL, sessionID, prompt string, schema json.RawMessage, model string) (*opencodeMessageResponse, error) {
	body := map[string]any{
		"parts": []map[string]string{{"type": "text", "text": prompt}},
	}

	// model at the root: opencode expects {providerID, modelID}, not a
	// plain string. The lookupAgentModel helpers pass the full
	// "provider/model" string; parse it into the object opencode expects.
	if model != "" {
		providerID, modelID, err := parseModelString(model)
		if err != nil {
			return nil, err
		}
		body["model"] = map[string]string{
			"providerID": providerID,
			"modelID":    modelID,
		}
	}

	// Structured output (json_schema) is handled via the prompt text
	// (buildOpencodePrompt appends schema instructions). We do not send
	// a root-level "format" key here because it enables server-side
	// structured-output enforcement (tool_choice), which the DeepSeek
	// API rejects with "Thinking mode does not support this tool_choice".
	// Prompt-based schema instructions work across all providers.

	respBytes, err := doJSON(ctx, http.MethodPost, baseURL+"/session/"+sessionID+"/message", nil, body)
	if err != nil {
		return nil, err
	}

	var resp opencodeMessageResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("opencode message parse: %w", err)
	}
	return &resp, nil
}

func (a *opencodeAgent) abortSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	doJSON(ctx, http.MethodPost, baseURL+"/session/"+sessionID+"/abort", nil, nil)
}

func (a *opencodeAgent) deleteSession(baseURL, sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, baseURL+"/session/"+sessionID, nil)
	if req != nil {
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil {
			resp.Body.Close()
		}
	}
}

// buildOpencodePrompt appends schema instructions to the prompt.
func buildOpencodePrompt(prompt string, schema json.RawMessage) string {
	return strings.Join([]string{
		prompt,
		"",
		"When you finish, reply with only valid JSON.",
		"Do not wrap the JSON in markdown fences.",
		"Do not include any prose before or after the JSON.",
		"The JSON must match this schema exactly: " + string(schema),
	}, "\n")
}
