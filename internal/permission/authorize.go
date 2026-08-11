// Package permission provides the single scoped authorization decision point.
package permission

import (
	"context"
	"fmt"
	"strings"

	"github.com/example/safefilehub/internal/config"
	"github.com/example/safefilehub/internal/db"
	"github.com/example/safefilehub/internal/pathpolicy"
)

type Authorizer struct {
	repository interface {
		PermissionsForUserAndRoot(context.Context, int64, int64) ([]db.Permission, error)
	}
	policy config.NamePolicy
}

func NewAuthorizer(repository interface {
	PermissionsForUserAndRoot(context.Context, int64, int64) ([]db.Permission, error)
}, policy config.NamePolicy) *Authorizer { return &Authorizer{repository: repository, policy: policy} }

// Authorize applies permissions for exactly one user, root, canonical logical
// path, and action. It defaults to deny; the longest matching prefix controls
// the result and a deny at that prefix wins.
func (a *Authorizer) Authorize(ctx context.Context, userID, rootID int64, logicalPath, action string) (bool, error) {
	path, err := canonicalPath(logicalPath, a.policy)
	if err != nil {
		return false, err
	}
	action = strings.ToLower(strings.TrimSpace(action))
	if action != "read" && action != "write" && action != "delete" && action != "archive" {
		return false, fmt.Errorf("invalid permission action %q", action)
	}
	permissions, err := a.repository.PermissionsForUserAndRoot(ctx, userID, rootID)
	if err != nil {
		return false, err
	}
	best := -1
	allow := false
	denied := false
	for _, p := range permissions {
		if p.Action != action {
			continue
		}
		prefix, err := canonicalPath(p.PathPrefix, a.policy)
		if err != nil {
			return false, fmt.Errorf("invalid stored permission prefix: %w", err)
		}
		if !matches(path, prefix) {
			continue
		}
		n := len(prefix)
		if n > best {
			best = n
			allow = p.Allow
			denied = !p.Allow
		} else if n == best && !p.Allow {
			denied = true
		}
	}
	return best >= 0 && allow && !denied, nil
}
func canonicalPath(value string, policy config.NamePolicy) (string, error) {
	if value == "/" {
		return "/", nil
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("logical path must be canonical")
	}
	p, err := pathpolicy.ParseDecodedPath(strings.TrimPrefix(value, "/"), policy)
	if err != nil {
		return "", err
	}
	if p.Canonical != value {
		return "", fmt.Errorf("logical path must be canonical")
	}
	return p.Canonical, nil
}
func matches(path, prefix string) bool {
	return prefix == "/" || path == prefix || strings.HasPrefix(path, prefix+"/")
}
