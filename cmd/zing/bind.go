// bind.go resolves console.bind into concrete listen addresses (design
// section 6.14): a literal IP unchanged, and "tailscale" resolved
// CLI-first, falling through to a name-and-range interface scan that skips
// rather than guesses on an absent or ambiguous candidate. Every seam here
// (the CLI runner, the interface lister) is a plain function value so
// bind_test.go can drive the resolution logic with a fake CLI and a fake
// interface list, never a real tailscale binary or a real network
// interface.
package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// tailscaleCLITimeout bounds the `tailscale ip -4` call (design section
// 6.14: "it runs under a bounded context (a 2s timeout)").
const tailscaleCLITimeout = 2 * time.Second

// cgnatPrefix is Tailscale's carrier-grade NAT range, 100.64.0.0/10 (design
// section 6.14), the range a resolved tailnet address must fall in.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// tailscaleCLIRunner runs `tailscale ip -4` (or a fake standing in for it)
// and returns its trimmed stdout. The real implementation is
// runTailscaleCLI; tests inject a fake so the suite depends on no real
// binary.
type tailscaleCLIRunner func(ctx context.Context) (string, error)

// tailscaleInterface is a minimal, fake-friendly view of one network
// interface: its name and its bound IP addresses. realTailscaleInterfaces
// is the only place that reads the real net.Interfaces and calls its
// Addrs() method (which resolves by OS interface index and so cannot be
// faked through a plain net.Interface literal); every other caller,
// including every test, goes through this type instead.
type tailscaleInterface struct {
	Name  string
	Addrs []netip.Addr
}

// tailscaleInterfaceLister lists every network interface (or a fake list
// standing in for one). The real implementation is realTailscaleInterfaces.
type tailscaleInterfaceLister func() ([]tailscaleInterface, error)

// runTailscaleCLI runs `tailscale ip -4` under a bounded context and
// returns its trimmed stdout. A missing binary, a non-zero exit, or a
// timeout all report an error, which resolveTailnetAddr treats as "fall
// through to the interface scan" (design section 6.14).
func runTailscaleCLI(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, tailscaleCLITimeout)
	defer cancel()

	var out bytes.Buffer
	cmd := exec.CommandContext(cctx, "tailscale", "ip", "-4")
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("run tailscale ip -4: %w", err)
	}
	return strings.TrimSpace(out.String()), nil
}

// realTailscaleInterfaces lists every real network interface and its bound
// addresses via net.Interfaces, adapted to tailscaleInterface. An interface
// whose Addrs() call fails is skipped rather than failing the whole list,
// so one broken interface cannot block tailnet resolution.
func realTailscaleInterfaces() ([]tailscaleInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("list interfaces: %w", err)
	}

	out := make([]tailscaleInterface, 0, len(ifaces))
	for _, ifc := range ifaces {
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		var ips []netip.Addr
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip, ok := netip.AddrFromSlice(ipNet.IP); ok {
				ips = append(ips, ip.Unmap())
			}
		}
		out = append(out, tailscaleInterface{Name: ifc.Name, Addrs: ips})
	}
	return out, nil
}

// parseCGNATIPv4 reports whether s (the tailscale CLI's trimmed stdout)
// parses as exactly one IPv4 address in cgnatPrefix (design section 6.14:
// "its output is accepted only when it parses as a single IPv4 in
// 100.64.0.0/10").
func parseCGNATIPv4(s string) (addr string, ok bool) {
	fields := strings.Fields(s)
	if len(fields) != 1 {
		return "", false
	}
	a, err := netip.ParseAddr(fields[0])
	if err != nil || !a.Is4() || !cgnatPrefix.Contains(a) {
		return "", false
	}
	return a.String(), true
}

// isTailscaleIfaceName reports whether name matches the design section
// 6.14 interface-name patterns: "tailscale*" or "utun*".
func isTailscaleIfaceName(name string) bool {
	return strings.HasPrefix(name, "tailscale") || strings.HasPrefix(name, "utun")
}

// tailscaleInterfaceCandidates scans ifaces for a tailscale*/utun*
// interface with an IPv4 address in cgnatPrefix, returning every distinct
// address found, sorted (design section 6.14).
func tailscaleInterfaceCandidates(ifaces []tailscaleInterface) []string {
	seen := make(map[string]bool)
	var out []string
	for _, ifc := range ifaces {
		if !isTailscaleIfaceName(ifc.Name) {
			continue
		}
		for _, a := range ifc.Addrs {
			if !a.Is4() || !cgnatPrefix.Contains(a) {
				continue
			}
			s := a.String()
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	sort.Strings(out)
	return out
}

// resolveTailnetAddr resolves the "tailscale" bind token to one concrete
// IPv4 address (design section 6.14): the tailscale CLI's answer when it
// parses as a single CGNAT IPv4, else a scan of runCLI's fallback,
// listIfaces, for a tailscale*/utun* interface with a CGNAT IPv4. It
// returns ok=false, having logged why, when the CLI is unusable and the
// scan finds no candidate ("tailscale interface not found, skipping") or
// more than one distinct candidate with the CLI not having disambiguated
// ("ambiguous tailscale address, skipping").
func resolveTailnetAddr(ctx context.Context, runCLI tailscaleCLIRunner, listIfaces tailscaleInterfaceLister) (addr string, ok bool) {
	if out, err := runCLI(ctx); err != nil {
		slog.Warn("tailscale CLI unavailable, falling back to an interface scan", "err", err)
	} else if a, parsed := parseCGNATIPv4(out); parsed {
		slog.Info("resolved tailnet address via the tailscale CLI", "addr", a)
		return a, true
	} else {
		slog.Warn("tailscale CLI output did not parse as one CGNAT IPv4, falling back to an interface scan", "output", out)
	}

	ifaces, err := listIfaces()
	if err != nil {
		slog.Warn("list network interfaces for tailnet resolution", "err", err)
		return "", false
	}

	switch candidates := tailscaleInterfaceCandidates(ifaces); len(candidates) {
	case 0:
		slog.Info("tailscale interface not found, skipping")
		return "", false
	case 1:
		slog.Info("resolved tailnet address via an interface scan", "addr", candidates[0])
		return candidates[0], true
	default:
		slog.Warn("ambiguous tailscale address, skipping", "candidates", candidates)
		return "", false
	}
}

// bindToken is the literal string every config.Console.Bind entry that
// means "resolve the tailnet address" (design section 6.14).
const bindTokenTailscale = "tailscale"

// resolveBindHosts resolves every entry in tokens to a concrete host
// address (design section 6.14): a literal IP unchanged (config.Load
// already rejected a wildcard at load time), or "tailscale" resolved via
// resolveTailnetAddr and skipped, not erred, when resolution fails. The
// returned hosts are in tokens' order, tailscale-token entries omitted when
// unresolved.
func resolveBindHosts(ctx context.Context, tokens []string, runCLI tailscaleCLIRunner, listIfaces tailscaleInterfaceLister) []string {
	hosts := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		if tok == bindTokenTailscale {
			if addr, ok := resolveTailnetAddr(ctx, runCLI, listIfaces); ok {
				hosts = append(hosts, addr)
			}
			continue
		}
		hosts = append(hosts, tok)
	}
	return hosts
}
