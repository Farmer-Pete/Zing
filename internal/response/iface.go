package response

// Response is implemented by every job's response type. Header returns the
// shared job/outcome header embedded in Head.
type Response interface {
	Header() Head
}

// Header returns h itself. Every response type embeds Head anonymously, so
// this one method satisfies Response for all of them. The accessor is named
// Header, not Head, because Head is also the name of the embedded field,
// and a method named Head would collide with it.
func (h Head) Header() Head {
	return h
}

// Compile-time proof that every registered response type satisfies
// Response. One assertion per type in the registry (registry.go).
var (
	_ Response = (*ClassifyResponse)(nil)
	_ Response = (*QuestionResponse)(nil)
	_ Response = (*ReadyResponse)(nil)
	_ Response = (*ChildrenResponse)(nil)
	_ Response = (*NothingToDoResponse)(nil)
	_ Response = (*FindingsResponse)(nil)
	_ Response = (*BuildResponse)(nil)
	_ Response = (*PerimeterResponse)(nil)
	_ Response = (*JudgeResponse)(nil)
	_ Response = (*RespondResponse)(nil)
	_ Response = (*SideResponse)(nil)
	_ Response = (*ErrorResponse)(nil)
)
