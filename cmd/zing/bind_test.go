package main

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

// testUtunIface is the fake macOS-style tailscale interface name this
// file's interface-scan fixtures reuse (design section 6.14: "tailscale*"
// or "utun*").
const testUtunIface = "utun4"

// fakeCLI builds a tailscaleCLIRunner that returns (out, err) unconditionally,
// the seam design section 12 (Task 11) calls for: "inject a fake
// resolver/CLI seam" so the suite depends on no real tailscale binary.
func fakeCLI(out string, err error) tailscaleCLIRunner {
	return func(context.Context) (string, error) { return out, err }
}

// fakeIfaces builds a tailscaleInterfaceLister returning ifaces unconditionally.
func fakeIfaces(ifaces []tailscaleInterface) tailscaleInterfaceLister {
	return func() ([]tailscaleInterface, error) { return ifaces, nil }
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("ParseAddr(%q): %v", s, err)
	}
	return a
}

// TestResolveTailnetAddr_CLIAddressWinsWhenPresent proves the CLI's answer
// is used, and the interface scan is never consulted, when the CLI returns
// a single CGNAT IPv4 (design section 6.14, 12).
func TestResolveTailnetAddr_CLIAddressWinsWhenPresent(t *testing.T) {
	t.Parallel()

	scanCalled := false
	listIfaces := func() ([]tailscaleInterface, error) {
		scanCalled = true
		return nil, nil
	}

	addr, ok := resolveTailnetAddr(t.Context(), fakeCLI("100.64.1.2\n", nil), listIfaces)
	if !ok {
		t.Fatal("resolveTailnetAddr: ok = false, want true")
	}
	if addr != "100.64.1.2" {
		t.Errorf("addr = %q, want 100.64.1.2", addr)
	}
	if scanCalled {
		t.Error("the interface scan ran even though the CLI answered, want it skipped")
	}
}

// TestResolveTailnetAddr_InterfaceMatchWinsOtherwise proves the interface
// scan is used when the CLI is unavailable, matching a tailscale*/utun*
// interface with one CGNAT IPv4.
func TestResolveTailnetAddr_InterfaceMatchWinsOtherwise(t *testing.T) {
	t.Parallel()

	ifaces := []tailscaleInterface{
		{Name: "eth0", Addrs: []netip.Addr{mustAddr(t, "192.0.2.5")}},
		{Name: testUtunIface, Addrs: []netip.Addr{mustAddr(t, "100.64.9.9"), mustAddr(t, "fe80::1")}},
	}

	addr, ok := resolveTailnetAddr(t.Context(), fakeCLI("", errors.New("no tailscale binary")), fakeIfaces(ifaces))
	if !ok {
		t.Fatal("resolveTailnetAddr: ok = false, want true")
	}
	if addr != "100.64.9.9" {
		t.Errorf("addr = %q, want 100.64.9.9", addr)
	}
}

// TestResolveTailnetAddr_CLIOutputNotSingleCGNATIPv4FallsThrough proves a
// CLI answer that does not parse as one CGNAT IPv4 (multiple lines, a
// non-CGNAT address, garbage) falls through to the interface scan rather
// than being used as-is.
func TestResolveTailnetAddr_CLIOutputNotSingleCGNATIPv4FallsThrough(t *testing.T) {
	t.Parallel()

	ifaces := []tailscaleInterface{
		{Name: "tailscale0", Addrs: []netip.Addr{mustAddr(t, "100.64.3.3")}},
	}

	tests := []string{
		"192.0.2.1",        // not CGNAT
		"100.64.1.1 extra", // more than one token
		"not-an-address",   // unparseable
		"",                 // empty
	}
	for _, out := range tests {
		t.Run(out, func(t *testing.T) {
			t.Parallel()
			addr, ok := resolveTailnetAddr(t.Context(), fakeCLI(out, nil), fakeIfaces(ifaces))
			if !ok {
				t.Fatal("resolveTailnetAddr: ok = false, want true (falls through to the interface scan)")
			}
			if addr != "100.64.3.3" {
				t.Errorf("addr = %q, want 100.64.3.3 (from the interface scan)", addr)
			}
		})
	}
}

// TestResolveTailnetAddr_MissingInterfaceSkips proves a CLI failure plus no
// matching interface skips rather than binding anything (design section
// 6.14: "tailscale interface not found, skipping").
func TestResolveTailnetAddr_MissingInterfaceSkips(t *testing.T) {
	t.Parallel()

	ifaces := []tailscaleInterface{
		{Name: "eth0", Addrs: []netip.Addr{mustAddr(t, "192.0.2.5")}},
	}

	_, ok := resolveTailnetAddr(t.Context(), fakeCLI("", errors.New("no binary")), fakeIfaces(ifaces))
	if ok {
		t.Error("resolveTailnetAddr: ok = true, want false (no matching interface)")
	}
}

// TestResolveTailnetAddr_CompetingCandidatesSkip proves that when the CLI
// does not disambiguate and more than one distinct tailscale/utun CGNAT
// address is found, resolution skips rather than picking one (design
// section 6.14: "ambiguous tailscale address, skipping").
func TestResolveTailnetAddr_CompetingCandidatesSkip(t *testing.T) {
	t.Parallel()

	ifaces := []tailscaleInterface{
		{Name: testUtunIface, Addrs: []netip.Addr{mustAddr(t, "100.64.1.1")}},
		{Name: "tailscale0", Addrs: []netip.Addr{mustAddr(t, "100.64.2.2")}},
	}

	_, ok := resolveTailnetAddr(t.Context(), fakeCLI("", errors.New("no binary")), fakeIfaces(ifaces))
	if ok {
		t.Error("resolveTailnetAddr: ok = true, want false (ambiguous candidates)")
	}
}

// TestResolveBindHosts_MixesLiteralsAndTailscale proves resolveBindHosts
// passes a literal IP through unchanged and appends the resolved tailnet
// address only when resolution succeeds, in order.
func TestResolveBindHosts_MixesLiteralsAndTailscale(t *testing.T) {
	t.Parallel()

	ifaces := []tailscaleInterface{
		{Name: testUtunIface, Addrs: []netip.Addr{mustAddr(t, "100.64.5.5")}},
	}

	got := resolveBindHosts(t.Context(), []string{loopback, bindTokenTailscale}, fakeCLI("", errors.New("no binary")), fakeIfaces(ifaces))
	want := []string{loopback, "100.64.5.5"}
	if len(got) != len(want) {
		t.Fatalf("resolveBindHosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("resolveBindHosts[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestResolveBindHosts_SkipsAnUnresolvedTailscaleEntry proves a
// "tailscale" entry that cannot resolve is simply omitted, not erred,
// leaving only the literal entries.
func TestResolveBindHosts_SkipsAnUnresolvedTailscaleEntry(t *testing.T) {
	t.Parallel()

	got := resolveBindHosts(t.Context(), []string{loopback, bindTokenTailscale},
		fakeCLI("", errors.New("no binary")), fakeIfaces(nil))
	want := []string{loopback}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("resolveBindHosts = %v, want %v", got, want)
	}
}

// TestResolveBindHosts_DedupesAResolvedDuplicate proves resolveBindHosts
// dedupes: an explicit tailnet IP alongside a "tailscale" token that
// resolves to that same address must appear exactly once in the result, in
// first-occurrence order, so listenOnAll (serve.go) never gets asked to bind
// the same host:port twice (the second net.Listen call would otherwise fail
// "address already in use" and crash startup).
func TestResolveBindHosts_DedupesAResolvedDuplicate(t *testing.T) {
	t.Parallel()

	const tailnetIP = "100.64.5.5"
	ifaces := []tailscaleInterface{
		{Name: testUtunIface, Addrs: []netip.Addr{mustAddr(t, tailnetIP)}},
	}

	got := resolveBindHosts(t.Context(), []string{tailnetIP, bindTokenTailscale},
		fakeCLI("", errors.New("no binary")), fakeIfaces(ifaces))
	want := []string{tailnetIP}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("resolveBindHosts = %v, want %v (deduplicated, first occurrence kept)", got, want)
	}
}

// TestResolveBindHosts_DedupesRepeatedLiterals proves the same dedupe
// applies to two identical literal entries, not only the tailscale-token
// case.
func TestResolveBindHosts_DedupesRepeatedLiterals(t *testing.T) {
	t.Parallel()

	got := resolveBindHosts(t.Context(), []string{loopback, loopback}, fakeCLI("", errors.New("no binary")), fakeIfaces(nil))
	want := []string{loopback}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("resolveBindHosts = %v, want %v (deduplicated)", got, want)
	}
}
