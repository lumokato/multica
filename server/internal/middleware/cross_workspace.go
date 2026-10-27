package middleware

import (
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/multica-ai/multica/server/internal/util"
)

// CrossWorkspacePolicy is the explicit allow-list for task-token requests that
// target a workspace other than the one bound to the token. It is deliberately
// process-scoped: changing the environment requires a restart.
type CrossWorkspacePolicy struct {
	allowed          map[string]struct{}
	allowDestructive bool
}

// NewCrossWorkspacePolicyFromEnv parses the cross-workspace policy at server
// startup. Invalid configuration is a programmer/operator error and must not
// silently disable the boundary, so callers should let the panic fail startup.
func NewCrossWorkspacePolicyFromEnv() *CrossWorkspacePolicy {
	p := &CrossWorkspacePolicy{allowed: make(map[string]struct{})}
	for _, raw := range strings.Split(strings.TrimSpace(os.Getenv("MULTICA_CROSS_WORKSPACE_AGENT_ACCESS")), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parts := strings.Split(raw, ">")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			panic(fmt.Sprintf("invalid MULTICA_CROSS_WORKSPACE_AGENT_ACCESS entry %q: expected sourceUUID>targetUUID", raw))
		}
		source, err := util.ParseUUID(strings.TrimSpace(parts[0]))
		if err != nil {
			panic(fmt.Sprintf("invalid MULTICA_CROSS_WORKSPACE_AGENT_ACCESS source %q: %v", parts[0], err))
		}
		target, err := util.ParseUUID(strings.TrimSpace(parts[1]))
		if err != nil {
			panic(fmt.Sprintf("invalid MULTICA_CROSS_WORKSPACE_AGENT_ACCESS target %q: %v", parts[1], err))
		}
		p.allowed[util.UUIDToString(source)+">"+util.UUIDToString(target)] = struct{}{}
	}
	p.allowDestructive = os.Getenv("MULTICA_CROSS_WORKSPACE_AGENT_ACCESS_ALLOW_DESTRUCTIVE") == "1"
	return p
}

func (p *CrossWorkspacePolicy) allows(source, target string) bool {
	source, target, ok := canonicalWorkspacePair(source, target)
	if !ok {
		return false
	}
	if source == target {
		return true
	}
	if p == nil {
		return false
	}
	_, ok = p.allowed[source+">"+target]
	return ok
}

func canonicalWorkspacePair(source, target string) (string, string, bool) {
	sourceUUID, sourceErr := util.ParseUUID(source)
	targetUUID, targetErr := util.ParseUUID(target)
	if sourceErr != nil || targetErr != nil {
		return "", "", false
	}
	return util.UUIDToString(sourceUUID), util.UUIDToString(targetUUID), true
}

func crossWorkspaceAllows(source, target string) bool {
	return NewCrossWorkspacePolicyFromEnv().allows(source, target)
}

func crossWorkspaceDestructiveAllowed(source, target, method, path string) bool {
	policy := NewCrossWorkspacePolicyFromEnv()
	canonicalSource, canonicalTarget, ok := canonicalWorkspacePair(source, target)
	return ok && (canonicalSource == canonicalTarget || policy.allowDestructive || !isCrossWorkspaceDestructivePath(method, path))
}

// isCrossWorkspaceDestructivePath keeps the cross-workspace exception focused
// on agent collaboration. Credential, membership, sharing, and workspace
// lifecycle mutations remain human/workspace-local operations by default.
func isCrossWorkspaceDestructivePath(method, path string) bool {
	path = strings.TrimRight(path, "/")
	if method == http.MethodDelete && strings.HasPrefix(path, "/api/workspaces/") {
		rest := strings.TrimPrefix(path, "/api/workspaces/")
		if !strings.Contains(rest, "/") {
			return true
		}
	}
	if strings.HasPrefix(path, "/api/tokens") || strings.HasPrefix(path, "/api/cli-token") {
		return true
	}
	if strings.HasPrefix(path, "/api/invitations") || strings.HasPrefix(path, "/api/share-links") {
		return true
	}
	for _, segment := range []string{"/members", "/invitations", "/share-links"} {
		if strings.Contains(path, segment) && strings.HasPrefix(path, "/api/workspaces/") {
			return true
		}
	}
	if strings.HasPrefix(path, "/api/agents/") && strings.HasSuffix(path, "/env") {
		return true
	}
	return false
}
