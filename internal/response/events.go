package response

// CheckRerunEvent is the check_rerun typed event's payload (messages table,
// event_kind "check_rerun"): check re-ran on sha. #91 writes it and gates on
// its count per sha.
type CheckRerunEvent struct {
	Check CheckName `json:"check"`
	SHA   string    `json:"sha" jsonschema:"pattern=^[0-9a-f]{40}$"`
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
}

// OwnerEditLine renders an owner_edit event as one owner-facing feed
// sentence.
func OwnerEditLine(e OwnerEditEvent) string {
	switch {
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
