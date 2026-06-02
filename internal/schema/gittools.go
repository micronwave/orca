package schema

import "time"

// RepoStatusSnapshot captures git working-tree status at a plan-cycle boundary.
// Written by the orchestrator during emitCycleStartSnapshot so that status and
// diff provenance are available in the artifact graph. orca.md Phase B §3.
type RepoStatusSnapshot struct {
	SnapshotID     string    `json:"snapshot_id"`
	GoalID         string    `json:"goal_id"`
	WorkDir        string    `json:"work_dir"`
	Branch         string    `json:"branch"`
	Clean          bool      `json:"clean"`
	StagedCount    int       `json:"staged_count"`
	UnstagedCount  int       `json:"unstaged_count"`
	UntrackedCount int       `json:"untracked_count"`
	CreatedAt      time.Time `json:"created_at"`
}

// DiffSnapshot captures a point-in-time diff summary for a worktree.
// Optional; populated only when diff provenance is needed for evidence.
// orca.md Phase B §3.
type DiffSnapshot struct {
	SnapshotID   string    `json:"snapshot_id"`
	GoalID       string    `json:"goal_id"`
	WorkDir      string    `json:"work_dir"`
	Staged       bool      `json:"staged"`
	ChangedFiles []string  `json:"changed_files"`
	CreatedAt    time.Time `json:"created_at"`
}
