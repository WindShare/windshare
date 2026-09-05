package icepolicy

import (
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
)

const DefaultCandidateLimit = 64
const MaximumCandidateBytes = 4096
const candidateClassReserve = 2
const minimumReservedCandidateLimit = 12

type CandidateDecision struct {
	Accepted bool
	Reason   string
	Class    string
}
type CandidateBudget struct {
	mu          sync.Mutex
	limit, sent int
	seen        map[string]bool
	classes     map[string]int
}
type candidateIdentity struct {
	key, class string
	dimensions []string
}

func NewCandidateBudget(limit int) *CandidateBudget {
	if limit <= 0 || limit > DefaultCandidateLimit {
		limit = DefaultCandidateLimit
	}
	return &CandidateBudget{limit: limit, seen: make(map[string]bool), classes: make(map[string]int)}
}

// Accept prunes local gathering; it never validates remote signaling.
func (b *CandidateBudget) Accept(candidate string) CandidateDecision {
	return b.accept(candidate, false)
}

// AcceptMapped requires provider provenance, not a guess from srflx SDP.
func (b *CandidateBudget) AcceptMapped(candidate string) CandidateDecision {
	return b.accept(candidate, true)
}

func (b *CandidateBudget) accept(candidate string, mapped bool) CandidateDecision {
	b.mu.Lock()
	defer b.mu.Unlock()
	path, ok := candidatePath(candidate)
	if !ok {
		return CandidateDecision{Reason: "invalid", Class: "unknown"}
	}
	if mapped {
		path.class = "mapped"
		path.dimensions = append(path.dimensions, "mapped")
	}
	result := CandidateDecision{Class: path.class}
	if b.seen[path.key] {
		result.Reason = "duplicate"
		return result
	}
	if b.sent >= b.limit {
		result.Reason = "budget"
		return result
	}
	reserve := 0
	if b.limit >= minimumReservedCandidateLimit {
		for _, other := range []string{"lan", "ipv4", "ipv6", "srflx", "tcp", "mapped"} {
			if !slices.Contains(path.dimensions, other) {
				reserve += max(0, candidateClassReserve-b.classes[other])
			}
		}
	}
	if b.sent >= b.limit-reserve {
		result.Reason = "reserved"
		return result
	}
	b.seen[path.key] = true
	b.sent++
	for _, dimension := range path.dimensions {
		b.classes[dimension]++
	}
	result.Accepted = true
	result.Reason = "accepted"
	return result
}

func candidatePath(candidate string) (candidateIdentity, bool) {
	if len(candidate) > MaximumCandidateBytes {
		return candidateIdentity{}, false
	}
	fields := strings.Fields(strings.TrimPrefix(candidate, "a="))
	if len(fields) < 8 || !strings.HasPrefix(fields[0], "candidate:") || fields[6] != "typ" {
		return candidateIdentity{}, false
	}
	protocol := strings.ToLower(fields[2])
	if protocol != "udp" && protocol != "tcp" {
		return candidateIdentity{}, false
	}
	port, err := strconv.Atoi(fields[5])
	if err != nil || port < 1 || port > 65535 {
		return candidateIdentity{}, false
	}
	address, kind := strings.ToLower(fields[4]), fields[7]
	if kind != "host" && kind != "srflx" && kind != "prflx" && kind != "relay" {
		return candidateIdentity{}, false
	}
	path := candidateClasses(address, kind, protocol)
	tcpType := ""
	for i := 8; i+1 < len(fields); i += 2 {
		if fields[i] == "tcptype" {
			tcpType = fields[i+1]
		}
	}
	// Foundation and priority do not create a distinct connectivity path.
	path.key = strings.Join([]string{fields[1], protocol, address, fields[5], kind, tcpType}, "|")
	return path, true
}
func candidateClasses(address, kind, protocol string) candidateIdentity {
	path := candidateIdentity{class: "unknown"}
	ip := net.ParseIP(address)
	if ip != nil {
		family := "ipv6"
		if ip.To4() != nil {
			family = "ipv4"
		}
		path.class = family
		path.dimensions = append(path.dimensions, family)
	}
	if kind == "host" && (strings.HasSuffix(address, ".local") || (ip != nil && (ip.IsPrivate() || ip.IsLinkLocalUnicast()))) {
		path.class = "lan"
		path.dimensions = append(path.dimensions, "lan")
	}
	if kind == "srflx" || kind == "prflx" {
		path.class = "srflx"
		path.dimensions = append(path.dimensions, "srflx")
	}
	if protocol == "tcp" {
		path.class = "tcp"
		path.dimensions = append(path.dimensions, "tcp")
	}
	return path
}
