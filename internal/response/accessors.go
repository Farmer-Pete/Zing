package response

// Files returns p's delivered file changes (design section 6.11). p's zero
// value has a nil Delivery.Files, which this returns unchanged: a
// zero-length slice, never a panic.
func Files(p Plan) []FileChange {
	return p.Delivery.Files
}

// Fences returns p's deletions. It reads p.Delivery.Deletions.Items,
// hiding the $.delivery.deletions.deletions nesting the stored JSON shape
// carries (design section 6.11).
func Fences(p Plan) []Fence {
	return p.Delivery.Deletions.Items
}

// Tasks returns p's delivery tasks, in build order (design section 6.11).
func Tasks(p Plan) []Task {
	return p.Delivery.Tasks
}
