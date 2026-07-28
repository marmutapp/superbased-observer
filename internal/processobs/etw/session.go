package etw

import "fmt"

// DefaultSessionName is the ETW session name this package starts when Options
// leaves SessionName empty.
//
// It is deliberately NOT "NT Kernel Logger": using the manifest provider on our
// own named session is what keeps the capturer from colliding with WPR, xperf,
// Defender or any other tool that holds the singleton kernel logger. See the
// package doc's provider-choice section.
const DefaultSessionName = "SuperBasedObserverNet"

// Default session buffer geometry. ETW buffer sizes are expressed in KB. These
// are modest: the capturer decodes and discards in the callback, so the buffers
// only have to absorb bursts, not hold history.
const (
	defaultBufferSizeKB   = 64
	defaultMinimumBuffers = 8
	defaultMaximumBuffers = 64
)

// Options configures a capture Session. The zero value is not usable — at least
// one handler is required — but every other field has a working default.
//
// Options lives in an untagged file so OS-agnostic wiring in later phases can
// construct it on any platform; only NewSession is Windows-only.
type Options struct {
	// SessionName is the ETW session name. Empty means DefaultSessionName.
	// Sessions are machine-global, so two capturers with the same name
	// conflict.
	SessionName string

	// OnTCP receives every decoded TCP data event, on the ETW processing
	// goroutine. It must not block: ETW buffers are being held while it runs.
	// It must not retain any pointer derived from the callback either — the
	// event is a value type precisely so it can be copied out cheaply.
	OnTCP func(TCPDataEvent)

	// OnUDP receives every decoded UDP datagram event, same contract. Leaving
	// it nil is the norm and the TCP-only default: UDP events are then dropped
	// at the callback and can never reach a byte total. Setting it is an
	// explicit, reviewable act.
	OnUDP func(UDPDatagramEvent)

	// ExcludeIPv6 drops the provider's IPv6 keyword from the capture scope.
	//
	// It defaults to FALSE — IPv6 IS captured — for parity, which is the whole
	// point of this arc. The Linux backend attaches fexit/tcp_sendmsg and
	// fentry/tcp_cleanup_rbuf; both are address-family agnostic and therefore
	// already count IPv4 and IPv6 together. Capturing IPv4 only on Windows
	// would make the Windows total silently omit every IPv6 byte while the
	// Linux number it is compared against includes them — a wrong number with
	// no error anywhere, the exact §0.1 failure mode. IPv6 is also fully
	// decoded here (templates 26/27 and 58/59, covered by the parse tests), so
	// there is nothing to "enable only what is decoded" about.
	//
	// This exists as an escape hatch for a box where the IPv6 event rate is
	// genuinely a problem. Setting it makes the resulting totals NOT comparable
	// to a Linux node, so it should be a deliberate and recorded choice.
	ExcludeIPv6 bool

	// BufferSizeKB, MinimumBuffers and MaximumBuffers override the session
	// buffer geometry. Zero means the package default.
	BufferSizeKB   uint32
	MinimumBuffers uint32
	MaximumBuffers uint32
}

// validate fills defaults and rejects a configuration that would start a
// privileged trace session and then throw every event away.
func (o *Options) validate() error {
	if o.OnTCP == nil && o.OnUDP == nil {
		return fmt.Errorf("etw.Options.validate: %w", ErrNoHandler)
	}
	if o.SessionName == "" {
		o.SessionName = DefaultSessionName
	}
	if o.BufferSizeKB == 0 {
		o.BufferSizeKB = defaultBufferSizeKB
	}
	if o.MinimumBuffers == 0 {
		o.MinimumBuffers = defaultMinimumBuffers
	}
	if o.MaximumBuffers == 0 {
		o.MaximumBuffers = defaultMaximumBuffers
	}
	if o.MaximumBuffers < o.MinimumBuffers {
		o.MaximumBuffers = o.MinimumBuffers
	}
	return nil
}

// keywords is EnableTraceEx2's MatchAnyKeyword for this configuration.
//
// It lives in the untagged file so the default — both families, for Linux
// parity — is pinned by a test on the platform CI actually has. MatchAllKeyword
// stays 0 at the call site; setting it to the OR of both keywords would admit
// only the two dual-tagged failure events.
func (o Options) keywords() uint64 {
	kw := KeywordIPv4
	if !o.ExcludeIPv6 {
		kw |= KeywordIPv6
	}
	return kw
}
