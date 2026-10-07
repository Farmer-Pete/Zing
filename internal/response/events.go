package response

// CheckRerunEvent is the check_rerun typed event's payload (messages table,
// event_kind "check_rerun"): one re-run of a GitHub check run on sha. #91
// writes it and gates re-runs on its count per check per sha.
type CheckRerunEvent struct {
	Check      string      `json:"check" jsonschema:"minLength=1,maxLength=200"`
	SHA        string      `json:"sha" jsonschema:"pattern=^[0-9a-f]{40}$"`
	RunID      int64       `json:"run_id" jsonschema:"minimum=1"`
	CheckRunID int64       `json:"check_run_id" jsonschema:"minimum=1"`
	Reason     RerunReason `json:"reason"`
	Tests      []string    `json:"tests,omitempty" jsonschema:"maxItems=20"`
}

// CheckRerunPassedEvent is the check_rerun_passed typed event's payload
// (messages table, event_kind "check_rerun_passed"): a check that had a
// flaky or no_log check_rerun on sha passed once re-run, so the fix run it
// would have spent is skipped.
type CheckRerunPassedEvent struct {
	Check string   `json:"check" jsonschema:"minLength=1,maxLength=200"`
	SHA   string   `json:"sha" jsonschema:"pattern=^[0-9a-f]{40}$"`
	Tests []string `json:"tests,omitempty" jsonschema:"maxItems=20"`
}

// OwnerEditEvent is the owner_edit typed event's payload (messages table,
// event_kind "owner_edit"): one owner edit to a sealed scenario, a sealed
// plan's task, a sealed plan's file task list, or the ticket body (#41,
// #51). Old and New hold the full prior and new text -- the entire scenario
// or plan payload JSON for those targets (plan_file instead holds just the
// file's task list), since a plan drop rewrites the whole task and file
// list, or the raw ticket body text for ticket_body.
type OwnerEditEvent struct {
	Target string `json:"target" jsonschema:"enum=scenario,enum=plan_task,enum=plan_file,enum=ticket_body"`
	Ref    string `json:"ref"`
	Action string `json:"action" jsonschema:"enum=edit,enum=drop"`
	Old    string `json:"old"`
	New    string `json:"new"`
	Reason string `json:"reason,omitempty" doc:"the judge's reason when a handler commit applied an accepted amendment; empty for every console edit"`
}

// PlanUnblockEvent is the plan_unblock typed event's payload (messages
// table, event_kind "plan_unblock"): one successful unblock turn at plan
// review's loop cap -- the plan version whose capped review it read, the
// above-floor finding ids still open, and the guidance planning resumes
// with.
type PlanUnblockEvent struct {
	PlanVersion int      `json:"plan_version" jsonschema:"minimum=1"`
	FindingIDs  []string `json:"finding_ids"  jsonschema:"minItems=1"`
	Guidance    string   `json:"guidance"     jsonschema:"minLength=1"`
}

// BudgetRaisedEvent is the budget_raised typed event's payload (messages
// table, event_kind "budget_raised"): one owner pick of the wall_clock
// cap_budget escalation's chip d, raising this ticket's agent budget by
// Minutes on top of the global budget.
type BudgetRaisedEvent struct {
	Minutes int `json:"minutes" jsonschema:"minimum=1,maximum=525600"`
}

// StaleBaseEvent is the stale_base typed event's payload (messages table,
// event_kind "stale_base"): a step that used the last fetched base
// because the fetch itself failed, once per step and sha (#68 follow-up).
type StaleBaseEvent struct {
	Step   string `json:"step" jsonschema:"enum=building,enum=reviewing,enum=judging,enum=shipping"`
	Branch string `json:"branch" jsonschema:"minLength=1,maxLength=255"`
	SHA    string `json:"sha" jsonschema:"pattern=^[0-9a-f]{40}$"`
	Reason string `json:"reason" jsonschema:"enum=no_origin,enum=remote_ref_missing,enum=auth,enum=network,enum=fetch_failed"`
}

// StaleBaseLine renders a stale_base event as one owner-facing feed
// sentence. An unknown reason reads as the generic fetch_failed phrase.
func StaleBaseLine(e StaleBaseEvent) string {
	var why string
	switch e.Reason {
	case "no_origin":
		why = "origin is missing or is not a git repository"
	case "remote_ref_missing":
		why = "origin has no branch " + e.Branch
	case "auth":
		why = "origin refused the credentials"
	case "network":
		why = "origin could not be reached"
	default:
		why = "git fetch failed"
	}
	sha := e.SHA
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return "Used the last fetched " + e.Branch + " at " + sha + " for " + e.Step + ": " + why + "."
}

// OwnerEditLine renders an owner_edit event as one owner-facing feed
// sentence.
func OwnerEditLine(e OwnerEditEvent) string {
	switch {
	case e.Target == "scenario" && e.Reason != "":
		return "Owner accepted the judge's amendment to scenario " + e.Ref + ": " + e.Reason
	case e.Target == "scenario":
		return "Owner edited scenario " + e.Ref + "."
	case e.Target == "plan_task" && e.Action == "drop":
		return "Owner dropped plan task " + e.Ref + "; later tasks moved up one."
	case e.Target == "plan_task":
		return "Owner edited plan task " + e.Ref + "."
	case e.Target == "plan_file":
		return "Owner set the tasks for " + e.Ref + " to " + e.New + " (was " + e.Old + ")."
	default:
		return "Owner amended the ticket body."
	}
}
