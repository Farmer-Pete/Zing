package response

// CheckRerunEvent is the check_rerun typed event's payload (messages table,
// event_kind "check_rerun"): check re-ran on sha. #91 writes it and gates on
// its count per sha.
type CheckRerunEvent struct {
	Check CheckName `json:"check"`
	SHA   string    `json:"sha" jsonschema:"pattern=^[0-9a-f]{40}$"`
}
