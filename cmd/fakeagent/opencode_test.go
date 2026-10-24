package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeOpencodeServerUnsubscribeLeavesCopiedSubscriberSafe(t *testing.T) {
	srv := newFakeOpencodeServer(defaultScenario())
	ch := make(chan []byte, 1)
	srv.subscribe(ch)

	srv.mu.Lock()
	subs := append([]chan []byte(nil), srv.subscribers...)
	srv.mu.Unlock()

	srv.unsubscribe(ch)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("send to copied subscriber panicked after unsubscribe: %v", r)
		}
	}()

	subs[0] <- []byte("data: {}\n\n")
}

func TestFakeOpencodeServerConfiguredFixtureLoadFailureIsNotSilent(t *testing.T) {
	t.Setenv("FAKEAGENT_FIXTURE", t.TempDir())
	fixtureDir := filepath.Join(os.Getenv("FAKEAGENT_FIXTURE"), "opencode", "structured")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatalf("mkdir fixture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "session.json"), []byte(`{"id":"sess-123"}`), 0o644); err != nil {
		t.Fatalf("write session fixture: %v", err)
	}

	srv := newFakeOpencodeServer(defaultScenario())
	req := httptest.NewRequest(http.MethodGet, "/global/health", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestPatchOpencodeMessageRequiresRecordedInfo(t *testing.T) {
	t.Helper()

	_, err := patchOpencodeMessage([]byte(`{"id":"msg-123"}`), Action{
		Structured: map[string]any{"summary": "ok"},
	}, fakeOpencodeDefaultModel)
	if err == nil {
		t.Fatal("expected malformed recorded message to fail")
	}
	if !containsAll(err.Error(), []string{"message", "info"}) {
		t.Fatalf("error = %q, want mention of missing info", err)
	}
}

func containsAll(s string, want []string) bool {
	for _, part := range want {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}

func TestPatchOpencodeMessagePreservesRecordedInfo(t *testing.T) {
	t.Helper()

	raw := []byte(`{"info":{"id":"msg-123","role":"assistant"}}`)
	patched, err := patchOpencodeMessage(raw, Action{Structured: map[string]any{"summary": "ok"}}, fakeOpencodeDefaultModel)
	if err != nil {
		t.Fatalf("patchOpencodeMessage: %v", err)
	}
	var resp struct {
		Info struct {
			ID         string          `json:"id"`
			Role       string          `json:"role"`
			Structured json.RawMessage `json:"structured"`
		} `json:"info"`
	}
	if err := json.Unmarshal(patched, &resp); err != nil {
		t.Fatalf("unmarshal patched response: %v", err)
	}
	if resp.Info.ID != "msg-123" || resp.Info.Role != "assistant" {
		t.Fatalf("patched info = %+v, want recorded id and role", resp.Info)
	}
	if string(resp.Info.Structured) != `{"summary":"ok"}` {
		t.Fatalf("structured = %s, want patched payload", resp.Info.Structured)
	}
}

func TestPatchOpencodeMessagePreservesStructuredRaw(t *testing.T) {
	t.Helper()

	raw := []byte(`{"info":{"id":"msg-123","role":"assistant"}}`)
	patched, err := patchOpencodeMessage(raw, Action{StructuredRaw: `"not an object"`}, fakeOpencodeDefaultModel)
	if err != nil {
		t.Fatalf("patchOpencodeMessage: %v", err)
	}
	var resp struct {
		Info struct {
			Structured json.RawMessage `json:"structured"`
		} `json:"info"`
	}
	if err := json.Unmarshal(patched, &resp); err != nil {
		t.Fatalf("unmarshal patched response: %v", err)
	}
	if string(resp.Info.Structured) != `"not an object"` {
		t.Fatalf("structured = %s, want raw payload", resp.Info.Structured)
	}
}

func TestFakeOpencodeServerMessageIncludesStructuredRaw(t *testing.T) {
	t.Helper()

	srv := newFakeOpencodeServer(&Scenario{Actions: []Action{{
		Match:         "raw",
		Text:          "scenario text",
		StructuredRaw: `"not an object"`,
	}}})

	createReq := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(fmt.Sprintf(`{"directory":%q}`, t.TempDir())))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status = %d, want %d", createRec.Code, http.StatusOK)
	}

	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &session); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}

	msgReq := httptest.NewRequest(http.MethodPost, "/session/"+session.ID+"/message", strings.NewReader(`{"parts":[{"type":"text","text":"raw prompt"}]}`))
	msgReq.Header.Set("Content-Type", "application/json")
	msgRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(msgRec, msgReq)
	if msgRec.Code != http.StatusOK {
		t.Fatalf("message status = %d, want %d", msgRec.Code, http.StatusOK)
	}

	var resp struct {
		Info struct {
			Structured json.RawMessage `json:"structured"`
		} `json:"info"`
	}
	if err := json.Unmarshal(msgRec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if string(resp.Info.Structured) != `"not an object"` {
		t.Fatalf("structured = %s, want raw payload", resp.Info.Structured)
	}
}

func TestOpencodeFixtureRewritesSessionIDsPerRequest(t *testing.T) {
	t.Helper()

	fixture := &opencodeFixture{
		sessionID: "ses_recorded",
		session:   []byte(`{"id":"ses_recorded","slug":"recorded"}`),
		sse:       []byte("data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"ses_recorded\",\"info\":{\"sessionID\":\"ses_recorded\"}}}}\n\ndata: {\"payload\":{\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_recorded\"}}}\n\n"),
		message:   []byte(`{"info":{"id":"msg-123","role":"assistant","sessionID":"ses_recorded"},"parts":[{"type":"text","sessionID":"ses_recorded","messageID":"msg-123"}]}`),
	}

	firstSession, err := rewriteOpencodeFixtureSession(fixture, "ses_first")
	if err != nil {
		t.Fatalf("rewrite session: %v", err)
	}
	secondSession, err := rewriteOpencodeFixtureSession(fixture, "ses_second")
	if err != nil {
		t.Fatalf("rewrite session again: %v", err)
	}
	if bytes.Equal(firstSession, secondSession) {
		t.Fatal("rewritten sessions should differ per request")
	}
	if bytes.Contains(firstSession, []byte("ses_recorded")) || bytes.Contains(secondSession, []byte("ses_recorded")) {
		t.Fatal("rewritten session payload should not keep recorded session ID")
	}

	rewrittenSSE, err := rewriteOpencodeFixtureSSE(fixture, Action{Structured: map[string]any{"summary": "ok"}}, "ses_first", fakeOpencodeDefaultModel)
	if err != nil {
		t.Fatalf("rewrite sse: %v", err)
	}
	if !bytes.Contains(rewrittenSSE, []byte("ses_first")) {
		t.Fatalf("rewritten sse = %s, want new session ID", rewrittenSSE)
	}
	if bytes.Contains(rewrittenSSE, []byte("ses_recorded")) {
		t.Fatalf("rewritten sse = %s, want recorded session ID removed", rewrittenSSE)
	}

	rewrittenMessage, err := rewriteOpencodeFixtureMessage(fixture, Action{Structured: map[string]any{"summary": "ok"}}, "ses_first", fakeOpencodeDefaultModel)
	if err != nil {
		t.Fatalf("rewrite message: %v", err)
	}
	if !bytes.Contains(rewrittenMessage, []byte("ses_first")) {
		t.Fatalf("rewritten message = %s, want new session ID", rewrittenMessage)
	}
	if bytes.Contains(rewrittenMessage, []byte("ses_recorded")) {
		t.Fatalf("rewritten message = %s, want recorded session ID removed", rewrittenMessage)
	}
}

func TestFakeOpencodeFixturePlainRunRewritesRecordedText(t *testing.T) {
	t.Helper()

	srv := newFakeOpencodeServer(&Scenario{Actions: []Action{{
		Match: "plain",
		Text:  "scenario text",
	}}})
	srv.fixture = &opencodeFixture{
		sessionID: "ses_recorded",
		session:   []byte(`{"id":"ses_recorded"}`),
		sse: []byte(strings.Join([]string{
			`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"ses_recorded","part":{"id":"p1","messageID":"msg-123","sessionID":"ses_recorded","type":"text","text":"recorded text","metadata":{"openai":{"phase":"final_answer"}}}}}}`,
			"",
			`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_recorded","partID":"p1","field":"text","delta":"recorded text"}}}`,
			"",
			`data: {"payload":{"type":"session.idle","properties":{"sessionID":"ses_recorded"}}}`,
			"",
		}, "\n")),
		message: []byte(`{"info":{"id":"msg-123","role":"assistant","sessionID":"ses_recorded"},"parts":[{"type":"text","text":"recorded text","metadata":{"openai":{"phase":"final_answer"}}}]}`),
	}

	createReq := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(fmt.Sprintf(`{"directory":%q}`, t.TempDir())))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status = %d, want %d", createRec.Code, http.StatusOK)
	}

	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &session); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}

	ch := make(chan []byte, 1)
	srv.subscribe(ch)
	defer srv.unsubscribe(ch)

	msgReq := httptest.NewRequest(http.MethodPost, "/session/"+session.ID+"/message", strings.NewReader(`{"parts":[{"type":"text","text":"plain prompt"}]}`))
	msgReq.Header.Set("Content-Type", "application/json")
	msgRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(msgRec, msgReq)
	if msgRec.Code != http.StatusOK {
		t.Fatalf("message status = %d, want %d", msgRec.Code, http.StatusOK)
	}

	broadcast := <-ch
	if !bytes.Contains(broadcast, []byte("scenario text")) {
		t.Fatalf("broadcast = %s, want scenario text", broadcast)
	}
	if bytes.Contains(broadcast, []byte("recorded text")) {
		t.Fatalf("broadcast = %s, want recorded text removed", broadcast)
	}
	if !bytes.Contains(msgRec.Body.Bytes(), []byte("scenario text")) {
		t.Fatalf("message = %s, want scenario text", msgRec.Body.Bytes())
	}
	if bytes.Contains(msgRec.Body.Bytes(), []byte("recorded text")) {
		t.Fatalf("message = %s, want recorded text removed", msgRec.Body.Bytes())
	}
}

func TestFakeOpencodeServerAppliesEditsInSessionDirectory(t *testing.T) {
	t.Helper()

	wd := t.TempDir()
	dir := filepath.Join(wd, "session-dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	t.Chdir(wd)

	srv := newFakeOpencodeServer(&Scenario{Actions: []Action{{
		Match: "fix",
		Edits: []Edit{{Path: filepath.Join("nested", "note.txt"), New: "hello\n"}},
	}}})

	createReq := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(fmt.Sprintf(`{"directory":%q}`, dir)))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status = %d, want %d", createRec.Code, http.StatusOK)
	}

	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &session); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if session.ID == "" {
		t.Fatal("expected session id")
	}

	msgReq := httptest.NewRequest(http.MethodPost, "/session/"+session.ID+"/message", strings.NewReader(`{"parts":[{"type":"text","text":"please fix this"}]}`))
	msgReq.Header.Set("Content-Type", "application/json")
	msgRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(msgRec, msgReq)
	if msgRec.Code != http.StatusOK {
		t.Fatalf("message status = %d, want %d", msgRec.Code, http.StatusOK)
	}

	if _, err := os.Stat(filepath.Join(dir, "nested", "note.txt")); err != nil {
		t.Fatalf("expected edit in session directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wd, "nested", "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("working directory edit err = %v, want not exist", err)
	}
}

// The fake logs each message body's model and variant, so e2e tests can assert
// per-invocation routing that never appears in opencode's argv.
func TestFakeOpencodeServerLogsTheRequestedModelAndVariant(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "fakeagent.log")
	t.Setenv("FAKEAGENT_LOG", logPath)
	srv := newFakeOpencodeServer(&Scenario{})

	post := func(path, body string) []byte {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("POST %s status = %d: %s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(post("/session", fmt.Sprintf(`{"directory":%q}`, t.TempDir())), &session); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	post("/session/"+session.ID+"/message", `{"parts":[{"type":"text","text":"routed"}],"model":{"providerID":"deepseek","modelID":"deepseek-flash"},"variant":"high"}`)
	post("/session/"+session.ID+"/message", `{"parts":[{"type":"text","text":"default"}]}`)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("log lines = %d, want 2:\n%s", len(lines), data)
	}
	var routed, unrouted invocation
	if err := json.Unmarshal(lines[0], &routed); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(lines[1], &unrouted); err != nil {
		t.Fatal(err)
	}
	if routed.Prompt != "routed" || routed.Model != "deepseek/deepseek-flash" || routed.Variant != "high" {
		t.Fatalf("routed invocation = %+v", routed)
	}
	if unrouted.Model != "" || unrouted.Variant != "" {
		t.Fatalf("unrouted invocation logged a model: %+v", unrouted)
	}
}

// TestFakeOpencodeServerServesTheRequestedModel proves the fake answers the
// way opencode does: every assistant message, in the stream and in the
// response, carries the model the body asked for (or the fixed default when it
// asked for none) and reasoning tokens - in both synthetic and recorded-fixture
// mode.
func TestFakeOpencodeServerServesTheRequestedModel(t *testing.T) {
	recorded, err := loadOpencodeFixture(filepath.Join("..", "..", "internal", "e2e", "fixtures", "opencode"), "structured")
	if err != nil {
		t.Fatalf("load recorded fixture: %v", err)
	}
	for _, mode := range []struct {
		name    string
		fixture *opencodeFixture
	}{{"synthetic", nil}, {"fixture", recorded}} {
		for _, tc := range []struct {
			name, body, provider, model string
		}{
			{"routed", `{"parts":[{"type":"text","text":"routed"}],"model":{"providerID":"deepseek","modelID":"deepseek-flash"}}`, "deepseek", "deepseek-flash"},
			{"default", `{"parts":[{"type":"text","text":"default"}]}`, "openai", "gpt-5.4"},
		} {
			t.Run(mode.name+"/"+tc.name, func(t *testing.T) {
				t.Setenv("FAKEAGENT_LOG", filepath.Join(t.TempDir(), "fakeagent.log"))
				srv := newFakeOpencodeServer(&Scenario{})
				srv.fixture = mode.fixture
				session := postFakeOpencode(t, srv, "/session", fmt.Sprintf(`{"directory":%q}`, t.TempDir()))
				var created struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(session, &created); err != nil {
					t.Fatalf("unmarshal session: %v", err)
				}

				ch := make(chan []byte, 32)
				srv.subscribe(ch)
				defer srv.unsubscribe(ch)
				resp := postFakeOpencode(t, srv, "/session/"+created.ID+"/message", tc.body)

				var msg struct {
					Info servedInfo `json:"info"`
				}
				if err := json.Unmarshal(resp, &msg); err != nil {
					t.Fatalf("unmarshal message: %v", err)
				}
				msg.Info.check(t, "response", tc.provider, tc.model)

				assistants := 0
				for len(ch) > 0 {
					for _, info := range assistantInfos(t, <-ch) {
						info.check(t, "stream", tc.provider, tc.model)
						assistants++
					}
				}
				if assistants == 0 {
					t.Fatal("stream carried no assistant message.updated event")
				}
			})
		}
	}
}

type servedInfo struct {
	Role       string `json:"role"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Tokens     *struct {
		Reasoning *int `json:"reasoning"`
	} `json:"tokens"`
}

func (i servedInfo) check(t *testing.T, where, provider, model string) {
	t.Helper()
	if i.ProviderID != provider || i.ModelID != model {
		t.Errorf("%s served %s/%s, want %s/%s", where, i.ProviderID, i.ModelID, provider, model)
	}
	if i.Tokens == nil || i.Tokens.Reasoning == nil {
		t.Errorf("%s reported no reasoning tokens", where)
	}
}

// assistantInfos reads the assistant message.updated infos out of SSE-framed
// bytes.
func assistantInfos(t *testing.T, framed []byte) []servedInfo {
	t.Helper()
	var infos []servedInfo
	for _, line := range bytes.Split(framed, []byte("\n")) {
		data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
		if !ok {
			continue
		}
		var event struct {
			Payload struct {
				Type       string `json:"type"`
				Properties struct {
					Info *servedInfo `json:"info"`
				} `json:"properties"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil {
			t.Fatalf("parse sse event %s: %v", data, err)
		}
		if event.Payload.Type == "message.updated" && event.Payload.Properties.Info != nil && event.Payload.Properties.Info.Role == "assistant" {
			infos = append(infos, *event.Payload.Properties.Info)
		}
	}
	return infos
}

func postFakeOpencode(t *testing.T, srv *fakeOpencodeServer, path, body string) []byte {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s status = %d: %s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}
