package gitcmd

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// GetURLAndResolveConfig parses the given repository's URL and resolved address to generate
// the modified URL and configuration to avoid DNS rebinding.
//
// In Git v2.37.0 we added the functionality for `http.curloptResolve` which like its
// curl counterpart when provided with a `HOST:PORT:ADDRESS` value, uses the IP Address provided
// directly for the given HOST:PORT combination for HTTP/HTTPS protocols, without requiring
// DNS resolution.
//
// This functions currently does the following operations:
//
// - Git Protocol: Replaces the hostname with the resolved IP address.
// - SSH Protocol: Replaces the hostname with the resolved IP address (supports both the regular syntax
// `ssh://[user@]server/project.git` and scp-like syntax `[user@]server:project.git`).
// - HTTP/HTTPS Protocol: Keeps the URL as is, but adds the `http.curloptResolve` flag.
//
// SideNote: We cannot replace the hostname with IP in HTTPS protocol because the protocol
// demands the hostname to be present, as it is required for the SSL verification.
func GetURLAndResolveConfig(remoteURL string, resolvedAddress string) (string, []ConfigPair, error) {
	if remoteURL == "" {
		return "", nil, fmt.Errorf("URL is empty")
	}

	if resolvedAddress == "" {
		return "", nil, fmt.Errorf("resolved address is empty")
	}

	resolvedIP := net.ParseIP(resolvedAddress)
	if resolvedIP == nil {
		return "", nil, fmt.Errorf("resolved address has invalid IPv4/IPv6 address")
	}

	switch {
	case strings.HasPrefix(remoteURL, "http://"), strings.HasPrefix(remoteURL, "https://"), strings.HasPrefix(remoteURL, "git://"):
		return getURLAndResolveConfigForURL(remoteURL, resolvedAddress)
	case strings.HasPrefix(remoteURL, "ssh://"):
		return getURLAndResolveConfigForSSH(remoteURL, resolvedAddress)
	default:
		return getURLAndResolveConfigForSCP(remoteURL, resolvedAddress)
	}
}

func getURLAndResolveConfigForSSH(remoteURL, resolvedAddress string) (string, []ConfigPair, error) {
	u, err := url.ParseRequestURI(remoteURL)
	if err != nil {
		return "", nil, fmt.Errorf("couldn't parse remoteURL: %w", err)
	}

	u.Host = resolvedAddress

	return u.String(), nil, nil
}

func getURLAndResolveConfigForSCP(remoteURL, resolvedAddress string) (string, []ConfigPair, error) {
	hostAndPath := strings.SplitN(remoteURL, ":", 2)
	if len(hostAndPath) != 2 {
		return "", nil, fmt.Errorf("invalid protocol/URL encountered: %s", remoteURL)
	}

	if strings.Contains(hostAndPath[0], "/") {
		return "", nil, fmt.Errorf("SSH URLs with '/' before colon are unsupported")
	}

	var userPrefix string

	if userAndHost := strings.SplitAfterN(remoteURL, "@", 2); len(userAndHost) > 1 {
		userPrefix = userAndHost[0]
	}

	return fmt.Sprintf("%s%s:%s", userPrefix, resolvedAddress, hostAndPath[1]), nil, nil
}

func getURLAndResolveConfigForURL(remoteURL, resolvedAddress string) (string, []ConfigPair, error) {
	u, err := url.ParseRequestURI(remoteURL)
	if err != nil {
		return "", nil, fmt.Errorf("couldn't parse remoteURL: %w", err)
	}

	port := u.Port()

	if port == "" {
		switch u.Scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		case "git":
			port = "9418"
		default:
			return "", nil, fmt.Errorf("unknown schema provided: %s", u.Scheme)
		}
	}

	return remoteURL, []ConfigPair{
		{Key: "http.curloptResolve", Value: fmt.Sprintf("%s:%s:%s", u.Hostname(), port, resolvedAddress)},
	}, nil
}
