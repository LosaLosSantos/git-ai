package resolver

import (
	"testing"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
	"github.com/stretchr/testify/require"
)

func TestAllowed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		path  string
		allow []string
		deny  []string
		want  bool
	}{
		{name: "no lists allows everything", path: "a/b.go", want: true},
		{name: "deep prefix denied", path: "secrets/a/b.env", deny: []string{"secrets/**"}, want: false},
		{name: "deep prefix matches its direct child", path: "secrets/x", deny: []string{"secrets/**"}, want: false},
		{name: "deep prefix does not match a sibling with the same start", path: "secretsx/a", deny: []string{"secrets/**"}, want: true},
		{name: "deep prefix does not match the bare directory name", path: "secrets", deny: []string{"secrets/**"}, want: true},
		{name: "deny wins over allow", path: "src/keys/a.pem", allow: []string{"src/**"}, deny: []string{"src/keys/**"}, want: false},
		{name: "allow restricts", path: "docs/a.md", allow: []string{"src/**"}, want: false},
		{name: "allow matches", path: "src/a.go", allow: []string{"src/**"}, want: true},
		{name: "single star does not cross a slash", path: "a/b.go", allow: []string{"*.go"}, want: false},
		{name: "single star matches top level", path: "a.go", allow: []string{"*.go"}, want: true},
		{name: "exact path", path: "CHANGELOG.md", allow: []string{"CHANGELOG.md"}, want: true},
		{name: "bare double star matches everything", path: "x/y/z", allow: []string{"**"}, want: true},
		{name: "bare double star deny denies everything", path: "x", deny: []string{"**"}, want: false},
		{name: "loose trailing double star", path: "release-2/x", deny: []string{"release**"}, want: false},
		{name: "malformed deny fails closed", path: "a", deny: []string{"[**"}, want: false},
		{name: "malformed deny with suffix fails closed", path: "a/b", deny: []string{"[/**"}, want: false},
		{name: "malformed allow matches nothing", path: "a", allow: []string{"["}, want: false},
		{name: "malformed allow does not hide a valid one", path: "a", allow: []string{"[", "a"}, want: true},
		{name: "empty path is never allowed", path: "", want: false},
		{name: "dot is never allowed", path: ".", want: false},
		{name: "parent traversal is never allowed", path: "../x", want: false},
		{name: "embedded traversal is never allowed", path: "src/../secrets/key", allow: []string{"src/**"}, want: false},
		{name: "absolute path is never allowed", path: "/etc/passwd", want: false},
		{name: "unclean path is never allowed", path: "a//b", want: false},
		{name: "trailing slash is never allowed", path: "a/", want: false},
		{name: "NUL byte is never allowed", path: "a\x00b", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, Allowed(tc.path, tc.allow, tc.deny))
		})
	}
}

func TestActorAllowed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		actor string
		globs []string
		want  bool
	}{
		{name: "no globs allows every caller", actor: "anyone", want: true},
		{name: "no globs allows an empty actor", actor: "", want: true},
		{name: "exact match", actor: "agent-7", globs: []string{"agent-7"}, want: true},
		{name: "glob match", actor: "agent-7", globs: []string{"agent-*"}, want: true},
		{name: "no match", actor: "human", globs: []string{"agent-*"}, want: false},
		{name: "glob does not cross a slash", actor: "agent/7", globs: []string{"agent-*"}, want: false},
		{name: "empty actor with globs is refused", actor: "", globs: []string{"*"}, want: false},
		{name: "malformed glob does not match", actor: "agent-7", globs: []string{"["}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tc.want, ActorAllowed(tc.actor, tc.globs))
		})
	}
}

func TestClientPolicyMethods(t *testing.T) {
	t.Parallel()

	client, err := New(config.GraftResolver{
		Enabled:    true,
		Command:    []string{"/bin/true"},
		PathsAllow: []string{"src/**"},
		PathsDeny:  []string{"src/secrets/**"},
		Actors:     []string{"agent-*"},
	})
	require.NoError(t, err)

	require.True(t, client.PathAllowed("src/a.go"))
	require.False(t, client.PathAllowed("src/secrets/key.pem"))
	require.False(t, client.PathAllowed("docs/a.md"))
	require.True(t, client.ActorAllowed("agent-1"))
	require.False(t, client.ActorAllowed("root"))
}

func TestNilClientPolicyMethodsAllowNothing(t *testing.T) {
	t.Parallel()

	var client *Client
	require.False(t, client.PathAllowed("a"))
	require.False(t, client.ActorAllowed("agent-1"))
}
