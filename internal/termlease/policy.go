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
	// RequesterRemote is an authenticated remote device (needs the local owner
	// to not hold the writer).
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

// leaseRules is the ordered grant/takeover policy table (plan §4.α.3), walked
// top-down, one row per case. It encodes:
//   - The local controller is NEVER silently evicted, and a local acquire is
//     NEVER refused (local takeover cannot be refused).
//   - A local takeover ALWAYS revokes an incumbent remote writer.
//   - A remote writer requires the local owner to not hold the writer (an
//     explicit local yield = local Release ⇒ HolderNone); otherwise it fails
//     closed with "held locally".
//   - Only one remote writer at a time: a second remote acquire is refused
//     while a remote holds (one input source ever).
var leaseRules = []leaseRule{
	{RequesterLocal, HolderNone, Outcome{ActionGrant, false, "local acquire, no current holder"}},
	{RequesterLocal, HolderLocal, Outcome{ActionGrant, true, "local re-acquire, revoke prior local lease"}},
	{RequesterLocal, HolderRemote, Outcome{ActionGrant, true, "local takeover, revoke remote writer"}},
	{RequesterRemote, HolderNone, Outcome{ActionGrant, false, "remote acquire, no current holder"}},
	{RequesterRemote, HolderLocal, Outcome{ActionRefuse, false, "held locally — remote refused until explicit local yield"}},
	{RequesterRemote, HolderRemote, Outcome{ActionRefuse, false, "already held by another remote writer"}},
}

// Decide evaluates the grant/takeover policy table for a requester against the
// current holder. It walks the ordered rule set top-down and returns the first
// match; an unmatched combination fails closed (refuse). Pure: no side effects,
// no locking — the caller applies the verdict under its own write fence.
func Decide(req Requester, current HolderKind) Outcome {
	for _, r := range leaseRules {
		if r.req == req && r.current == current {
			return r.out
		}
	}
	return Outcome{ActionRefuse, false, "no matching policy rule (fail closed)"}
}
