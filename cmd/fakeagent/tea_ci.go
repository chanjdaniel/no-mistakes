package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// teaCIStub extends the tea stub with one Gitea Actions CI cycle, so an e2e
// test can drive the CI step's repair path on the real binary: the head a PR
// is created at fails its "build" job, and once a repair pushes a new head
// the PR reads as merged, which ends the CI step without a 30-second poll.
// The stub is stateful (statePath records the PR's branch, base and created
// head) and reads the branch's live head from remote, the bare repository the
// e2e's git URL rewrite stands in for the Gitea host. It also keeps the PR's
// title and body, because publishing a repair attests its head in the PR body
// (read back through the raw pull API, rewritten with pulls edit). It is on
// only when
// FAKEAGENT_TEA_CI_REMOTE is set; otherwise the stateless stub answers.
type teaCIStub struct {
	remote    string
	statePath string
	host      string
}

type teaCIState struct {
	Branch     string `json:"branch"`
	Base       string `json:"base"`
	Repo       string `json:"repo"`
	CreatedSHA string `json:"created_sha"`
	CreatedAt  string `json:"created_at"`
	Title      string `json:"title"`
	Body       string `json:"body"`
}

const (
	teaCIPRIndex     = 99
	teaCIFailingJob  = 11
	teaCIPassingJob  = 21
	teaCIFailingRun  = "1"
	teaCIRepairRun   = "2"
	teaCIJobName     = "build"
	teaCIFailingLogs = "FAIL: build: feature.txt is not repaired yet\n"
)

func teaCIStubFromEnv() (teaCIStub, bool) {
	remote := os.Getenv("FAKEAGENT_TEA_CI_REMOTE")
	if remote == "" {
		return teaCIStub{}, false
	}
	host := os.Getenv("FAKEAGENT_TEA_HOST")
	if host == "" {
		host = "gitea.example.com"
	}
	return teaCIStub{remote: remote, statePath: os.Getenv("FAKEAGENT_TEA_CI_STATE"), host: host}, true
}

// handle answers the tea calls the CI cycle needs and reports whether it
// answered; a call it leaves alone falls through to the stateless stub.
func (s teaCIStub) handle(args []string, stdout io.Writer) (int, bool) {
	switch {
	case len(args) >= 2 && args[0] == "pulls" && args[1] == "create":
		if err := s.recordCreate(args); err != nil {
			fmt.Fprintf(os.Stderr, "fakeagent tea ci: %v\n", err)
			return 1, true
		}
		return 0, false
	case len(args) >= 3 && args[0] == "pulls" && args[1] == "edit":
		return s.edit(args, stdout)
	case len(args) >= 1 && args[0] == "api" && strings.HasSuffix(args[len(args)-1], fmt.Sprintf("/pulls/%d", teaCIPRIndex)):
		return s.content(stdout)
	case len(args) >= 2 && args[0] == "pulls" && args[1] == "list":
		return s.list(stdout)
	case len(args) >= 2 && args[0] == "pulls" && isDigits(args[1]):
		return s.view(stdout)
	case len(args) >= 3 && args[0] == "actions" && args[1] == "runs" && args[2] == "list":
		return s.runs(stdout)
	case len(args) >= 4 && args[0] == "actions" && args[1] == "runs" && args[2] == "logs":
		if args[3] == teaCIFailingRun && argAfter(args, "--job") == fmt.Sprint(teaCIFailingJob) {
			fmt.Fprint(stdout, teaCIFailingLogs)
			return 0, true
		}
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: no failing job log for %v\n", args)
		return 1, true
	case len(args) >= 1 && args[0] == "api" && strings.HasSuffix(args[len(args)-1], "/jobs"):
		return s.jobs(args[len(args)-1], stdout)
	}
	return 0, false
}

func (s teaCIStub) recordCreate(args []string) error {
	branch := argAfter(args, "--head")
	sha, err := s.branchHead(branch)
	if err != nil {
		return err
	}
	state := teaCIState{
		Branch:     branch,
		Base:       argAfter(args, "--base"),
		Repo:       strings.TrimSuffix(argAfter(args, "--repo"), ".git"),
		CreatedSHA: sha,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		Title:      argAfter(args, "--title"),
		Body:       argAfter(args, "--description"),
	}
	return s.save(state)
}

func (s teaCIStub) save(state teaCIState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(s.statePath, data, 0o644)
}

// edit applies `tea pulls edit <idx> [--title t] --description d`.
func (s teaCIStub) edit(args []string, stdout io.Writer) (int, bool) {
	state, ok, err := s.state()
	if err != nil || !ok || args[2] != fmt.Sprint(teaCIPRIndex) {
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: no pull request to edit for %v (%v)\n", args, err)
		return 1, true
	}
	if title := argAfter(args, "--title"); title != "" {
		state.Title = title
	}
	state.Body = argAfter(args, "--description")
	if err := s.save(state); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: %v\n", err)
		return 1, true
	}
	fmt.Fprintln(stdout, "updated")
	return 0, true
}

// content answers the raw REST pull object, which carries number, not the
// formatted view's index.
func (s teaCIStub) content(stdout io.Writer) (int, bool) {
	state, ok, err := s.state()
	if err != nil || !ok {
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: no pull request content (%v)\n", err)
		return 1, true
	}
	return writeTeaCIJSON(stdout, map[string]any{"number": teaCIPRIndex, "title": state.Title, "body": state.Body})
}

func (s teaCIStub) state() (teaCIState, bool, error) {
	data, err := os.ReadFile(s.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return teaCIState{}, false, nil
	}
	if err != nil {
		return teaCIState{}, false, err
	}
	var state teaCIState
	if err := json.Unmarshal(data, &state); err != nil {
		return teaCIState{}, false, err
	}
	return state, true, nil
}

func (s teaCIStub) branchHead(branch string) (string, error) {
	out, err := exec.Command("git", "--git-dir", s.remote, "rev-parse", "refs/heads/"+branch).Output()
	if err != nil {
		return "", fmt.Errorf("resolve %s in %s: %w", branch, s.remote, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// current reads the PR state and the branch's live head. The PR is merged
// once its head has moved past the head it was created at.
func (s teaCIStub) current() (teaCIState, string, bool, error) {
	state, ok, err := s.state()
	if err != nil || !ok {
		return state, "", ok, err
	}
	head, err := s.branchHead(state.Branch)
	return state, head, true, err
}

func (s teaCIStub) list(stdout io.Writer) (int, bool) {
	state, head, ok, err := s.current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: %v\n", err)
		return 1, true
	}
	if !ok || head != state.CreatedSHA {
		fmt.Fprintln(stdout, "[]")
		return 0, true
	}
	return writeTeaCIJSON(stdout, []map[string]string{{
		"index": fmt.Sprint(teaCIPRIndex),
		"state": "open",
		"url":   s.prURL(state),
		"head":  state.Branch,
		"base":  state.Base,
	}})
}

func (s teaCIStub) view(stdout io.Writer) (int, bool) {
	state, head, ok, err := s.current()
	if err != nil || !ok {
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: no pull request to view (%v)\n", err)
		return 1, true
	}
	merged := head != state.CreatedSHA
	prState := "open"
	if merged {
		prState = "closed"
	}
	return writeTeaCIJSON(stdout, map[string]any{
		"index":     teaCIPRIndex,
		"state":     prState,
		"url":       s.prURL(state),
		"head":      state.Branch,
		"base":      state.Base,
		"headSha":   head,
		"hasMerged": merged,
	})
}

func (s teaCIStub) runs(stdout io.Writer) (int, bool) {
	state, head, ok, err := s.current()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: %v\n", err)
		return 1, true
	}
	runs := []map[string]string{}
	if ok {
		runs = append(runs, map[string]string{"id": teaCIFailingRun, "status": "completed", "branch": state.Branch, "event": "pull_request"})
		if head != state.CreatedSHA {
			runs = append(runs, map[string]string{"id": teaCIRepairRun, "status": "completed", "branch": state.Branch, "event": "pull_request"})
		}
	}
	return writeTeaCIJSON(stdout, runs)
}

func (s teaCIStub) jobs(endpoint string, stdout io.Writer) (int, bool) {
	state, head, ok, err := s.current()
	if err != nil || !ok {
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: no pull request for %s (%v)\n", endpoint, err)
		return 1, true
	}
	job := map[string]any{"name": teaCIJobName, "status": "completed", "completed_at": state.CreatedAt}
	switch {
	case strings.HasSuffix(endpoint, "/runs/"+teaCIFailingRun+"/jobs"):
		job["id"], job["conclusion"], job["head_sha"] = teaCIFailingJob, "failure", state.CreatedSHA
	case strings.HasSuffix(endpoint, "/runs/"+teaCIRepairRun+"/jobs") && head != state.CreatedSHA:
		job["id"], job["conclusion"], job["head_sha"] = teaCIPassingJob, "success", head
	default:
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: unknown run %s\n", endpoint)
		return 1, true
	}
	job["html_url"] = fmt.Sprintf("http://%s/%s/actions/runs/%v", s.host, state.Repo, job["id"])
	return writeTeaCIJSON(stdout, map[string]any{"jobs": []any{job}})
}

func (s teaCIStub) prURL(state teaCIState) string {
	return fmt.Sprintf("http://%s/%s/pulls/%d", s.host, state.Repo, teaCIPRIndex)
}

func writeTeaCIJSON(stdout io.Writer, v any) (int, bool) {
	if err := json.NewEncoder(stdout).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "fakeagent tea ci: %v\n", err)
		return 1, true
	}
	return 0, true
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
