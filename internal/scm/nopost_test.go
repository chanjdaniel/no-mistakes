package scm_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/scm/azuredevops"
	"github.com/kunchenguid/no-mistakes/internal/scm/forgejo"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitea"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// TestHostsCannotPostCommentsOrUploadFiles pins what a forge host can publish:
// a PR's title and body, through CreatePR and UpdatePR. No host has a method
// that writes a PR comment or uploads a file, so no step can post one; reading
// review comments (GetReviewComments, for the CI step) stays allowed. A new
// writer must be a deliberate change to this test, not a quiet addition.
func TestHostsCannotPostCommentsOrUploadFiles(t *testing.T) {
	t.Parallel()
	hosts := []any{
		(*github.Host)(nil),
		(*gitlab.Host)(nil),
		(*gitea.Host)(nil),
		(*forgejo.Host)(nil),
		(*azuredevops.Host)(nil),
		(*bitbucket.Host)(nil),
	}
	for _, host := range hosts {
		typ := reflect.TypeOf(host)
		for i := 0; i < typ.NumMethod(); i++ {
			name := typ.Method(i).Name
			lower := strings.ToLower(name)
			readOnly := strings.HasPrefix(name, "Get") || strings.HasPrefix(name, "List")
			if strings.Contains(lower, "comment") && !readOnly {
				t.Errorf("%s has a comment-writing method %s; the PR step posts no PR comments", typ, name)
			}
			if strings.Contains(lower, "upload") || strings.Contains(lower, "attach") || strings.Contains(lower, "asset") {
				t.Errorf("%s has an upload method %s; no image or video of the work may leave the machine", typ, name)
			}
		}
	}
}
