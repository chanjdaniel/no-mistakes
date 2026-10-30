package config

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
)

// test.evidence.attach_media once uploaded image and video evidence to GitHub
// user-attachments. No image or video of the work leaves the machine any more,
// so the key is a parse-only tombstone like jev: accepted in global and repo
// config, never read, and reported as having no effect.

func TestRetiredAttachMediaParsesWithoutEffect(t *testing.T) {
	without := Merge(mustLoadGlobal(t, "log_level: info\n"), mustLoadRepo(t, "test:\n  evidence:\n    dir: docs/evidence\n"))
	for _, value := range []string{"true", "false"} {
		t.Run(value, func(t *testing.T) {
			global := mustLoadGlobal(t, "log_level: info\ntest:\n  evidence:\n    attach_media: "+value+"\n")
			repo := mustLoadRepo(t, "test:\n  evidence:\n    dir: docs/evidence\n    attach_media: "+value+"\n")
			with := Merge(global, repo)
			if !reflect.DeepEqual(with.Test, without.Test) {
				t.Fatalf("attach_media: %s changed the merged test config:\nwith    %#v\nwithout %#v", value, with.Test, without.Test)
			}
		})
	}
}

func TestRetiredAttachMediaWarnsAtLoad(t *testing.T) {
	for _, tc := range []struct {
		name string
		load func(t *testing.T, yaml string)
	}{
		{"global", func(t *testing.T, yaml string) { mustLoadGlobal(t, yaml) }},
		{"repo", func(t *testing.T, yaml string) { mustLoadRepo(t, yaml) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(prev)

			tc.load(t, "test:\n  evidence:\n    attach_media: true\n")
			logged := buf.String()
			if strings.Count(logged, "test.evidence.attach_media") != 1 || !strings.Contains(logged, "no effect") {
				t.Fatalf("want one no-effect deprecation for test.evidence.attach_media, got: %s", logged)
			}
			buf.Reset()
			tc.load(t, "test:\n  evidence:\n    dir: docs/evidence\n")
			if strings.Contains(buf.String(), "attach_media") {
				t.Fatalf("an absent attach_media must not warn: %s", buf.String())
			}
		})
	}
}

func mustLoadGlobal(t *testing.T, yaml string) *GlobalConfig {
	t.Helper()
	cfg, err := LoadGlobalFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("global config must load: %v", err)
	}
	return cfg
}

func mustLoadRepo(t *testing.T, yaml string) *RepoConfig {
	t.Helper()
	cfg, err := LoadRepoFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("repo config must load: %v", err)
	}
	return cfg
}
