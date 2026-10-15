package gitcmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetUrlAndResolveConfig(t *testing.T) {
	t.Parallel()

	type args struct {
		remoteURL       string
		resolvedAddress string
	}
	tests := []struct {
		desc               string
		args               args
		expectedURL        string
		expectedConfigPair []ConfigPair
		expectedErrString  string
	}{
		{
			desc: "No remote URL provided",
			args: args{
				remoteURL:       "",
				resolvedAddress: "192.0.0.1",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "URL is empty",
		},
		{
			desc: "No resolved address provided",
			args: args{
				remoteURL:       "www.gitlab.com",
				resolvedAddress: "",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "resolved address is empty",
		},
		{
			desc: "resolved address has malformed IP",
			args: args{
				remoteURL:       "www.gitlab.com",
				resolvedAddress: "abcd",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "resolved address has invalid IPv4/IPv6 address",
		},
		{
			desc: "resolved address has malformed IP",
			args: args{
				remoteURL:       "www.gitlab.com",
				resolvedAddress: "1922.23.2323.2323",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "resolved address has invalid IPv4/IPv6 address",
		},
		{
			desc: "bad URL format",
			args: args{
				remoteURL:       "http//foo/bar",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "invalid protocol/URL encountered: http//foo/bar",
		},
		{
			desc: "valid http format",
			args: args{
				remoteURL:       "http://gitlab.com/gitlab-org/gitaly.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "http://gitlab.com/gitlab-org/gitaly.git",
			expectedConfigPair: []ConfigPair{{Key: "http.curloptResolve", Value: "gitlab.com:80:192.168.0.1"}},
			expectedErrString:  "",
		},
		{
			desc: "valid https format",
			args: args{
				remoteURL:       "https://gitlab.com/gitlab-org/gitaly.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "https://gitlab.com/gitlab-org/gitaly.git",
			expectedConfigPair: []ConfigPair{{Key: "http.curloptResolve", Value: "gitlab.com:443:192.168.0.1"}},
			expectedErrString:  "",
		},
		{
			desc: "valid https format with port",
			args: args{
				remoteURL:       "https://gitlab.com:1234/gitlab-org/gitaly.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "https://gitlab.com:1234/gitlab-org/gitaly.git",
			expectedConfigPair: []ConfigPair{{Key: "http.curloptResolve", Value: "gitlab.com:1234:192.168.0.1"}},
			expectedErrString:  "",
		},
		{
			desc: "valid ssh format",
			args: args{
				remoteURL:       "ssh://user@gitlab.com/gitlab-org/gitaly.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "ssh://user@192.168.0.1/gitlab-org/gitaly.git",
			expectedConfigPair: nil,
			expectedErrString:  "",
		},
		{
			desc: "valid ssh format without username",
			args: args{
				remoteURL:       "ssh://gitlab.com/gitlab-org/gitaly.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "ssh://192.168.0.1/gitlab-org/gitaly.git",
			expectedConfigPair: nil,
			expectedErrString:  "",
		},
		{
			desc: "invalid ssh format",
			args: args{
				remoteURL:       "ssh://foo" + string(rune(0x7f)),
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "couldn't parse remoteURL",
		},
		{
			desc: "valid ssh format (scp-like) without prefix",
			args: args{
				remoteURL:       "user@gitlab.com:gitlab-org/gitaly.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "user@192.168.0.1:gitlab-org/gitaly.git",
			expectedConfigPair: nil,
			expectedErrString:  "",
		},
		{
			desc: "valid ssh format (scp-like) without prefix without user",
			args: args{
				remoteURL:       "gitlab.com:gitlab-org/gitaly.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "192.168.0.1:gitlab-org/gitaly.git",
			expectedConfigPair: nil,
			expectedErrString:  "",
		},
		{
			desc: "invalid format (shouldn't fallback to other protocol)",
			args: args{
				remoteURL:       "gitlab.com/gitlab-org/gitaly.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "invalid protocol/URL encountered: gitlab.com/gitlab-org/gitaly.git",
		},
		{
			desc: "invalid git (SCP) url provided",
			args: args{
				remoteURL:       "git@gitlab.com/foo/bar",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "invalid protocol/URL encountered: git@gitlab.com/foo/bar",
		},
		{
			desc: "valid git (SCP) url provided",
			args: args{
				remoteURL:       "git@gitlab.com:gitlab-org/security/gitlab.git",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "git@192.168.0.1:gitlab-org/security/gitlab.git",
			expectedConfigPair: nil,
			expectedErrString:  "",
		},
		{
			desc: "valid git url provided",
			args: args{
				remoteURL:       "git://www.gitlab.com/foo/bar",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "git://www.gitlab.com/foo/bar",
			expectedConfigPair: []ConfigPair{{Key: "http.curloptResolve", Value: "www.gitlab.com:9418:192.168.0.1"}},
			expectedErrString:  "",
		},
		{
			desc: "ssh url with slash before colon",
			args: args{
				remoteURL:       "foo@bar/goo.com:/abc/def",
				resolvedAddress: "192.168.0.1",
			},
			expectedURL:        "",
			expectedConfigPair: nil,
			expectedErrString:  "SSH URLs with '/' before colon are unsupported",
		},
	}
	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			url, configPair, err := GetURLAndResolveConfig(tc.args.remoteURL, tc.args.resolvedAddress)

			if tc.expectedErrString != "" {
				require.Error(t, err, tc.expectedErrString)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.expectedURL, url)
			require.Equal(t, tc.expectedConfigPair, configPair)
		})
	}
}
