package termlease

// HolderKind is the identity class of the current writer-lease holder.
type HolderKind int

const (
	// HolderNone means no writer lease is currently held.
	HolderNone HolderKind = iota
	// HolderLocal means the owner-local loopback path holds the writer.
	HolderLocal
	// HolderRemote means an authenticated remote device session holds it.
	HolderRemote
)

// String renders a HolderKind for audit/logs.
func (h HolderKind) String() string {
	switch h {
	case HolderLocal:
		return "local"
	case HolderRemote:
		return "remote"
	default:
		return "none"
	}
}

// Requester is the identity class asking to acquire the writer lease.
type Requester int

const (
	// RequesterLocal is the owner-local loopback path (never refused).
	RequesterLocal Requester = iota
	// RequesterRemote is an authenticated remote device. Whether it may
	// supersede a current writer is selected by the live takeover policy.
	RequesterRemote
)

// String renders a Requester for audit/logs.
func (r Requester) String() string {
	if r == RequesterRemote {
		return "remote"
	}
	return "local"
}

// Action is the policy verdict.
type Action int

const (
	// ActionRefuse denies the acquire (fail closed).
	ActionRefuse Action = iota
	// ActionGrant admits the acquire.
	ActionGrant
)

// Outcome is one policy-table verdict: whether to grant, whether the incumbent
// lease must be revoked first, and a human-readable reason for audit.
type Outcome struct {
	Action        Action
	RevokeCurrent bool
	Reason        string
}

// Granted reports whether the outcome admits the acquire.
func (o Outcome) Granted() bool { return o.Action == ActionGrant }

// leaseRule is one row of the grant/takeover policy table.
type leaseRule struct {
	req     Requester
	current HolderKind
	out     Outcome
}

// leaseRulesCommon is the shared prefix of both ordered grant/takeover policy
// tables (plan §4.α.3): the four cases whose outcome does not depend on the
// authenticated-remote takeover setting.
var leaseRulesCommon = []leaseRule{
	{RequesterLocal, HolderNone, Outcome{ActionGrant, false, "local acquire, no current holder"}},
	{RequesterLocal, HolderLocal, Outcome{ActionGrant, true, "local re-acquire, revoke prior local lease"}},
	{RequesterLocal, HolderRemote, Outcome{ActionGrant, true, "local takeover, revoke remote writer"}},
	{RequesterRemote, HolderNone, Outcome{ActionGrant, false, "remote acquire, no current holder"}},
}

// leaseRules keeps the opt-out posture for rows five and six: when authenticated
// remote takeover is disabled, a remote requester cannot evict either holder.
// The local controller is therefore never evicted by a remote in this table.
var leaseRules = append(
	append([]leaseRule{}, leaseRulesCommon...),
	leaseRule{RequesterRemote, HolderLocal, Outcome{ActionRefuse, false, "held locally — remote takeover disabled"}},
	leaseRule{RequesterRemote, HolderRemote, Outcome{ActionRefuse, false, "already held by another remote writer"}},
)

// leaseRulesRemoteTakeover keeps rows one through four byte-identical and
// replaces only rows five and six. A valid remote credential has already been
// authorized upstream before this policy is consulted; these grants revoke the
// incumbent lease so the one-live-writer invariant remains intact.
var leaseRulesRemoteTakeover = append(
	append([]leaseRule{}, leaseRulesCommon...),
	leaseRule{RequesterRemote, HolderLocal, Outcome{ActionGrant, true, "authenticated remote takeover of local writer"}},
	leaseRule{RequesterRemote, HolderRemote, Outcome{ActionGrant, true, "authenticated remote takeover of remote writer"}},
)

// Decide evaluates the grant/takeover policy table for a requester against the
// current holder. It walks the ordered rule set top-down and returns the first
// match; an unmatched combination fails closed (refuse). Pure: no side effects,
// no locking — the caller applies the verdict under its own write fence.
func Decide(req Requester, current HolderKind, allowRemoteTakeover bool) Outcome {
	rules := leaseRules
	if allowRemoteTakeover {
		rules = leaseRulesRemoteTakeover
	}
	for _, r := range rules {
		if r.req == req && r.current == current {
			return r.out
		}
	}
	return Outcome{ActionRefuse, false, "no matching policy rule (fail closed)"}
}
