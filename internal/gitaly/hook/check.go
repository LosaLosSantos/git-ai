package hook

import (
	"context"

	"github.com/feder-cr/git-ai/internal/gitlab"
)

//nolint:revive // This is unintentionally missing documentation.
func (m *GitLabHookManager) Check(ctx context.Context) (*gitlab.CheckInfo, error) {
	return m.gitlabClient.Check(ctx)
}
