package client

import "net/url"

// UploadCIJobs sends GitHub Actions job metadata (one repo, at most 500 jobs)
// to POST /v1/ci/jobs. The service prices the jobs and matches them to the
// signed-in user's agent sessions.
func (c *Client) UploadCIJobs(body interface{}) *APIResponse {
	return c.jsonRequest("POST", "/v1/ci/jobs", body)
}

// GetSessionCI returns the CI (GitHub Actions) cost attributed to one of the
// user's sessions: ciUsd, ciJobs, ciMinutes.
func (c *Client) GetSessionCI(sessionID string) *APIResponse {
	return c.Get("/v1/me/ci/sessions/"+url.PathEscape(sessionID), nil)
}

// GetTeamCI returns a team's CI cost report for a period (7d|30d|90d): total,
// waste signals with $ and fix, and the sessions behind it.
func (c *Client) GetTeamCI(orgID, period string) *APIResponse {
	return c.Get(orgPath(orgID, "/ci"), map[string]string{"period": period})
}
