package client

// UploadCIJobs sends GitHub Actions job metadata (one repo, at most 500 jobs)
// to POST /v1/ci/jobs. The service prices the jobs and matches them to the
// signed-in user's agent sessions.
func (c *Client) UploadCIJobs(body interface{}) *APIResponse {
	return c.jsonRequest("POST", "/v1/ci/jobs", body)
}
