// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package g

import (
	"sync/atomic"
	"unsafe"
)

// defined constants
const (
	// G status
	//
	// Beyond indicating the general state of a G, the G status
	// acts like a lock on the goroutine's Stack (and hence its
	// ability to execute user code).
	//
	// If you add to this list, add to the list
	// of "okay during garbage collection" status
	// in mgcmark.go too.
	//
	// TODO(austin): The _Gscan bit could be much lighter-weight.
	// For example, we could choose not to run _Gscanrunnable
	// goroutines found in the run queue, rather than CAS-looping
	// until they become _Grunnable. And transitions like
	// _Gscanwaiting -> _Gscanrunnable are actually okay because
	// they don't affect Stack ownership.

	// _Gidle means this goroutine was just allocated and has not
	// yet been initialized.
	_Gidle = iota // 0

	// _Grunnable means this goroutine is on a run queue. It is
	// not currently executing user code. The Stack is not owned.
	_Grunnable // 1

	// _Grunning means this goroutine may execute user code. The
	// Stack is owned by this goroutine. It is not on a run queue.
	// It is assigned an M and a P (G.m and G.M.p are valid).
	_Grunning // 2

	// _Gsyscall means this goroutine is executing a system call.
	// It is not executing user code. The Stack is owned by this
	// goroutine. It is not on a run queue. It is assigned an M.
	_Gsyscall // 3

	// _Gwaiting means this goroutine is blocked in the runtime.
	// It is not executing user code. It is not on a run queue,
	// but should be recorded somewhere (e.G., a channel wait
	// queue) so it can be ready()d when necessary. The Stack is
	// not owned *except* that a channel operation may read or
	// write parts of the Stack under the appropriate channel
	// lock. Otherwise, it is not safe to access the Stack after a
	// goroutine enters _Gwaiting (e.G., it may get moved).
	_Gwaiting // 4

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	_Gmoribund_unused // 5

	// _Gdead means this goroutine is currently unused. It may be
	// just exited, on a free list, or just being initialized. It
	// is not executing user code. It may or may not have a Stack
	// allocated. The G and its Stack (if any) are owned by the M
	// that is exiting the G or that obtained the G from the free
	// list.
	_Gdead // 6

	// _Genqueue_unused is currently unused.
	_Genqueue_unused // 7

	// _Gcopystack means this goroutine's Stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// Stack is owned by the goroutine that put it in _Gcopystack.
	_Gcopystack // 8

	// _Gpreempted means this goroutine stopped itself for a
	// suspendG preemption. It is like _Gwaiting, but nothing is
	// yet responsible for ready()ing it. Some suspendG must CAS
	// the status to _Gwaiting to take responsibility for
	// ready()ing this G.
	_Gpreempted // 9

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the Stack. The
	// goroutine is not executing user code and the Stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// Stack. This is otherwise like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	_Gscan          = 0x1000
	_Gscanrunnable  = _Gscan + _Grunnable  // 0x1001
	_Gscanrunning   = _Gscan + _Grunning   // 0x1002
	_Gscansyscall   = _Gscan + _Gsyscall   // 0x1003
	_Gscanwaiting   = _Gscan + _Gwaiting   // 0x1004
	_Gscanpreempted = _Gscan + _Gpreempted // 0x1009
)

const (
	// P status

	// _Pidle means a P is not being used to run user code or the
	// scheduler. Typically, it's on the idle P list and available
	// to the scheduler, but it may just be transitioning between
	// other states.
	//
	// The P is owned by the idle list or by whatever is
	// transitioning its state. Its run queue is empty.
	_Pidle = iota

	// _Prunning means a P is owned by an M and is being used to
	// run user code or the scheduler. Only the M that owns this P
	// is allowed to change the P's status from _Prunning. The M
	// may transition the P to _Pidle (if it has no more work to
	// do), _Psyscall (when entering a syscall), or _Pgcstop (to
	// halt for the GC). The M may also hand ownership of the P
	// off directly to another M (e.G., to schedule a locked G).
	_Prunning

	// _Psyscall means a P is not running user code. It has
	// affinity to an M in a syscall but is not owned by it and
	// may be stolen by another M. This is similar to _Pidle but
	// uses lightweight transitions and maintains M affinity.
	//
	// Leaving _Psyscall must be done with a CAS, either to steal
	// or retake the P. Note that there's an ABA hazard: even if
	// an M successfully CASes its original P back to _Prunning
	// after a syscall, it must understand the P may have been
	// used by another M in the interim.
	_Psyscall

	// _Pgcstop means a P is halted for STW and owned by the M
	// that stopped the world. The M that stopped the world
	// continues to use its P, even in _Pgcstop. Transitioning
	// from _Prunning to _Pgcstop causes an M to release its P and
	// park.
	//
	// The P retains its run queue and startTheWorld will restart
	// the scheduler on Ps with non-empty run queues.
	_Pgcstop

	// _Pdead means a P is no longer used (GOMAXPROCS shrank). We
	// reuse Ps if GOMAXPROCS increases. A dead P is mostly
	// stripped of its resources, though a few things remain
	// (e.G., trace buffers).
	_Pdead
)

// Mutex Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
// Initialization is helpful for static lock ranking, but not required.
type Mutex struct {
	// Empty struct if lock ranking is disabled, otherwise includes the lock rank
	//lockRankStruct
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

// sleep and wakeup on one-time events.
// before any calls to notesleep or notewakeup,
// must call noteclear to initialize the Note.
// then, exactly one thread can call notesleep
// and exactly one thread can call notewakeup (once).
// once notewakeup has been called, the notesleep
// will return.  future notesleep will return immediately.
// subsequent noteclear must be called only after
// previous notesleep has returned, e.G. it's disallowed
// to call noteclear straight after notewakeup.
//
// notetsleep is like notesleep but wakes up after
// a given number of nanoseconds even if the event
// has not yet happened.  if a goroutine uses notetsleep to
// wake up early, it must wait to call noteclear until it
// can be sure that no other goroutine is calling
// notewakeup.
//
// notesleep/notetsleep are generally called on g0,
// notetsleepg is similar to notetsleep but is called on user G.
type Note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr
}

type Funcval struct {
	fn uintptr
	// variable-size, fn-specific data here
}

//type iface struct {
//	tab  *itab
//	data unsafe.Pointer
//}

//type eface struct {
//	_type *_type
//	data  unsafe.Pointer
//}
//
//func efaceOf(ep *any) *eface {
//	return (*eface)(unsafe.Pointer(ep))
//}

// The Guintptr, Muintptr, and Puintptr are all used to bypass write barriers.
// It is particularly important to avoid write barriers when the current P has
// been released, because the GC thinks the world is stopped, and an
// unexpected write barrier would not be synchronized with the GC,
// which can lead to a half-executed write barrier that has marked the object
// but not queued it. If the GC skips the object and completes before the
// queuing can occur, it will incorrectly free the object.
//
// We tried using special assignment functions invoked only when not
// holding a running P, but then some updates to a particular memory
// word went through write barriers and some did not. This breaks the
// write barrier shadow checking mode, and it is also scary: better to have
// a word that is completely ignored by the GC than to have one for which
// only a few updates are ignored.
//
// Gs and Ps are always reachable via true pointers in the
// allgs and allp lists or (during allocation before they reach those lists)
// from Stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an Muintptr across a safe point.

// A Guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.G goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.G's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using Guintptr doesn't make that problem any worse.
// Note that pollDesc.rg, pollDesc.wg also store G in uintptr form,
// so they would need to be updated too if G's start moving.
type Guintptr uintptr

//go:nosplit
func (gp Guintptr) ptr() *G { return (*G)(unsafe.Pointer(gp)) }

//go:nosplit
func (gp *Guintptr) set(g *G) { *gp = Guintptr(unsafe.Pointer(g)) }

/// /go:nosplit
// func (gp *Guintptr) cas(old, new Guintptr) bool {
// 	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
// }

// // setGNoWB performs *gp = new without a write barrier.
// // For times when it's impractical to use a Guintptr.
// //
// //go:nosplit
// //go:nowritebarrier
// func setGNoWB(gp **G, new *G) {
// 	(*Guintptr)(unsafe.Pointer(gp)).set(new)
// }

type Puintptr uintptr

// //go:nosplit
//func (pp Puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

////go:nosplit
//func (pp *Puintptr) set(p *p) { *pp = Puintptr(unsafe.Pointer(p)) }

// Muintptr is a *M that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
//  1. Never hold an Muintptr locally across a safe point.
//
//  2. Any Muintptr in the heap must be owned by the M itself so it can
//     ensure it is not in use when the last true *M is released.
type Muintptr uintptr

//go:nosplit
func (mp Muintptr) ptr() *M { return (*M)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *Muintptr) set(m *M) { *mp = Muintptr(unsafe.Pointer(m)) }

// // setMNoWB performs *mp = new without a write barrier.
// // For times when it's impractical to use an Muintptr.
// //
// //go:nosplit
// //go:nowritebarrier
// func setMNoWB(mp **M, new *M) {
// 	(*Muintptr)(unsafe.Pointer(mp)).set(new)
// }

type GoBuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	//
	// ctxt is unusual with respect to GC: it may be a
	// heap-allocated Funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the GoBuf. Hence, we treat it as a
	// root during Stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.
	sp   uintptr
	pc   uintptr
	g    Guintptr
	ctxt unsafe.Pointer
	ret  uintptr
	lr   uintptr
	bp   uintptr // for framepointer-enabled architectures
}

// Sudog represents a G in a wait list, such as for sending/receiving
// on a channel.
//
// Sudog is necessary because the G ↔ synchronization object relation
// is many-to-many. A G can be on many wait lists, so there may be
// many sudogs for one G; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.
type Sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this Sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.

	g *G

	next *Sudog
	prev *Sudog
	elem unsafe.Pointer // data element (may point to Stack)

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by G.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.

	acquiretime int64
	releasetime int64
	ticket      uint32

	// isSelect indicates g is participating in a select, so
	// G.selectDone must be CAS'd to win the wake-up race.
	isSelect bool

	// success indicates whether communication over channel c
	// succeeded. It is true if the goroutine was awoken because a
	// value was delivered over channel c, and false if awoken
	// because c was closed.
	success bool

	parent   *Sudog // semaRoot binary tree
	waitlink *Sudog // G.waiting list or semaRoot
	waittail *Sudog // semaRoot
	c        *HChan // channel
}

type libcall struct {
	fn   uintptr
	n    uintptr // number of parameters
	args uintptr // parameters
	r1   uintptr // return values
	r2   uintptr
	err  uintptr // error number
}

// Stack describes a Go execution Stack.
// The bounds of the Stack are exactly [lo, hi),
// with no implicit data structures on either side.
type Stack struct {
	lo uintptr
	hi uintptr
}

// heldLockInfo gives info on a held lock and the rank of that lock
type heldLockInfo struct {
	lockAddr uintptr
	//rank     lockRank
}

type G struct {
	// Stack parameters.
	// stack describes the actual stack memory: [Stack.lo, Stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is Stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the C stack growth prologue.
	// It is Stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	stack       Stack   // offset known to runtime/cgo
	stackguard0 uintptr // offset known to liblink
	stackguard1 uintptr // offset known to liblink

	_panic    *_panic // innermost panic - offset known to liblink
	_defer    *_defer // innermost defer
	m         *M      // current m; offset known to arm liblink
	sched     GoBuf
	syscallsp uintptr // if status==Gsyscall, syscallsp = sched.sp to use during gc
	syscallpc uintptr // if status==Gsyscall, syscallpc = sched.pc to use during gc
	stktopsp  uintptr // expected sp at top of stack, to check in traceback
	// param is a generic pointer parameter field used to pass
	// values in particular contexts where other storage for the
	// parameter would be difficult to find. It is currently used
	// in three ways:
	// 1. When a channel operation wakes up a blocked goroutine, it sets param to
	//    point to the Sudog of the completed blocking operation.
	// 2. By gcAssistAlloc1 to signal back to its caller that the goroutine completed
	//    the GC cycle. It is unsafe to do so in any other way, because the goroutine's
	//    stack may have moved in the meantime.
	// 3. By debugCallWrap to pass parameters to a new goroutine because allocating a
	//    closure in the runtime is forbidden.
	param        unsafe.Pointer
	atomicstatus atomic.Uint32
	stackLock    uint32 // sigprof/scang lock; TODO: fold in to atomicstatus
	GoID         uint64
	schedlink    Guintptr
	waitsince    int64      // approx time when the G become blocked
	waitreason   waitReason // if status==Gwaiting

	preempt       bool // preemption signal, duplicates stackguard0 = stackpreempt
	preemptStop   bool // transition to _Gpreempted on preemption; otherwise, just deschedule
	preemptShrink bool // shrink stack at synchronous safe point

	// asyncSafePoint is set if G is stopped at an asynchronous
	// safe point. This means there are frames on the stack
	// without precise pointer information.
	asyncSafePoint bool

	paniconfault bool // panic (instead of crash) on unexpected fault address
	gcscandone   bool // G has scanned stack; protected by _Gscan bit in status
	throwsplit   bool // must not split stack
	// activeStackChans indicates that there are unlocked channels
	// pointing into this goroutine's stack. If true, stack
	// copying needs to acquire channel locks to protect these
	// areas of the stack.
	activeStackChans bool
	// parkingOnChan indicates that the goroutine is about to
	// park on a chansend or chanrecv. Used to signal an unsafe point
	// for stack shrinking.
	parkingOnChan atomic.Bool

	raceignore     int8     // ignore race detection events
	sysblocktraced bool     // StartTrace has emitted EvGoInSyscall about this goroutine
	tracking       bool     // whether we're tracking this G for sched latency statistics
	trackingSeq    uint8    // used to decide whether to track this G
	trackingStamp  int64    // timestamp of when the G last started being tracked
	runnableTime   int64    // the amount of time spent runnable, cleared when running, only used when tracking
	sysexitticks   int64    // cputicks when syscall has returned (for tracing)
	traceseq       uint64   // trace event sequencer
	tracelastp     Puintptr // last P emitted an event for this goroutine
	lockedm        Muintptr
	sig            uint32
	writebuf       []byte
	sigcode0       uintptr
	sigcode1       uintptr
	sigpc          uintptr
	gopc           uintptr         // pc of go statement that created this goroutine
	ancestors      *[]ancestorInfo // ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors)
	startpc        uintptr         // pc of goroutine function
	racectx        uintptr
	waiting        *Sudog         // Sudog structures this G is waiting on (that have a valid elem ptr); in lock order
	cgoCtxt        []uintptr      // cgo traceback context
	labels         unsafe.Pointer // profiler labels
	timer          *Timer         // cached timer for time.Sleep
	selectDone     atomic.Uint32  // are we participating in a select and did someone win the race?

	// goroutineProfiled indicates the status of this goroutine's stack for the
	// current in-progress goroutine profile
	//goroutineProfiled goroutineProfileStateHolder

	// Per-G GC state

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	gcAssistBytes int64
}

// gTrackingPeriod is the number of transitions out of _Grunning between
// latency tracking runs.
const gTrackingPeriod = 8

const (
	// tlsSlots is the number of pointer-sized slots reserved for TLS on some platforms,
	// like Windows.
	tlsSlots = 6
	//tlsSize  = tlsSlots * goarch.PtrSize
)

// Values for M.freeWait.
const (
	freeMStack = 0 // M done, free Stack and reference.
	freeMRef   = 1 // M done, free reference.
	freeMWait  = 2 // M still in use.
)

type M struct {
	g0      *G     // goroutine with scheduling Stack
	morebuf GoBuf  // GoBuf arg to morestack
	divmod  uint32 // div/mod denominator for arm - known to liblink
	_       uint32 // align next field to 8 bytes

	// Fields not known to debuggers.
	procid        uint64            // for debuggers, but offset not hard-coded
	gsignal       *G                // signal-handling G
	goSigStack    GSignalStack      // Go-allocated signal handling Stack
	sigmask       SigSet            // storage for saved signal mask
	tls           [tlsSlots]uintptr // thread-local storage (for x86 extern register)
	mstartfn      func()
	curg          *G       // current running goroutine
	caughtsig     Guintptr // goroutine running during fatal signal
	p             Puintptr // attached p for executing go code (nil if not executing go code)
	nextp         Puintptr
	oldp          Puintptr // the p that was attached before executing a syscall
	id            int64
	mallocing     int32
	throwing      int32
	preemptoff    string // if != "", keep curg running on this M
	locks         int32
	dying         int32
	profilehz     int32
	spinning      bool // M is out of work and is actively looking for work
	blocked       bool // M is blocked on a Note
	newSigstack   bool // minit on C thread called sigaltstack
	printlock     int8
	incgo         bool          // M is executing a cgo call
	isextra       bool          // M is an extra M
	freeWait      atomic.Uint32 // Whether it is safe to free g0 and delete M (one of freeMRef, freeMStack, freeMWait)
	fastrand      uint64
	needextram    bool
	traceback     uint8
	ncgocall      uint64        // number of cgo calls in total
	ncgo          int32         // number of cgo calls currently in progress
	cgoCallersUse atomic.Uint32 // if non-zero, cgoCallers in use temporarily
	cgoCallers    *CgoCallers   // cgo traceback if crashing in cgo call
	park          Note
	alllink       *M // on allm
	schedlink     Muintptr
	lockedg       Guintptr
	createstack   [32]uintptr // Stack that created this thread.
	lockedExt     uint32      // tracking for external LockOSThread
	lockedInt     uint32      // tracking for internal lockOSThread
	nextwaitm     Muintptr    // next M waiting for lock
	waitunlockf   func(*G, unsafe.Pointer) bool
	waitlock      unsafe.Pointer
	waittraceev   byte
	waittraceskip int
	startingtrace bool
	syscalltick   uint32
	freelink      *M // on sched.freem

	// these are here because they are too large to be on the Stack
	// of low-level NOSPLIT functions.
	libcall   libcall
	libcallpc uintptr // for cpu profiler
	libcallsp uintptr
	libcallg  Guintptr
	syscall   libcall // stores syscall parameters on windows

	vdsoSP uintptr // SP for traceback while in VDSO call (0 if not in call)
	vdsoPC uintptr // PC for traceback while in VDSO call

	// preemptGen counts the number of completed preemption
	// signals. This is used to detect when a preemption is
	// requested, but fails.
	preemptGen atomic.Uint32

	// Whether this is a pending preemption signal on this M.
	signalPending atomic.Uint32

	// dlogPerM

	MOS

	// Up to 10 locks held by this M, maintained by the lock ranking code.
	locksHeldLen int
	locksHeld    [10]heldLockInfo
}

// Values for the flags field of a sigTabT.
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly
	_SigThrow                // if signal.Notify doesn't take it, exit loudly
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // Don't explicitly install handler, but add SA_ONSTACK to existing libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
// type _func struct {
// 	entryOff uint32 // start pc, as offset from moduledata.text/pcHeader.textStart
// 	nameOff  int32  // function name, as index into moduledata.funcnametab.

// 	args        int32  // in/out args size
// 	deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.

// 	pcsp      uint32
// 	pcfile    uint32
// 	pcln      uint32
// 	npcdata   uint32
// 	cuOffset  uint32 // runtime.cutab offset of this function's CU
// 	startLine int32  // line number of start of function (func keyword/TEXT directive)
// 	funcID    funcID // set for certain special runtime functions
// 	flag      funcFlag
// 	_         [1]byte // pad
// 	nfuncdata uint8   // must be last, must end on a uint32-aligned boundary

// 	// The end of the struct is followed immediately by two variable-length
// 	// arrays that reference the pcdata and funcdata locations for this
// 	// function.

// 	// pcdata contains the offset into moduledata.pctab for the start of
// 	// that index's table. e.G.,
// 	// &moduledata.pctab[_func.pcdata[_PCDATA_UnsafePoint]] is the start of
// 	// the unsafe point table.
// 	//
// 	// An offset of 0 indicates that there is no table.
// 	//
// 	// pcdata [npcdata]uint32

// 	// funcdata contains the offset past moduledata.gofunc which contains a
// 	// pointer to that index's funcdata. e.G.,
// 	// *(moduledata.gofunc +  _func.funcdata[_FUNCDATA_ArgsPointerMaps]) is
// 	// the argument pointer map.
// 	//
// 	// An offset of ^uint32(0) indicates that there is no entry.
// 	//
// 	// funcdata [nfuncdata]uint32
// }

// // Pseudo-Func that is returned for PCs that occur in inlined code.
// // A *Func can be either a *_func or a *funcinl, and they are distinguished
// // by the first uintptr.
// type funcinl struct {
// 	ones      uint32  // set to ^0 to distinguish from _func
// 	entry     uintptr // entry of the real (the "outermost") frame
// 	name      string
// 	file      string
// 	line      int32
// 	startLine int32
// }

// // layout of Itab known to compilers
// // allocated in non-garbage-collected memory
// // Needs to be in sync with
// // ../cmd/compile/internal/reflectdata/reflect.go:/^func.WriteTabs.
// type itab struct {
// 	inter *interfacetype
// 	_type *_type
// 	hash  uint32 // copy of _type.hash. Used for type switches.
// 	_     [4]byte
// 	fun   [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
// }

// Lock-free Stack node.
// Also known to export_test.go.
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

type forcegcstate struct {
	lock Mutex
	g    *G
	idle atomic.Bool
}

//
//// extendRandom extends the random numbers in r[:n] to the whole slice r.
//// Treats n<0 as n==0.
//func extendRandom(r []byte, n int) {
//	if n < 0 {
//		n = 0
//	}
//	for n < len(r) {
//		// Extend random bits using hash function & time seed
//		w := n
//		if w > 16 {
//			w = 16
//		}
//		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
//		for i := 0; i < goarch.PtrSize && n < len(r); i++ {
//			r[n] = byte(h)
//			n++
//			h >>= 8
//		}
//	}
//}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in deferProcStack.
// This struct must match the code in cmd/compile/internal/ssagen/ssa.go:deferstruct
// and cmd/compile/internal/ssagen/ssa.go:(*state).call.
// Some defers will be allocated on the Stack and some on the heap.
// All defers are logically part of the Stack, so write barriers to
// initialize them are not required. All defers must be manually scanned,
// and for heap defers, marked.
type _defer struct {
	started bool
	heap    bool
	// openDefer indicates that this _defer is for a frame with open-coded
	// defers. We have only one defer record for the entire frame (which may
	// currently have 0, 1, or more defers active).
	openDefer bool
	sp        uintptr // sp at time of defer
	pc        uintptr // pc at time of defer
	fn        func()  // can be nil for open-coded defers
	_panic    *_panic // panic that is running defer
	link      *_defer // next defer on G; can point to either heap or Stack!

	// If openDefer is true, the fields below record values about the Stack
	// frame and associated function that has the open-coded defer(s). sp
	// above will be the sp for the frame, and pc will be address of the
	// deferreturn call in the function.
	fd   unsafe.Pointer // funcdata for the function associated with the frame
	varp uintptr        // value of varp for the Stack frame
	// framepc is the current pc associated with the Stack frame. Together,
	// with sp above (which is the sp associated with the Stack frame),
	// framepc/sp can be used as pc/sp pair to continue a Stack trace via
	// gentraceback().
	framepc uintptr
}

// A _panic holds information about an active panic.
//
// A _panic value must only ever live on the Stack.
//
// The argp and link fields are Stack pointers, but don't need special
// handling during Stack growth: because they are pointer-typed and
// _panic values only live on the Stack, regular Stack pointer
// adjustment takes care of them.
type _panic struct {
	argp      unsafe.Pointer // pointer to arguments of deferred call run during panic; cannot move - known to liblink
	arg       any            // argument to panic
	link      *_panic        // link to earlier panic
	pc        uintptr        // where to return to in runtime if this panic is bypassed
	sp        unsafe.Pointer // where to return to in runtime if this panic is bypassed
	recovered bool           // whether this panic is over
	aborted   bool           // the panic was aborted
	goexit    bool
}

// ancestorInfo records details of where a goroutine was started.
type ancestorInfo struct {
	pcs  []uintptr // pcs from the Stack of this goroutine
	goid uint64    // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

const (
	_TraceRuntimeFrames = 1 << iota // include frames for internal runtime functions.
	_TraceTrap                      // the initial PC, SP are from a trap, not a return PC from a call
	_TraceJumpStack                 // if traceback is on a systemstack, resume trace at G that called into it
)

// The maximum number of frames we print for a traceback
const _TracebackMaxFrames = 100

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonSelect                                  // "select"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonGCScavengeWait                          // "GC scavenge wait"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGCIdle                             // "force gc (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonSyncMutexLock                           // "sync.Mutex.Lock"
	waitReasonSyncRWMutexRLock                        // "sync.RWMutex.RLock"
	waitReasonSyncRWMutexLock                         // "sync.RWMutex.Lock"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonGCWorkerActive                          // "GC worker (active)"
	waitReasonPreempted                               // "preempted"
	waitReasonDebugCall                               // "debug call"
	waitReasonGCMarkTermination                       // "GC mark termination"
	waitReasonStoppingTheWorld                        // "stopping the world"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonGCScavengeWait:        "GC scavenge wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGCIdle:           "force gc (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonSyncMutexLock:         "sync.Mutex.Lock",
	waitReasonSyncRWMutexRLock:      "sync.RWMutex.RLock",
	waitReasonSyncRWMutexLock:       "sync.RWMutex.Lock",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonGCWorkerActive:        "GC worker (active)",
	waitReasonPreempted:             "preempted",
	waitReasonDebugCall:             "debug call",
	waitReasonGCMarkTermination:     "GC mark termination",
	waitReasonStoppingTheWorld:      "stopping the world",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

func (w waitReason) isMutexWait() bool {
	return w == waitReasonSyncMutexLock ||
		w == waitReasonSyncRWMutexRLock ||
		w == waitReasonSyncRWMutexLock
}
