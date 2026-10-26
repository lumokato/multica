package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/multica-ai/multica/server/internal/util"
)

func TestCrossWorkspacePolicy(t *testing.T) {
	const a = "0a0a0a0a-0000-4000-8000-00000000000a"
	const b = "0b0b0b0b-0000-4000-8000-00000000000b"
	const c = "0c0c0c0c-0000-4000-8000-00000000000c"

	t.Run("empty configuration denies cross workspace", func(t *testing.T) {
		t.Setenv("MULTICA_CROSS_WORKSPACE_AGENT_ACCESS", "")
		p := NewCrossWorkspacePolicyFromEnv()
		if !p.allows(a, a) || p.allows(a, b) {
			t.Fatalf("policy decision = same:%v cross:%v", p.allows(a, a), p.allows(a, b))
		}
	})
	t.Run("configured pair is allowed and unrelated target is denied", func(t *testing.T) {
		t.Setenv("MULTICA_CROSS_WORKSPACE_AGENT_ACCESS", a+">"+b)
		p := NewCrossWorkspacePolicyFromEnv()
		if !p.allows(a, b) || p.allows(a, c) || p.allows(b, a) {
			t.Fatal("unexpected cross-workspace allow-list decision")
		}
	})
	t.Run("allow destructive override", func(t *testing.T) {
		t.Setenv("MULTICA_CROSS_WORKSPACE_AGENT_ACCESS", a+">"+b)
		t.Setenv("MULTICA_CROSS_WORKSPACE_AGENT_ACCESS_ALLOW_DESTRUCTIVE", "1")
		p := NewCrossWorkspacePolicyFromEnv()
		if !p.allowDestructive {
			t.Fatal("expected destructive override")
		}
	})
	t.Run("destructive path is denied by default", func(t *testing.T) {
		t.Setenv("MULTICA_CROSS_WORKSPACE_AGENT_ACCESS", a+">"+b)
		p := NewCrossWorkspacePolicyFromEnv()
		req := httptest.NewRequest(http.MethodDelete, "/api/workspaces/"+b+"/", nil)
		if p.allowDestructive || !isCrossWorkspaceDestructivePath(req.Method, req.URL.Path) {
			t.Fatal("destructive path policy not enabled")
		}
	})
	t.Run("invalid UUID fails fast", func(t *testing.T) {
		t.Setenv("MULTICA_CROSS_WORKSPACE_AGENT_ACCESS", "not-a-uuid>"+b)
		defer func() {
			if recover() == nil {
				t.Fatal("expected invalid configuration panic")
			}
		}()
		NewCrossWorkspacePolicyFromEnv()
	})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/workspaces/" + b + "/members"},
		{http.MethodDelete, "/api/workspaces/" + b + "/invitations/id"},
		{http.MethodPost, "/api/workspaces/" + b + "/share-links"},
		{http.MethodPost, "/api/invitations/id/accept"},
		{http.MethodPost, "/api/share-links/join"},
		{http.MethodPut, "/api/agents/id/env"},
		{http.MethodPost, "/api/cli-token"},
	} {
		if !isCrossWorkspaceDestructivePath(tc.method, tc.path) {
			t.Errorf("isCrossWorkspaceDestructivePath(%s, %s) = false", tc.method, tc.path)
		}
	}

	if _, err := util.ParseUUID(a); err != nil {
		t.Fatal(err)
	}
}
