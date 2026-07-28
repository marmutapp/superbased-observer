//go:build windows

package etw

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ETW session control + consumption, bound by hand.
//
// golang.org/x/sys/windows exposes ZERO ETW surface (E0 finding 1): no
// StartTrace, ControlTrace, EnableTraceEx2, OpenTrace, ProcessTrace, CloseTrace,
// and none of the structs. Everything below is therefore bound the same way
// poll/procmetrics_windows.go binds psapi/kernel32 — windows.NewLazySystemDLL,
// one block of lazy proc vars, `r, _, _ := proc.Call(...)`, unsafe.Sizeof for
// cb/size fields — just a lot more of it. No CGO, no third-party dependency.
var (
	modadvapi32 = windows.NewLazySystemDLL("advapi32.dll")

	procStartTraceW    = modadvapi32.NewProc("StartTraceW")
	procControlTraceW  = modadvapi32.NewProc("ControlTraceW")
	procEnableTraceEx2 = modadvapi32.NewProc("EnableTraceEx2")
	procOpenTraceW     = modadvapi32.NewProc("OpenTraceW")
	procProcessTrace   = modadvapi32.NewProc("ProcessTrace")
	procCloseTrace     = modadvapi32.NewProc("CloseTrace")

	etwProcs = []*windows.LazyProc{
		procStartTraceW, procControlTraceW, procEnableTraceEx2,
		procOpenTraceW, procProcessTrace, procCloseTrace,
	}

	procsOnce sync.Once
	procsErr  error
)

// ensureProcs resolves every advapi32 ETW entry point once, up front.
//
// (*windows.LazyProc).Call PANICS if the symbol cannot be found, and CLAUDE.md
// forbids panic() in library code. These six exist on every Windows this ships
// to, so the guard should never fire — which is precisely why it costs nothing
// and why an elevated capturer should not be the thing that discovers
// otherwise. Every exported entry point that reaches a Call goes through here
// first, so no Call in this file can panic.
func ensureProcs() error {
	procsOnce.Do(func() {
		for _, p := range etwProcs {
			if err := p.Find(); err != nil {
				procsErr = fmt.Errorf("etw: advapi32.dll!%s: %w: %w", p.Name, err, ErrProcUnavailable)
				return
			}
		}
	})
	return procsErr
}

// 64-bit ABI assertion. Every struct size pinned below is the 64-bit layout,
// and TRACEHANDLE (always ULONG64) is passed by value through uintptr, which is
// only lossless when uintptr is 8 bytes. The shipped Windows artifacts are
// win32-x64 (and arm64), and `GOOS=windows GOARCH=amd64 go build ./...` is the
// CI gate, so a 32-bit Windows build failing loudly here is the correct
// outcome — far better than silently mis-laying every struct.
const (
	_ uintptr = unsafe.Sizeof(uintptr(0)) - 8
	_ uintptr = 8 - unsafe.Sizeof(uintptr(0))
)

// Win32 / ETW constants. Values from evntrace.h and evntcons.h.
const (
	// EVENT_TRACE_REAL_TIME_MODE — deliver events to a live consumer rather
	// than an .etl file.
	eventTraceRealTimeMode uint32 = 0x00000100
	// WNODE_FLAG_TRACED_GUID — required in EVENT_TRACE_PROPERTIES.Wnode.Flags.
	wnodeFlagTracedGUID uint32 = 0x00020000

	// ControlTrace control codes.
	eventTraceControlStop uint32 = 1

	// EnableTraceEx2 control codes.
	eventControlCodeEnableProvider uint32 = 1

	// ENABLE_TRACE_PARAMETERS_VERSION_2.
	enableTraceParametersVersion2 uint32 = 2

	// TRACE_LEVEL_VERBOSE — the provider's data events are Informational, so
	// Verbose admits them with room to spare.
	traceLevelVerbose uint32 = 5

	// PROCESS_TRACE_MODE_* for EVENT_TRACE_LOGFILEW.ProcessTraceMode.
	processTraceModeRealTime    uint32 = 0x00000100
	processTraceModeEventRecord uint32 = 0x10000000

	// INVALID_PROCESSTRACE_HANDLE on 64-bit Windows.
	invalidProcessTraceHandle uint64 = 0xFFFFFFFFFFFFFFFF
)

// sessionNameMaxChars bounds the UTF-16 name buffer StartTraceW copies the
// session name into. EVENT_TRACE_PROPERTIES requires trailing space for the
// logger (and optionally log-file) names within the same allocation.
const sessionNameMaxChars = 512

// ---------------------------------------------------------------------------
// Struct bindings.
//
// Each struct below is pinned by a TWO-SIDED compile-time size assertion rather
// than a test. CI has no Windows runner and `GOOS=windows go build ./...` — the
// only Windows signal — does not compile _test.go files, so a size test would
// never execute anywhere. A wrong size in either direction makes one of the two
// constants a negative untyped value, which does not fit uintptr, which is a
// compile error caught by the existing cross-compile gate.
// ---------------------------------------------------------------------------

// wnodeHeader mirrors WNODE_HEADER. 48 bytes on 64-bit — verified empirically
// by the E0 spike on the operator's Windows 11 box AND against the documented
// ABI at
// https://learn.microsoft.com/en-us/windows/win32/etw/wnode-header
// The two unions are bound at their widest member (ULONG64 / LARGE_INTEGER).
type wnodeHeader struct {
	BufferSize        uint32       // 0
	ProviderID        uint32       // 4
	HistoricalContext uint64       // 8   union{ULONG64 HistoricalContext; struct{ULONG Version; ULONG Linkage}}
	TimeStamp         int64        // 16  union{ULONG CountLost; HANDLE KernelHandle; LARGE_INTEGER TimeStamp}
	Guid              windows.GUID // 24
	ClientContext     uint32       // 40
	Flags             uint32       // 44
} // 48

const (
	_ uintptr = unsafe.Sizeof(wnodeHeader{}) - 48
	_ uintptr = 48 - unsafe.Sizeof(wnodeHeader{})
)

// eventTraceProperties mirrors EVENT_TRACE_PROPERTIES. 120 bytes on 64-bit —
// verified empirically by the E0 spike AND against
// https://learn.microsoft.com/en-us/windows/win32/api/evntrace/ns-evntrace-event_trace_properties
type eventTraceProperties struct {
	Wnode               wnodeHeader    // 0   (48)
	BufferSize          uint32         // 48
	MinimumBuffers      uint32         // 52
	MaximumBuffers      uint32         // 56
	MaximumFileSize     uint32         // 60
	LogFileMode         uint32         // 64
	FlushTimer          uint32         // 68
	EnableFlags         uint32         // 72
	AgeLimit            int32          // 76  union{LONG AgeLimit; LONG FlushThreshold}
	NumberOfBuffers     uint32         // 80
	FreeBuffers         uint32         // 84
	EventsLost          uint32         // 88
	BuffersWritten      uint32         // 92
	LogBuffersLost      uint32         // 96
	RealTimeBuffersLost uint32         // 100
	LoggerThreadID      windows.Handle // 104 (8)
	LogFileNameOffset   uint32         // 112
	LoggerNameOffset    uint32         // 116
} // 120

const (
	_ uintptr = unsafe.Sizeof(eventTraceProperties{}) - 120
	_ uintptr = 120 - unsafe.Sizeof(eventTraceProperties{})
)

// tracePropertiesBuffer is EVENT_TRACE_PROPERTIES plus the trailing name space
// StartTraceW/ControlTraceW copy the session name into. LoggerNameOffset must
// point at that space, so the offset is pinned too — a compiler that inserted
// padding between the two fields would silently give StartTraceW a bad offset.
type tracePropertiesBuffer struct {
	Props eventTraceProperties
	Names [2 * sessionNameMaxChars]uint16
}

const (
	_ uintptr = unsafe.Offsetof(tracePropertiesBuffer{}.Names) - 120
	_ uintptr = 120 - unsafe.Offsetof(tracePropertiesBuffer{}.Names)
)

// enableTraceParameters mirrors ENABLE_TRACE_PARAMETERS (version 2). 48 bytes
// on 64-bit: GUID is 4-aligned so SourceId sits at 12 and ends at 28, the
// pointer realigns to 32, FilterDescCount at 40, tail padding to 48. Documented
// at
// https://learn.microsoft.com/en-us/windows/win32/api/evntrace/ns-evntrace-enable_trace_parameters
type enableTraceParameters struct {
	Version          uint32       // 0
	EnableProperty   uint32       // 4
	ControlFlags     uint32       // 8
	SourceID         windows.GUID // 12 (16)
	EnableFilterDesc uintptr      // 32 (8)  PEVENT_FILTER_DESCRIPTOR
	FilterDescCount  uint32       // 40
} // 48 (4 bytes tail padding)

const (
	_ uintptr = unsafe.Sizeof(enableTraceParameters{}) - 48
	_ uintptr = 48 - unsafe.Sizeof(enableTraceParameters{})
)

// eventDescriptor mirrors EVENT_DESCRIPTOR. 16 bytes: the six leading fields
// pack into 8, then Keyword realigns to 8. Documented at
// https://learn.microsoft.com/en-us/windows/win32/api/evntprov/ns-evntprov-event_descriptor
type eventDescriptor struct {
	ID      uint16 // 0
	Version uint8  // 2
	Channel uint8  // 3
	Level   uint8  // 4
	Opcode  uint8  // 5
	Task    uint16 // 6
	Keyword uint64 // 8
} // 16

const (
	_ uintptr = unsafe.Sizeof(eventDescriptor{}) - 16
	_ uintptr = 16 - unsafe.Sizeof(eventDescriptor{})
)

// etwBufferContext mirrors ETW_BUFFER_CONTEXT. 4 bytes; the leading union is
// bound at its USHORT ProcessorIndex member. Documented at
// https://learn.microsoft.com/en-us/windows/win32/api/evntrace/ns-evntrace-etw_buffer_context
type etwBufferContext struct {
	ProcessorIndex uint16 // 0  union{struct{UCHAR ProcessorNumber; UCHAR Alignment}; USHORT ProcessorIndex}
	LoggerID       uint16 // 2
} // 4

const (
	_ uintptr = unsafe.Sizeof(etwBufferContext{}) - 4
	_ uintptr = 4 - unsafe.Sizeof(etwBufferContext{})
)

// eventHeader mirrors EVENT_HEADER. 80 bytes on 64-bit. Documented at
// https://learn.microsoft.com/en-us/windows/win32/api/evntcons/ns-evntcons-event_header
//
// ProcessId is bound but NEVER read for attribution: for kernel network events
// it is the interrupt-time context, not the socket owner. See the package doc.
type eventHeader struct {
	Size            uint16          // 0
	HeaderType      uint16          // 2
	Flags           uint16          // 4
	EventProperty   uint16          // 6
	ThreadID        uint32          // 8
	ProcessID       uint32          // 12
	TimeStamp       int64           // 16
	ProviderID      windows.GUID    // 24 (16)
	EventDescriptor eventDescriptor // 40 (16)
	ProcessorTime   uint64          // 56 union{struct{ULONG KernelTime; ULONG UserTime}; ULONG64 ProcessorTime}
	ActivityID      windows.GUID    // 64 (16)
} // 80

const (
	_ uintptr = unsafe.Sizeof(eventHeader{}) - 80
	_ uintptr = 80 - unsafe.Sizeof(eventHeader{})
)

// eventRecord mirrors EVENT_RECORD. 112 bytes on 64-bit. Documented at
// https://learn.microsoft.com/en-us/windows/win32/api/evntcons/ns-evntcons-event_record
//
// UserData is typed unsafe.Pointer because it is dereferenced (that is the
// event payload) and a uintptr-to-Pointer conversion at the use site is exactly
// what vet's unsafeptr check forbids. ExtendedData stays uintptr — it is never
// dereferenced here — and UserContext stays uintptr because it is not a pointer
// at all: it carries our integer session key. None of this memory is Go heap
// memory, and none of it may outlive the callback.
type eventRecord struct {
	EventHeader       eventHeader      // 0   (80)
	BufferContext     etwBufferContext // 80  (4)
	ExtendedDataCount uint16           // 84
	UserDataLength    uint16           // 86
	ExtendedData      uintptr          // 88  PEVENT_HEADER_EXTENDED_DATA_ITEM
	UserData          unsafe.Pointer   // 96  PVOID
	UserContext       uintptr          // 104 PVOID
} // 112

const (
	_ uintptr = unsafe.Sizeof(eventRecord{}) - 112
	_ uintptr = 112 - unsafe.Sizeof(eventRecord{})
)

// eventTraceHeader mirrors EVENT_TRACE_HEADER. 48 bytes. Bound only because it
// is embedded in EVENT_TRACE, which is embedded in EVENT_TRACE_LOGFILEW — its
// size participates in that struct's total, so it needs pinning even though
// nothing reads it. Documented at
// https://learn.microsoft.com/en-us/windows/win32/api/evntrace/ns-evntrace-event_trace_header
type eventTraceHeader struct {
	Size           uint16       // 0
	FieldTypeFlags uint16       // 2  union{USHORT FieldTypeFlags; struct{UCHAR HeaderType; UCHAR MarkerFlags}}
	Version        uint32       // 4  union{ULONG Version; struct Class}
	ThreadID       uint32       // 8
	ProcessID      uint32       // 12
	TimeStamp      int64        // 16
	Guid           windows.GUID // 24 (16) union{GUID Guid; ULONGLONG GuidPtr}
	ProcessorTime  uint64       // 40 union{struct{KernelTime,UserTime}; ULONG64 ProcessorTime; struct{ClientContext,Flags}}
} // 48

const (
	_ uintptr = unsafe.Sizeof(eventTraceHeader{}) - 48
	_ uintptr = 48 - unsafe.Sizeof(eventTraceHeader{})
)

// eventTrace mirrors EVENT_TRACE. 88 bytes on 64-bit. Documented at
// https://learn.microsoft.com/en-us/windows/win32/api/evntrace/ns-evntrace-event_trace
type eventTrace struct {
	Header           eventTraceHeader // 0  (48)
	InstanceID       uint32           // 48
	ParentInstanceID uint32           // 52
	ParentGuid       windows.GUID     // 56 (16)
	MofData          uintptr          // 72 (8)
	MofLength        uint32           // 80
	ClientContext    uint32           // 84 union{ULONG ClientContext; ETW_BUFFER_CONTEXT BufferContext}
} // 88

const (
	_ uintptr = unsafe.Sizeof(eventTrace{}) - 88
	_ uintptr = 88 - unsafe.Sizeof(eventTrace{})
)

// traceLogfileHeader mirrors TRACE_LOGFILE_HEADER (the user-mode variant, with
// LoggerName/LogFileName pointers and a TIME_ZONE_INFORMATION). 280 bytes on
// 64-bit: TimeZone is 172 bytes at offset 72, ending at 244, so BootTime
// realigns to 248. Documented at
// https://learn.microsoft.com/en-us/windows/win32/api/evntrace/ns-evntrace-trace_logfile_header
//
// windows.Timezoneinformation is x/sys's binding of TIME_ZONE_INFORMATION; its
// contribution is pinned by this struct's own assertion.
type traceLogfileHeader struct {
	BufferSize         uint32                      // 0
	Version            uint32                      // 4   union{ULONG Version; struct VersionDetail}
	ProviderVersion    uint32                      // 8
	NumberOfProcessors uint32                      // 12
	EndTime            int64                       // 16
	TimerResolution    uint32                      // 24
	MaximumFileSize    uint32                      // 28
	LogFileMode        uint32                      // 32
	BuffersWritten     uint32                      // 36
	LogInstanceGuid    windows.GUID                // 40  (16) union with {StartBuffers,PointerSize,EventsLost,CpuSpeedInMHz}
	LoggerName         uintptr                     // 56  (8)  LPWSTR — "do not use"
	LogFileName        uintptr                     // 64  (8)  LPWSTR — "do not use"
	TimeZone           windows.Timezoneinformation // 72  (172)
	BootTime           int64                       // 248
	PerfFreq           int64                       // 256
	StartTime          int64                       // 264
	ReservedFlags      uint32                      // 272
	BuffersLost        uint32                      // 276
} // 280

const (
	_ uintptr = unsafe.Sizeof(traceLogfileHeader{}) - 280
	_ uintptr = 280 - unsafe.Sizeof(traceLogfileHeader{})
)

// eventTraceLogfileW mirrors EVENT_TRACE_LOGFILEW. 448 bytes on 64-bit:
// CurrentEvent (88) at 32, LogfileHeader (280) at 120, BufferCallback at 400,
// then two 4-byte gaps ahead of the 8-aligned EventRecordCallback (424) and
// Context (440). Documented at
// https://learn.microsoft.com/en-us/windows/win32/api/evntrace/ns-evntrace-event_trace_logfilea
// (the W variant differs only in the two string pointers' character type).
type eventTraceLogfileW struct {
	LogFileName         *uint16            // 0
	LoggerName          *uint16            // 8
	CurrentTime         int64              // 16
	BuffersRead         uint32             // 24
	ProcessTraceMode    uint32             // 28  union{ULONG LogFileMode; ULONG ProcessTraceMode}
	CurrentEvent        eventTrace         // 32  (88)
	LogfileHeader       traceLogfileHeader // 120 (280)
	BufferCallback      uintptr            // 400 PEVENT_TRACE_BUFFER_CALLBACKW
	BufferSize          uint32             // 408
	Filled              uint32             // 412
	EventsLost          uint32             // 416
	EventRecordCallback uintptr            // 424 union{PEVENT_CALLBACK; PEVENT_RECORD_CALLBACK}
	IsKernelTrace       uint32             // 432
	Context             uintptr            // 440 PVOID
} // 448

const (
	_ uintptr = unsafe.Sizeof(eventTraceLogfileW{}) - 448
	_ uintptr = 448 - unsafe.Sizeof(eventTraceLogfileW{})
)

// ---------------------------------------------------------------------------
// Callback plumbing.
// ---------------------------------------------------------------------------

// Windows hands the callback an opaque PVOID context, which may not be a Go
// pointer (it is stored in memory Go does not own). So sessions are registered
// in a package-level table keyed by an integer token, and the token is what
// travels through EVENT_TRACE_LOGFILEW.Context / EVENT_RECORD.UserContext.
var (
	sessionsMu   sync.RWMutex
	liveSessions = map[uintptr]*Session{}
	nextCtxKey   uintptr

	callbackOnce sync.Once
	callbackAddr uintptr
)

// eventRecordCallbackAddr lazily creates the single syscall.NewCallback
// trampoline. NewCallback allocations are process-lifetime, so one is created
// for the package rather than one per session.
func eventRecordCallbackAddr() uintptr {
	callbackOnce.Do(func() {
		callbackAddr = syscall.NewCallback(eventRecordCallback)
	})
	return callbackAddr
}

// eventRecordCallback is the PEVENT_RECORD_CALLBACK ETW invokes for every
// event. It runs on an OS thread owned by ProcessTrace, so it does the minimum:
// look up the session, decode, hand the value to the caller's handler, return.
// It never returns an error (the ABI is void) and never panics.
func eventRecordCallback(rec *eventRecord) uintptr {
	if rec == nil {
		return 0
	}
	sessionsMu.RLock()
	s := liveSessions[rec.UserContext]
	sessionsMu.RUnlock()
	if s != nil {
		s.dispatch(rec)
	}
	return 0
}

// ---------------------------------------------------------------------------
// Session.
// ---------------------------------------------------------------------------

// Session is one real-time ETW trace session consuming
// Microsoft-Windows-Kernel-Network. Create it with NewSession, pump it with
// Process on a dedicated goroutine, and release it with Close.
type Session struct {
	opts   Options
	ctxKey uintptr

	providerGUID windows.GUID

	// handles owns StartTraceW's controller handle and OpenTraceW's consumer
	// handle. It is a separate atomic-backed type, in an untagged file, because
	// Process (one goroutine) reads the consumer handle while Close (another)
	// clears it — see sessionHandles for the full reasoning and handles_test.go
	// for the race coverage CI can actually run.
	handles sessionHandles

	closeOnce sync.Once

	// Counters, exported through Stats. Written from the ETW callback thread
	// and read by the owner, hence atomic.
	decoded            atomic.Int64
	dropped            atomic.Int64
	ignored            atomic.Int64
	unsupportedVersion atomic.Int64
}

// Stats is a point-in-time snapshot of a Session's decode counters. It is a
// diagnostic surface, not a data path: byte accounting happens in the handlers.
type Stats struct {
	// Decoded is the number of events handed to a handler.
	Decoded int64
	// Dropped is the number of Kernel-Network data events that failed to
	// decode (short or unexpected payload). A non-zero value here means the
	// payload layout assumptions need re-checking against the live provider.
	Dropped int64
	// Ignored is the number of events classified as not-a-data-event, plus UDP
	// events dropped because no OnUDP handler was configured.
	Ignored int64
	// UnsupportedVersion is the number of data events refused because their
	// EVENT_DESCRIPTOR.Version is not the one this package's layout table
	// describes. It is broken out from Dropped because it means something
	// specific and actionable: Windows shipped a new template and this
	// package's offsets may no longer apply. A non-zero value is the signal
	// that keeps a version bump from becoming silently wrong byte totals.
	UnsupportedVersion int64
}

// IsElevated reports whether the current process token is elevated, so a caller
// can pre-flight before paying for a StartTraceW that would fail with
// ERROR_ACCESS_DENIED.
//
// A false result is ADVISORY, not authoritative: membership of the Performance
// Log Users group (or running as LocalSystem/LocalService) also permits session
// control without the token being "elevated" in the UAC sense. NewSession's
// ErrNeedsElevation is the real answer; this is only a cheap hint.
//
// It uses GetCurrentProcessToken, which returns a TOKEN_QUERY pseudo-handle —
// no OpenProcessToken, and nothing to close. The error result is always nil on
// Windows; it exists so the signature matches the non-Windows stub, where the
// honest answer is "could not check".
func IsElevated() (bool, error) {
	return windows.GetCurrentProcessToken().IsElevated(), nil
}

// NewSession starts a real-time ETW session and enables the Kernel-Network
// provider on it. It does not consume anything yet — call Process for that.
//
// A non-elevated caller gets ErrNeedsElevation (wrapped), never a crash: ETW
// session control requires Administrator, the Performance Log Users group, or a
// LocalSystem/LocalService service.
//
// If a session of the same name is already running — typically ours, left
// behind by a killed process — NewSession stops it once and retries. A second
// ERROR_ALREADY_EXISTS is reported as ErrSessionExists rather than looping.
func NewSession(opts Options) (*Session, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := ensureProcs(); err != nil {
		return nil, fmt.Errorf("etw.NewSession: %w", err)
	}
	guid, err := windows.GUIDFromString(KernelNetworkProviderGUID)
	if err != nil {
		return nil, fmt.Errorf("etw.NewSession: parsing provider guid: %w", err)
	}

	s := &Session{opts: opts, providerGUID: guid}

	handle, err := startTrace(opts)
	if err != nil {
		return nil, err
	}
	s.handles.setTrace(handle)

	if err := s.enableProvider(); err != nil {
		_ = stopTrace(s.handles.takeTrace(), opts.SessionName)
		return nil, err
	}

	s.register()
	if err := s.openTrace(); err != nil {
		s.unregister()
		_ = stopTrace(s.handles.takeTrace(), opts.SessionName)
		return nil, err
	}
	return s, nil
}

// register installs the session in the callback lookup table and assigns the
// token that travels through EVENT_TRACE_LOGFILEW.Context.
func (s *Session) register() {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	nextCtxKey++
	s.ctxKey = nextCtxKey
	liveSessions[s.ctxKey] = s
}

// unregister removes the session from the callback lookup table. After this the
// callback becomes a no-op for any in-flight event carrying the stale token.
func (s *Session) unregister() {
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	delete(liveSessions, s.ctxKey)
}

// errStaleSession marks the ERROR_ALREADY_EXISTS case so startTrace can retry
// exactly once without re-inspecting a Win32 code at the call site. It stays
// unexported: callers see ErrSessionExists, which is the outcome that survives
// the retry.
var errStaleSession = errors.New("etw: trace session already exists")

// startTrace issues StartTraceW with a correctly sized EVENT_TRACE_PROPERTIES,
// translating the two failures that matter into typed sentinels.
func startTrace(opts Options) (uint64, error) {
	handle, err := startTraceOnce(opts)
	if err == nil {
		return handle, nil
	}
	if !errors.Is(err, errStaleSession) {
		return 0, err
	}
	// A same-named session survives its creator; reclaim it once.
	_ = stopTrace(0, opts.SessionName)
	handle, err = startTraceOnce(opts)
	if err != nil {
		if errors.Is(err, errStaleSession) {
			return 0, fmt.Errorf("etw.startTrace: session %q: %w", opts.SessionName, ErrSessionExists)
		}
		return 0, err
	}
	return handle, nil
}

// startTraceOnce performs a single StartTraceW attempt.
func startTraceOnce(opts Options) (uint64, error) {
	namePtr, err := windows.UTF16PtrFromString(opts.SessionName)
	if err != nil {
		return 0, fmt.Errorf("etw.startTrace: session name: %w", err)
	}

	buf := newTracePropertiesBuffer()
	buf.Props.BufferSize = opts.BufferSizeKB
	buf.Props.MinimumBuffers = opts.MinimumBuffers
	buf.Props.MaximumBuffers = opts.MaximumBuffers
	buf.Props.LogFileMode = eventTraceRealTimeMode

	var handle uint64
	r, _, _ := procStartTraceW.Call(
		uintptr(unsafe.Pointer(&handle)),
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(buf)),
	)
	switch syscall.Errno(r) {
	case 0:
		return handle, nil
	case windows.ERROR_ACCESS_DENIED:
		return 0, fmt.Errorf("etw.startTrace: session %q: %w", opts.SessionName, ErrNeedsElevation)
	case windows.ERROR_ALREADY_EXISTS:
		return 0, fmt.Errorf("etw.startTrace: session %q: %w", opts.SessionName, errStaleSession)
	default:
		return 0, fmt.Errorf("etw.startTrace: StartTraceW(%q): %w", opts.SessionName, syscall.Errno(r))
	}
}

// newTracePropertiesBuffer allocates an EVENT_TRACE_PROPERTIES with its
// trailing name space and fills the fields every call must set: the total
// allocation size, WNODE_FLAG_TRACED_GUID, and the offset StartTraceW copies
// the session name to. LogFileNameOffset stays 0 — real-time sessions have no
// log file.
func newTracePropertiesBuffer() *tracePropertiesBuffer {
	buf := &tracePropertiesBuffer{}
	buf.Props.Wnode.BufferSize = uint32(unsafe.Sizeof(*buf))
	buf.Props.Wnode.Flags = wnodeFlagTracedGUID
	buf.Props.LoggerNameOffset = uint32(unsafe.Offsetof(buf.Names))
	return buf
}

// stopTrace issues ControlTraceW(EVENT_TRACE_CONTROL_STOP). Either a handle or
// a name identifies the session; passing 0 for the handle and a name is how a
// stale session left by a dead process is reclaimed.
func stopTrace(handle uint64, name string) error {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("etw.stopTrace: session name: %w", err)
	}
	buf := newTracePropertiesBuffer()
	r, _, _ := procControlTraceW.Call(
		uintptr(handle),
		uintptr(unsafe.Pointer(namePtr)),
		uintptr(unsafe.Pointer(buf)),
		uintptr(eventTraceControlStop),
	)
	if errno := syscall.Errno(r); errno != 0 {
		return fmt.Errorf("etw.stopTrace: ControlTraceW(%q): %w", name, errno)
	}
	return nil
}

// enableProvider issues EnableTraceEx2 for Microsoft-Windows-Kernel-Network on
// this session, with MatchAnyKeyword set to the address families the caller
// asked for and MatchAllKeyword left at 0 (setting it to the OR of both
// keywords would admit only the two dual-tagged failure events).
func (s *Session) enableProvider() error {
	params := enableTraceParameters{Version: enableTraceParametersVersion2}
	r, _, _ := procEnableTraceEx2.Call(
		uintptr(s.handles.trace()),
		uintptr(unsafe.Pointer(&s.providerGUID)),
		uintptr(eventControlCodeEnableProvider),
		uintptr(traceLevelVerbose),
		uintptr(s.opts.keywords()),
		0, // MatchAllKeyword
		0, // Timeout: 0 = asynchronous
		uintptr(unsafe.Pointer(&params)),
	)
	switch syscall.Errno(r) {
	case 0:
		return nil
	case windows.ERROR_ACCESS_DENIED:
		return fmt.Errorf("etw.enableProvider: %w", ErrNeedsElevation)
	default:
		return fmt.Errorf("etw.enableProvider: EnableTraceEx2: %w", syscall.Errno(r))
	}
}

// openTrace issues OpenTraceW against our own live session, in EVENT_RECORD
// mode so the callback receives the modern record shape.
//
// The failure branch is written the way EVERY LazyProc.Call site in this
// package must be: the call's own documented failure sentinel
// (INVALID_PROCESSTRACE_HANDLE) decides success, and the errno is consulted
// only afterwards, only when it is non-zero. A `lastErr == nil` test here would
// be dead code — Call boxes a syscall.Errno in a non-nil interface even when
// GetLastError returned 0 — and rendering that zero errno would print "The
// operation completed successfully" as the reason a capture failed. See
// errnoFromCall.
func (s *Session) openTrace() error {
	namePtr, err := windows.UTF16PtrFromString(s.opts.SessionName)
	if err != nil {
		return fmt.Errorf("etw.openTrace: session name: %w", err)
	}
	logfile := eventTraceLogfileW{
		LoggerName:          namePtr,
		ProcessTraceMode:    processTraceModeRealTime | processTraceModeEventRecord,
		EventRecordCallback: eventRecordCallbackAddr(),
		Context:             s.ctxKey,
	}
	r, _, callErr := procOpenTraceW.Call(uintptr(unsafe.Pointer(&logfile)))
	if uint64(r) != invalidProcessTraceHandle {
		s.handles.setConsume(uint64(r))
		return nil
	}
	if errno, ok := errnoFromCall(callErr); ok {
		return fmt.Errorf("etw.openTrace: OpenTraceW(%q): %w", s.opts.SessionName, errno)
	}
	return fmt.Errorf("etw.openTrace: OpenTraceW(%q) returned INVALID_PROCESSTRACE_HANDLE and GetLastError reported no error", s.opts.SessionName)
}

// Process pumps the session, invoking the configured handlers for every decoded
// event. It BLOCKS until Close is called (or the session ends), so callers run
// it on its own goroutine — that is a property of ProcessTrace, not a choice.
//
// A clean shutdown returns nil: CloseTrace makes ProcessTrace return
// ERROR_SUCCESS or ERROR_CTX_CLOSE_PENDING, and both mean "stopped as asked".
//
// Racing Close is legal by construction, and both orderings resolve to a clean
// nil rather than a bogus failure. If Close won the race the consumer handle is
// already 0 and closingDown is true, so there is nothing to pump; if Close
// lands mid-ProcessTrace, an ERROR_INVALID_HANDLE is the documented
// consequence of the handle we were asked to close being closed, not a capture
// failure. Nothing else is swallowed.
func (s *Session) Process() error {
	if err := ensureProcs(); err != nil {
		return fmt.Errorf("etw.Process: %w", err)
	}
	handle := s.handles.consume()
	if handle == 0 {
		// Close ran first. beginClose publishes closing BEFORE clearing the
		// handle, so this is unambiguous.
		return nil
	}
	r, _, _ := procProcessTrace.Call(
		uintptr(unsafe.Pointer(&handle)),
		1, // HandleCount
		0, // StartTime  (NULL)
		0, // EndTime    (NULL)
	)
	switch errno := syscall.Errno(r); errno {
	case 0, windows.ERROR_CTX_CLOSE_PENDING:
		return nil
	case windows.ERROR_INVALID_HANDLE:
		if s.handles.closingDown() {
			return nil
		}
		return fmt.Errorf("etw.Process: ProcessTrace: %w", errno)
	default:
		return fmt.Errorf("etw.Process: ProcessTrace: %w", errno)
	}
}

// Close stops consuming, stops the trace session, and deregisters the callback
// token. It is safe to call more than once and is what unblocks Process.
func (s *Session) Close() error {
	if err := ensureProcs(); err != nil {
		return fmt.Errorf("etw.Close: %w", err)
	}
	var err error
	s.closeOnce.Do(func() {
		if handle := s.handles.beginClose(); handle != 0 {
			r, _, _ := procCloseTrace.Call(uintptr(handle))
			if errno := syscall.Errno(r); errno != 0 && errno != windows.ERROR_CTX_CLOSE_PENDING {
				err = fmt.Errorf("etw.Close: CloseTrace: %w", errno)
			}
		}
		if stopErr := stopTrace(s.handles.takeTrace(), s.opts.SessionName); stopErr != nil && err == nil {
			err = stopErr
		}
		s.unregister()
	})
	return err
}

// Stats snapshots the decode counters.
func (s *Session) Stats() Stats {
	return Stats{
		Decoded:            s.decoded.Load(),
		Dropped:            s.dropped.Load(),
		Ignored:            s.ignored.Load(),
		UnsupportedVersion: s.unsupportedVersion.Load(),
	}
}

// dispatch decodes one EVENT_RECORD and hands it to the matching handler.
//
// Two guards matter here. First, the provider id is re-checked: a session only
// has Kernel-Network enabled, but ETW also delivers its own bookkeeping events,
// and Kernel-Network's ids overlap numerically with every other provider's.
// Second, TCP and UDP take structurally separate paths into separate types —
// there is no branch in which a UDP payload can reach OnTCP.
func (s *Session) dispatch(rec *eventRecord) {
	if rec.EventHeader.ProviderID != s.providerGUID {
		s.ignored.Add(1)
		return
	}
	id := rec.EventHeader.EventDescriptor.ID
	version := rec.EventHeader.EventDescriptor.Version

	switch Classify(id) {
	case ClassTCPData:
		if s.opts.OnTCP == nil {
			s.ignored.Add(1)
			return
		}
		ev, err := DecodeTCPData(id, version, userData(rec))
		if err != nil {
			s.countDecodeFailure(err)
			return
		}
		s.decoded.Add(1)
		s.opts.OnTCP(ev)
	case ClassUDPDatagram:
		// UDP is dropped unless the caller explicitly asked for it. This is the
		// runtime half of the TCP-only scope; the type-level half is that
		// UDPDatagramEvent cannot be assigned into a TCP total at all.
		if s.opts.OnUDP == nil {
			s.ignored.Add(1)
			return
		}
		ev, err := DecodeUDPDatagram(id, version, userData(rec))
		if err != nil {
			s.countDecodeFailure(err)
			return
		}
		s.decoded.Add(1)
		s.opts.OnUDP(ev)
	case ClassIgnored:
		s.ignored.Add(1)
	}
}

// countDecodeFailure books a refused event against the counter that says WHY.
// A newer event version is separated from a malformed payload because the two
// mean different things to whoever reads Stats: one says Windows changed the
// template out from under this package, the other says the payload was short.
func (s *Session) countDecodeFailure(err error) {
	if errors.Is(err, ErrUnsupportedEventVersion) {
		s.unsupportedVersion.Add(1)
		return
	}
	s.dropped.Add(1)
}

// userData exposes an EVENT_RECORD's payload as a byte slice.
//
// The slice ALIASES an ETW-owned buffer that Windows reuses the moment the
// callback returns, so it must be consumed synchronously and never retained.
// Both decoders honour that: they copy every field into value types.
func userData(rec *eventRecord) []byte {
	if rec.UserData == nil || rec.UserDataLength == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(rec.UserData), int(rec.UserDataLength))
}
