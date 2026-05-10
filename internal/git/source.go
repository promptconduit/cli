package git

import (
	"strings"
)

// DetectSource maps a git remote URL to a short provider name.
// Returns "" when the URL is empty or the provider is not recognized.
//
// Supports both SSH (git@github.com:org/repo.git) and HTTPS
// (https://github.com/org/repo) forms.
func DetectSource(remoteURL string) string {
	if remoteURL == "" {
		return ""
	}
	host := extractHost(remoteURL)
	host = strings.ToLower(host)

	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return "github"
	case host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com"):
		return "gitlab"
	case host == "bitbucket.org" || strings.HasSuffix(host, ".bitbucket.org"):
		return "bitbucket"
	case host == "dev.azure.com" || strings.HasSuffix(host, ".visualstudio.com"):
		return "azure"
	case host == "codeberg.org":
		return "codeberg"
	case host == "git.sr.ht":
		return "sourcehut"
	}
	return ""
}

// extractHost pulls the hostname out of common git remote URL forms.
func extractHost(remoteURL string) string {
	// SSH: git@host:path or ssh://git@host/path
	if strings.HasPrefix(remoteURL, "git@") {
		rest := strings.TrimPrefix(remoteURL, "git@")
		if idx := strings.IndexAny(rest, ":/"); idx >= 0 {
			return rest[:idx]
		}
		return rest
	}

	// scheme://[user@]host[:port]/path
	if idx := strings.Index(remoteURL, "://"); idx >= 0 {
		rest := remoteURL[idx+3:]
		// Strip optional userinfo
		if at := strings.Index(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		// Up to first '/' or ':'
		if end := strings.IndexAny(rest, "/:"); end >= 0 {
			return rest[:end]
		}
		return rest
	}

	return ""
}
