package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func teaCIGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func teaCICall(t *testing.T, stub teaCIStub, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	code, handled := stub.handle(args, &out)
	if !handled || code != 0 {
		t.Fatalf("tea %v: handled=%v code=%d", args, handled, code)
	}
	return out.String()
}

// The stub's CI cycle as the Gitea host reads it: the created head fails its
// build job with a log, and a pushed repair head passes and reads as merged.
func TestTeaCIStubFailsTheCreatedHeadAndMergesTheRepair(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	teaCIGit(t, root, "init", "--bare", remote)
	teaCIGit(t, root, "init", "-b", "feature", work)
	teaCIGit(t, work, "commit", "--allow-empty", "-m", "created")
	teaCIGit(t, work, "push", remote, "feature")
	created := teaCIGit(t, work, "rev-parse", "HEAD")
	stub := teaCIStub{remote: remote, statePath: filepath.Join(root, "state.json"), host: "gitea.example.com"}

	if got := teaCICall(t, stub, "pulls", "list"); strings.TrimSpace(got) != "[]" {
		t.Fatalf("list before create = %q, want []", got)
	}
	var out bytes.Buffer
	if code, handled := stub.handle([]string{"pulls", "create", "--repo", "owner/repo", "--head", "feature", "--base", "main", "--title", "t", "--description", "body"}, &out); handled || code != 0 {
		t.Fatalf("create: handled=%v code=%d, want it recorded and left to the stateless stub", handled, code)
	}
	var listed []map[string]string
	if err := json.Unmarshal([]byte(teaCICall(t, stub, "pulls", "list")), &listed); err != nil || len(listed) != 1 || listed[0]["index"] != "99" || listed[0]["head"] != "feature" {
		t.Fatalf("list after create = %v (%v), want PR 99 for feature", listed, err)
	}

	if got := teaCICall(t, stub, "api", "--login", "e2e", "/repos/owner/repo/pulls/99"); !strings.Contains(got, `"body":"body"`) || !strings.Contains(got, `"number":99`) {
		t.Fatalf("raw pull after create = %s", got)
	}
	teaCICall(t, stub, "pulls", "edit", "99", "--repo", "owner/repo", "--description", "attested")
	if got := teaCICall(t, stub, "api", "--login", "e2e", "/repos/owner/repo/pulls/99"); !strings.Contains(got, `"body":"attested"`) || !strings.Contains(got, `"title":"t"`) {
		t.Fatalf("raw pull after edit = %s", got)
	}

	var view map[string]any
	_ = json.Unmarshal([]byte(teaCICall(t, stub, "pulls", "99", "--output", "json")), &view)
	if view["headSha"] != created || view["hasMerged"] != false || view["state"] != "open" {
		t.Fatalf("view at the created head = %v", view)
	}
	if got := teaCICall(t, stub, "actions", "runs", "list", "--branch", "feature"); strings.Count(got, `"id"`) != 1 {
		t.Fatalf("runs at the created head = %s, want one", got)
	}
	jobs := teaCICall(t, stub, "api", "--login", "e2e", "/repos/owner/repo/actions/runs/1/jobs")
	if !strings.Contains(jobs, `"conclusion":"failure"`) || !strings.Contains(jobs, created) {
		t.Fatalf("run 1 jobs = %s, want a failure at the created head", jobs)
	}
	if got := teaCICall(t, stub, "actions", "runs", "logs", "1", "--job", "11"); got != teaCIFailingLogs {
		t.Fatalf("failing job log = %q", got)
	}

	teaCIGit(t, work, "commit", "--allow-empty", "-m", "repair")
	teaCIGit(t, work, "push", remote, "feature")
	repaired := teaCIGit(t, work, "rev-parse", "HEAD")
	view = nil
	_ = json.Unmarshal([]byte(teaCICall(t, stub, "pulls", "99", "--output", "json")), &view)
	if view["headSha"] != repaired || view["hasMerged"] != true {
		t.Fatalf("view after the repair = %v, want merged at %s", view, repaired)
	}
	jobs = teaCICall(t, stub, "api", "--login", "e2e", "/repos/owner/repo/actions/runs/2/jobs")
	if !strings.Contains(jobs, `"conclusion":"success"`) || !strings.Contains(jobs, repaired) {
		t.Fatalf("run 2 jobs = %s, want a success at the repaired head", jobs)
	}
}
