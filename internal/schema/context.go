package schema

// GitContext contains git repository state information
type GitContext struct {
	// Commit info
	CommitHash      *string `json:"commit_hash,omitempty"`
	CommitMessage   *string `json:"commit_message,omitempty"`
	CommitAuthor    *string `json:"commit_author,omitempty"`
	CommitTimestamp *string `json:"commit_timestamp,omitempty"`

	// Branch info
	Branch         *string `json:"branch,omitempty"`
	IsDetachedHead *bool   `json:"is_detached_head,omitempty"`

	// Working tree state
	IsDirty        *bool `json:"is_dirty,omitempty"`
	StagedCount    *int  `json:"staged_count,omitempty"`
	UnstagedCount  *int  `json:"unstaged_count,omitempty"`
	UntrackedCount *int  `json:"untracked_count,omitempty"`

	// Remote tracking
	AheadCount     *int    `json:"ahead_count,omitempty"`
	BehindCount    *int    `json:"behind_count,omitempty"`
	UpstreamBranch *string `json:"upstream_branch,omitempty"`
	RemoteURL      *string `json:"remote_url,omitempty"`
}

// WorkspaceContext contains information about the workspace
type WorkspaceContext struct {
	RepoName         *string  `json:"repo_name,omitempty"`
	RepoPath         *string  `json:"repo_path,omitempty"`
	WorkingDirectory *string  `json:"working_directory,omitempty"`
	FilesReferenced  []string `json:"files_referenced,omitempty"`
}
