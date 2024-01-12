// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// defined constants
const (
	// G status
	//
	// Beyond indicating the general state of a G, the G status
	// acts like a lock on the goroutine's stack (and hence its
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
	// they don't affect stack ownership.

	// _Gidle means this goroutine was just allocated and has not
	// yet been initialized.
	_Gidle = iota // 0 // 注释：(空闲不执行)_Gidle=0 刚刚被分配并且还没有被初始化

	// _Grunnable means this goroutine is on a run queue. It is
	// not currently executing user code. The stack is not owned.
	_Grunnable // 1 // 注释：(准备执行)_Grunnable=1 没有执行代码，没有栈的所有权，存储在运行队列中

	// _Grunning means this goroutine may execute user code. The
	// stack is owned by this goroutine. It is not on a run queue.
	// It is assigned an M and a P (g.m and g.m.p are valid).
	_Grunning // 2 // 注释：(执行中)_Grunning=2 可以执行代码，拥有栈的所有权，被赋予了内核线程 M 和处理器 P

	// _Gsyscall means this goroutine is executing a system call.
	// It is not executing user code. The stack is owned by this
	// goroutine. It is not on a run queue. It is assigned an M.
	_Gsyscall // 3 // 注释：(系统调用)_Gsyscall=3 正在执行系统调用，没有执行用户代码，拥有栈的所有权，被赋予了内核线程 M 但是不在运行队列上

	// _Gwaiting means this goroutine is blocked in the runtime.
	// It is not executing user code. It is not on a run queue,
	// but should be recorded somewhere (e.g., a channel wait
	// queue) so it can be ready()d when necessary. The stack is
	// not owned *except* that a channel operation may read or
	// write parts of the stack under the appropriate channel
	// lock. Otherwise, it is not safe to access the stack after a
	// goroutine enters _Gwaiting (e.g., it may get moved).
	_Gwaiting // 4 // 注释：(等待中、挂起中)_Gwaiting=4 由于运行时而被阻塞，没有执行用户代码并且不在运行队列上，但是可能存在于 Channel 的等待队列上。若需要时执行ready()唤醒

	// _Gmoribund_unused is currently unused, but hardcoded in gdb
	// scripts.
	_Gmoribund_unused // 5 // 注释：(该状态未使用)_Gmoribund_unused=5 当前此状态未使用，但硬编码在了gdb 脚本里，可以不用关注

	// _Gdead means this goroutine is currently unused. It may be
	// just exited, on a free list, or just being initialized. It
	// is not executing user code. It may or may not have a stack
	// allocated. The G and its stack (if any) are owned by the M
	// that is exiting the G or that obtained the G from the free
	// list.
	_Gdead // 6 // 注释：_Gdead=6 没有被使用，可能刚刚退出，或在一个freelist；也或者刚刚被初始化；没有执行代码，可能有分配的栈也可能没有；G和分配的栈（如果已分配过栈）归刚刚退出G的M所有或从free list 中获取

	// _Genqueue_unused is currently unused.
	_Genqueue_unused // 7 // 注释：(该状态未使用)_Genqueue_unused=7 目前未使用，不用理会

	// _Gcopystack means this goroutine's stack is being moved. It
	// is not executing user code and is not on a run queue. The
	// stack is owned by the goroutine that put it in _Gcopystack.
	_Gcopystack // 8 // 注释：(栈拷贝中)_Gcopystack=8 栈正在被拷贝，没有执行代码，不在运行队列上

	// _Gpreempted means this goroutine stopped itself for a
	// suspendG preemption. It is like _Gwaiting, but nothing is
	// yet responsible for ready()ing it. Some suspendG must CAS
	// the status to _Gwaiting to take responsibility for
	// ready()ing this G.
	_Gpreempted // 9 // 注释：(抢占阻塞)_Gpreempted=9 由于抢占而被阻塞，没有执行用户代码并且不在运行队列上，等待唤醒

	// _Gscan combined with one of the above states other than
	// _Grunning indicates that GC is scanning the stack. The
	// goroutine is not executing user code and the stack is owned
	// by the goroutine that set the _Gscan bit.
	//
	// _Gscanrunning is different: it is used to briefly block
	// state transitions while GC signals the G to scan its own
	// stack. This is otherwise like _Grunning.
	//
	// atomicstatus&~Gscan gives the state the goroutine will
	// return to when the scan completes.
	_Gscan          = 0x1000               // 注释：_Gscan=10 GC 正在扫描栈空间，没有执行代码，可以与其他状态同时存在
	_Gscanrunnable  = _Gscan + _Grunnable  // 0x1001
	_Gscanrunning   = _Gscan + _Grunning   // 0x1002 // 注释：GC扫描中+G运行中
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
	_Pidle = iota // 注释：P的空闲状态

	// _Prunning means a P is owned by an M and is being used to
	// run user code or the scheduler. Only the M that owns this P
	// is allowed to change the P's status from _Prunning. The M
	// may transition the P to _Pidle (if it has no more work to
	// do), _Psyscall (when entering a syscall), or _Pgcstop (to
	// halt for the GC). The M may also hand ownership of the P
	// off directly to another M (e.g., to schedule a locked G).
	_Prunning // 注释：P运行中的状态

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
	_Psyscall // 注释：P系统调用状态

	// _Pgcstop means a P is halted for STW and owned by the M
	// that stopped the world. The M that stopped the world
	// continues to use its P, even in _Pgcstop. Transitioning
	// from _Prunning to _Pgcstop causes an M to release its P and
	// park.
	//
	// The P retains its run queue and startTheWorld will restart
	// the scheduler on Ps with non-empty run queues.
	_Pgcstop // 注释：(STW时停止P，包括系统调用时也会停止)GC停止世界（STW）时把当前的P也停止了，并设置这个状态

	// _Pdead means a P is no longer used (GOMAXPROCS shrank). We
	// reuse Ps if GOMAXPROCS increases. A dead P is mostly
	// stripped of its resources, though a few things remain
	// (e.g., trace buffers).
	_Pdead
)

// Mutual exclusion locks.  In the uncontended case,
// as fast as spin locks (just a few user-level instructions),
// but on the contention path they sleep in the kernel.
// A zeroed Mutex is unlocked (no need to initialize each lock).
// Initialization is helpful for static lock ranking, but not required.
type mutex struct {
	// Empty struct if lock ranking is disabled, otherwise includes the lock rank
	lockRankStruct
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
// previous notesleep has returned, e.g. it's disallowed
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
// notetsleepg is similar to notetsleep but is called on user g.
type note struct {
	// Futex-based impl treats it as uint32 key,
	// while sema-based impl as M* waitm.
	// Used to be a union, but unions break precise GC.
	key uintptr // 注释：这里的值是【0或1或*M】0什么都不做，1表示已经是唤醒状态，*M待唤醒的M指针
}

// 注释：存储方法的结构体
// 注释：还有一些隐士的字段，参考 runtime.reflectMethodValue 结构体
type funcval struct {
	fn uintptr // 注释：指令指针，PC值
	// variable-size, fn-specific data here
	// 注释：这里在newproc中定义了一个指针的大小用来存储P指针
	// 注释：后面会有两个属性分别是(stack *bitvector 和 argLen uintptr，参考 reflectMethodValue )
}

// 注释：带方法签名的接口在运行时的具体结构体
type iface struct {
	tab  *itab          // 注释：存储了接口的类型、接口中的动态数据类型、动态数据类型的函数指针等
	data unsafe.Pointer // 注释：动态类型值的指针，由于数据大小不确定。存储在接口中的值必须能够获取其地址，所以分配在栈中的值一旦赋值给接口后，会发生内存逃逸，在堆区为其开辟内存。
}

// 注释：空接口的接头体(empyt interface)
type eface struct {
	_type *_type
	data  unsafe.Pointer
}

func efaceOf(ep *interface{}) *eface {
	return (*eface)(unsafe.Pointer(ep))
}

// The guintptr, muintptr, and puintptr are all used to bypass write barriers.
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
// from stack variables.
//
// Ms are always reachable via true pointers either from allm or
// freem. Unlike Gs and Ps we do free Ms, so it's important that
// nothing ever hold an muintptr across a safe point.

// A guintptr holds a goroutine pointer, but typed as a uintptr
// to bypass write barriers. It is used in the Gobuf goroutine state
// and in scheduling lists that are manipulated without a P.
//
// The Gobuf.g goroutine pointer is almost always updated by assembly code.
// In one of the few places it is updated by Go code - func save - it must be
// treated as a uintptr to avoid a write barrier being emitted at a bad time.
// Instead of figuring out how to emit the write barriers missing in the
// assembly manipulation, we change the type of the field to uintptr,
// so that it does not require write barriers at all.
//
// Goroutine structs are published in the allg list and never freed.
// That will keep the goroutine structs from being collected.
// There is never a time that Gobuf.g's contain the only references
// to a goroutine: the publishing of the goroutine in allg comes first.
// Goroutine pointers are also kept in non-GC-visible places like TLS,
// so I can't see them ever moving. If we did want to start moving data
// in the GC, we'd need to allocate the goroutine structs from an
// alternate arena. Using guintptr doesn't make that problem any worse.
type guintptr uintptr

//go:nosplit
func (gp guintptr) ptr() *g { return (*g)(unsafe.Pointer(gp)) }

// 注释：把gp设置成g
//
//go:nosplit
func (gp *guintptr) set(g *g) { *gp = guintptr(unsafe.Pointer(g)) }

// 注释：(Compare And Swap)比较赋值(原子操作)，如果prt==old,则赋值ptr=new，返回TRUE否则返回FALSE
//
//go:nosplit
func (gp *guintptr) cas(old, new guintptr) bool {
	return atomic.Casuintptr((*uintptr)(unsafe.Pointer(gp)), uintptr(old), uintptr(new))
}

// setGNoWB performs *gp = new without a write barrier.
// For times when it's impractical to use a guintptr.
//
//go:nosplit
//go:nowritebarrier
func setGNoWB(gp **g, new *g) {
	(*guintptr)(unsafe.Pointer(gp)).set(new)
}

type puintptr uintptr

//go:nosplit
func (pp puintptr) ptr() *p { return (*p)(unsafe.Pointer(pp)) }

//go:nosplit
func (pp *puintptr) set(p *p) { *pp = puintptr(unsafe.Pointer(p)) }

// muintptr is a *m that is not tracked by the garbage collector.
//
// Because we do free Ms, there are some additional constrains on
// muintptrs:
//
// 1. Never hold an muintptr locally across a safe point.
//
//  2. Any muintptr in the heap must be owned by the M itself so it can
//     ensure it is not in use when the last true *m is released.
type muintptr uintptr

//go:nosplit
func (mp muintptr) ptr() *m { return (*m)(unsafe.Pointer(mp)) }

//go:nosplit
func (mp *muintptr) set(m *m) { *mp = muintptr(unsafe.Pointer(m)) }

// setMNoWB performs *mp = new without a write barrier.
// For times when it's impractical to use an muintptr.
//
//go:nosplit
//go:nowritebarrier
func setMNoWB(mp **m, new *m) {
	(*muintptr)(unsafe.Pointer(mp)).set(new)
}

// 注释：协成执行现场数据(调度信息)，G状态(g.atomicstatus)变更时，都需要保存当前G的上下文和寄存器等信息。保存协成切换中切走时的寄存器等数据
type gobuf struct {
	// The offsets of sp, pc, and g are known to (hard-coded in) libmach.
	// 注释：寄存器 sp, pc 和 g 的偏移量，硬编码在 libmach
	// ctxt is unusual with respect to GC: it may be a
	// heap-allocated funcval, so GC needs to track it, but it
	// needs to be set and cleared from assembly, where it's
	// difficult to have write barriers. However, ctxt is really a
	// saved, live register, and we only ever exchange it between
	// the real register and the gobuf. Hence, we treat it as a
	// root during stack scanning, which means assembly that saves
	// and restores it doesn't need write barriers. It's still
	// typed as a pointer so that any other writes from Go get
	// write barriers.
	// 注释：调度器在将G由一种状态变更为另一种状态时，需要将上下文信息保存到这个gobuf结构体，当再次运行G的时候，再从这个结构体中读取出来，它主要用来暂存上下文信息。
	// 注释：其中的栈指针 sp 和程序计数器 pc 会用来存储或者恢复寄存器中的值，设置即将执行的代码
	sp   uintptr        // 注释：sp栈指针位置(保存CPU的rsp寄存器的值)
	pc   uintptr        // 注释：pc程序计数器，运行到的程序位置（指向下一个需要执行的地址）(保存CPU的rip寄存器的值)
	g    guintptr       // 注释：保存现场所在的G指针；当前gobuf的g(记录当前这个gobuf对象属于哪个g)(当前运行的g地址)
	ctxt unsafe.Pointer // 注释：ctxt上下文（调用方函数指针，用来链路追踪）
	ret  sys.Uintreg    // 注释：系统调用的结果(保存系统调用的返回值，因为从系统调用返回之后如果p被其它工作线程抢占，则这个g会被放入全局运行队列被其它工作线程调度，其它线程需要知道系统调用的返回值)
	lr   uintptr        // 注释：保存LR寄存器的值（LR（Link Register）是连接寄存器）
	bp   uintptr        // 注释：(保存CPU的硬件BP寄存器的值) // for framepointer-enabled architectures
}

// sudog represents a g in a wait list, such as for sending/receiving
// on a channel.
// 注释：sudog表示等待列表中的g，例如用于在信道上发送/接收。
//
// sudog is necessary because the g ↔ synchronization object relation
// is many-to-many. A g can be on many wait lists, so there may be
// many sudogs for one g; and many gs may be waiting on the same
// synchronization object, so there may be many sudogs for one object.
// 注释：sudog是必要的，因为g↔ 同步对象关系是多对多的。一个g可以在很多等待列表中，所以一个g可能有很多sudog；许多gs可能正在等待同一个同步对象，因此一个对象可能有许多sudog。
//
// sudogs are allocated from a special pool. Use acquireSudog and
// releaseSudog to allocate and free them.
// 注释：sudog是从一个特殊的池中分配的。使用acquireSudog和releaseSudog来分配和释放它们。
//
// 注释：等待(阻塞)的G（通常是全局G链表或当前P中等待的G列表中的成员），所有要执行的G都是以这个结构体的形式存在
type sudog struct {
	// The following fields are protected by the hchan.lock of the
	// channel this sudog is blocking on. shrinkstack depends on
	// this for sudogs involved in channel ops.

	g *g // 注释：需要休眠的G

	next *sudog         // 注释：双向循环链表的下一个指针
	prev *sudog         // 注释：双向循环链表的上一个指针
	elem unsafe.Pointer // 注释：数据元素指针（保存或接收数据的指针）// data element (may point to stack)

	// The following fields are never accessed concurrently.
	// For channels, waitlink is only accessed by g.
	// For semaphores, all fields (including the ones above)
	// are only accessed when holding a semaRoot lock.

	acquiretime int64 // 注释：初始胡的时间
	releasetime int64 // 注释：释放时的时间,-1代表send是再设置时间，如果大于0，会把cputicks()设置进来（CPU时钟周期计数器）。启动阻塞事件，blockevent阻塞监听的时间是当前值减去当时cputicks()值
	ticket      uint32

	// isSelect indicates g is participating in a select, so
	// g.selectDone must be CAS'd to win the wake-up race.
	isSelect bool // 注释：（是否是select导致的阻塞）是否参与select

	// success indicates whether communication over channel c
	// succeeded. It is true if the goroutine was awoken because a
	// value was delivered over channel c, and false if awoken
	// because c was closed.
	// 注释：信道c上的通信是否成功。如果goroutine因为值通过通道c传递而被唤醒，则为true，如果因为c被关闭而被唤醒则为false
	success bool // 注释：是否因通道唤醒(管道非关闭时唤醒为true，关闭时唤醒为false)

	parent   *sudog // semaRoot binary tree
	waitlink *sudog // 注释：阻塞链表 // g.waiting list or semaRoot
	waittail *sudog // semaRoot
	c        *hchan // 注释：（阻塞时存放管道的地址）所在管道channel的地址 // channel
}

type libcall struct {
	fn   uintptr
	n    uintptr // number of parameters
	args uintptr // parameters
	r1   uintptr // return values
	r2   uintptr
	err  uintptr // error number
}

// Stack describes a Go execution stack.
// The bounds of the stack are exactly [lo, hi),
// with no implicit data structures on either side.
// 注释：g使用栈的起始和结束位置,g的函数调用栈边界结构体
// 注释：用于判断空G是否有栈空间(判断lo是否等于0)
type stack struct {
	lo uintptr // 注释：栈顶，指向内存低地址(栈开始位置地址)
	hi uintptr // 注释：栈底，指向内存高地址(栈结束位置地址)
}

// heldLockInfo gives info on a held lock and the rank of that lock
type heldLockInfo struct {
	lockAddr uintptr
	rank     lockRank
}

// 注释：一个G一但被创建，那就不会消失，因为runtime有个allgs保存着所有的g指针，但不要担心，g对象引用的其他对象是会释放的，所以也占不了啥内存。
// 注释：g结构体用于代表一个goroutine，该结构体保存了goroutine的所有信息，包括栈，gobuf结构体和其它的一些状态信息
type g struct {
	// Stack parameters.
	// stack describes the actual stack memory: [stack.lo, stack.hi).
	// stackguard0 is the stack pointer compared in the Go stack growth prologue.
	// It is stack.lo+StackGuard normally, but can be StackPreempt to trigger a preemption.
	// stackguard1 is the stack pointer compared in the C stack growth prologue.
	// It is stack.lo+StackGuard on g0 and gsignal stacks.
	// It is ~0 on other goroutine stacks, to trigger a call to morestackc (and crash).
	stack stack // offset known to runtime/cgo // 注释：当前栈（G所在的栈）的边界(对应的开始和结束地址)（占用空间时16字节）
	// 注释：在g结构体中的stackguard0 字段是出现爆栈前的警戒线，通常值是stack.lo+StackGuard也可以存StackPreempt触发抢占。
	// 注释：stackguard0 的偏移量是16个字节，与当前的真实SP(stack pointer)和爆栈警戒线（stack.lo+StackGuard）比较，如果超出警戒线则表示需要进行栈扩容。
	// 注释：先调用runtime·morestack_noctxt()进行栈扩容，然后又跳回到函数的开始位置，此时函数的栈已经调整了。
	// 注释：然后再进行一次栈大小的检测，如果依然不足则继续扩容，直到栈足够大为止。
	// 注释：下面两个成员用于栈溢出检查，实现栈的自动伸缩，抢占调度也会用到stackguard0
	// 注释：打印对应的汇编会看到：CMPQ	SP, 16(CX) 如果小于0则执行 JLS L_MORE_STK 跳转位置执行CALL runtime.morestack_noctxt(SB)进行栈扩容，扩容后跳回L_BEGIN位置重新执行栈空间检查
	// 注释：如果栈空间大小依然不够则再重复扩容一次，知道栈大小够用为止
	stackguard0 uintptr // 注释：爆栈前警戒线（所在位置是G偏移16字节）。Go代码检查栈空间低于这个值会扩张。被设置成StackPreempt意味着当前g发出了抢占请求 // offset known to liblink
	stackguard1 uintptr // 注释：（C代码的爆栈警戒线）C代码检查栈空间低于这个值会扩张。 // offset known to liblink

	_panic       *_panic        // 注释：当前G的panic的链表首指针(panic的链表，_panic.link 链接) （panic是记录在这里的）// innermost panic - offset known to liblink
	_defer       *_defer        // 注释：当前G的延迟调用的链表首指针(单向链表，deferreturn会获取链表数据) (defer是记录在这里的) // innermost defer
	m            *m             // 注释：当前G绑定的M指针（此g正在被哪个工作线程执行） // current m; offset known to arm liblink
	sched        gobuf          // 注释：协成执行现场数据(调度信息)，G状态(atomicstatus)变更时，都需要保存当前G的上下文和寄存器等信息。保存协成切换中切走时的寄存器等数据
	syscallsp    uintptr        // 注释：如果G的状态为Gsyscall(系统调用时的PC值)，值为sched.sp主要用于GC期间 // if status==Gsyscall, syscallsp = sched.sp to use during gc
	syscallpc    uintptr        // 注释：如果G的状态为GSyscall(系统调用时SPP值)，值为sched.pc主要用于GC期间 // if status==Gsyscall, syscallpc = sched.pc to use during gc
	stktopsp     uintptr        // 注释：期望sp位于栈顶，用于回源跟踪 // expected sp at top of stack, to check in traceback
	param        unsafe.Pointer // 注释：wakeup唤醒时候传递的参数，睡眠时其他g可以设置param，唤醒时该g可以获取，例如调用ready() // passed parameter on wakeup
	atomicstatus uint32         // 注释：当前G的状态，例如：_Gidle:0;_Grunnable:1;_Grunning:2;_Gsyscall:3;_Gwaiting:4 等
	stackLock    uint32         // 注释：栈锁 // sigprof/scang lock; TODO: fold in to atomicstatus
	goid         int64          // 注释：当前G的唯一标识goroutine的ID，对开发者不可见，一般不使用此字段，Go开发团队未向外开放访问此字段
	schedlink    guintptr       // 注释：指向全局运行队列中的下一个g（全局行队列中的g是个链表）
	waitsince    int64          // 注释：g被阻塞的时间 // approx time when the g become blocked
	waitreason   waitReason     // 注释：g被阻塞的原因 // if status==Gwaiting
	// 注释：每个G都有三个与抢占有关的字段，分别为preempt、preemptStop和premptShrink
	preempt       bool // 注释：标记是否可抢占,其值为true执行 stackguard0 = stackpreempt。(抢占调度标志，如果需要抢占调度，设置preempt为true) // preemption signal, duplicates stackguard0 = stackpreempt
	preemptStop   bool // 注释：将抢占标记修改为_Gpreedmpted，如果修改失败则取消 // transition to _Gpreempted on preemption; otherwise, just deschedule
	preemptShrink bool // 注释：在同步安全点收缩栈 // shrink stack at synchronous safe point

	// asyncSafePoint is set if g is stopped at an asynchronous
	// safe point. This means there are frames on the stack
	// without precise pointer information.
	asyncSafePoint bool // 注释：异步安全点；如果G在异步安全点停止则设置为true，表示在栈上没有精确的指针信息

	paniconfault bool // 注释：地址异常引起的panic（代替了崩溃） // panic (instead of crash) on unexpected fault address
	gcscandone   bool // 注释：g扫描完了栈，受状态_Gscan位保护。 // g has scanned stack; protected by _Gscan bit in status
	throwsplit   bool // 注释：不允许拆分stack // must not split stack
	// activeStackChans indicates that there are unlocked channels
	// pointing into this goroutine's stack. If true, stack
	// copying needs to acquire channel locks to protect these
	// areas of the stack.
	// 注释：表示是否有未加锁定的channel指向到了G栈，如果为true,那么对栈的复制需要channal锁来保护这些区域
	activeStackChans bool // 注释：比较是否处于活动的栈里，false表示管道不在活动的栈空间里，已经让渡控制权了（阻塞了）。唤醒时会设置为true，表示管道处于活动的栈空间里，复制栈空间时会有判断
	// parkingOnChan indicates that the goroutine is about to
	// park on a chansend or chanrecv. Used to signal an unsafe point
	// for stack shrinking. It's a boolean value, but is updated atomically.
	parkingOnChan uint8 // 注释：1表示G放在管道读取队列（c.recvq）或写入队列（c.sendq）里。用于栈的收缩，是一个布尔值，但是原子性更新

	raceignore     int8     // ignore race detection events
	sysblocktraced bool     // 注释：（系统调用时为true,其他情况为false）标记开始系统调用的栈追踪 // StartTrace has emitted EvGoInSyscall about this goroutine
	sysexitticks   int64    // cputicks when syscall has returned (for tracing)
	traceseq       uint64   // trace event sequencer
	tracelastp     puintptr // last P emitted an event for this goroutine
	lockedm        muintptr // 注释：g被锁定,只在这个m上运行
	sig            uint32
	writebuf       []byte
	sigcode0       uintptr
	sigcode1       uintptr
	sigpc          uintptr
	gopc           uintptr         // 注释：创建当前G的PC(调用者的PC(rip)) 例如：A调用B然后执行go指令，此时gopc是A的PC值 // pc of go statement that created this goroutine
	ancestors      *[]ancestorInfo // 注释：创建此g的祖先信息g仅在debug.traceback祖先时使用 // ancestor information goroutine(s) that created this goroutine (only used if debug.tracebackancestors)
	startpc        uintptr         // 注释：任务函数(go func(){}中指令对应的pc值) // pc of goroutine function
	racectx        uintptr
	waiting        *sudog         // 注释：等待的sudog链表头指针  // sudog structures this g is waiting on (that have a valid elem ptr); in lock order
	cgoCtxt        []uintptr      // cgo traceback context
	labels         unsafe.Pointer // 注释：探测器标签，用于pprof使用 // profiler labels
	timer          *timer         // 注释：通过time.Sleep缓存timer // cached timer for time.Sleep
	selectDone     uint32         // are we participating in a select and did someone win the race?

	// Per-G GC state

	// gcAssistBytes is this G's GC assist credit in terms of
	// bytes allocated. If this is positive, then the G has credit
	// to allocate gcAssistBytes bytes without assisting. If this
	// is negative, then the G must correct this by performing
	// scan work. We track this in bytes to make it fast to update
	// and check for debt in the malloc hot path. The assist ratio
	// determines how this corresponds to scan work debt.
	// 注释：gcAssistBytes是根据分配的字节数计算的G的GC辅助信用。如果这是肯定的，那么G可以在没有辅助的情况下分配gcAssistBytes字节。 如果结果为阴性，则G必须通过执行扫描工作来纠正此问题。
	//		我们以字节为单位跟踪它，以便在malloc热路径中快速更新和检查债务。协助比率决定了这与扫描工作债务的对应程度。
	gcAssistBytes int64 // 注释：与GC相关
}

// 注释：m结构体用来代表工作线程，它保存了m自身使用的栈信息，当前正在运行的goroutine以及与m绑定的p等信息
// 注释：m有3个链表分别是alllink,schedlink,freelink
type m struct {
	g0      *g     // goroutine with scheduling stack // 注释：g0主要用来记录工作线程m使用的栈信息，在执行调度代码时需要使用这个栈，执行用户g代码时，使用用户g自己的栈，调度时会发生栈的切换
	morebuf gobuf  // gobuf arg to morestack
	divmod  uint32 // div/mod denominator for arm - known to liblink

	// Fields not known to debuggers.
	procid     uint64       // 注释：p的ID,用来调试时使用,一般是协成ID，初始化m时是线程ID // for debuggers, but offset not hard-coded
	gsignal    *g           // 注释：M中正在处理信号的G(信号处理) // signal-handling g
	goSigStack gsignalStack // Go-allocated signal handling stack
	sigmask    sigset       // storage for saved signal mask
	// 注释：go在新建M时候设置FS寄存器的值为M.tls的地址，运行中每个M的FS寄存器都会指向对应的M.tls，内核调度线程时FS寄存器会跟着线程一起切换，这样go代码只需要访问FS寄存器就可以获取到线程本地的数据
	tls      [6]uintptr // 注释：通过TLS实现m结构体对象与工作线程之间的绑定,第一个元素是g(程序当前运行的g) // thread-local storage (for x86 extern register)
	mstartfn func()     // 注释：(起始函数)启动m（mstart）时执行的函数，如果不等于nil就执行

	curg          *g       // 注释：指向工作线程m正在运行的g结构体对象,要确定g是在用户堆栈还是系统堆栈上运行，可以使用if getg() == getg().m.curg {用户态堆栈} else {系统堆栈} // current running goroutine
	caughtsig     guintptr // goroutine running during fatal signal
	p             puintptr // 注释：记录与当前工作线程绑定的p结构体对象 // attached p for executing go code (nil if not executing go code)
	nextp         puintptr // 注释：(下一个要执行的P)新线程m要绑定的p（起始任务函数）(其他的m给新m设置该字段，当新m启动时会和当前字段的p进行绑定),其他M把P抢走后会设置这个字段告诉当前M如果执行时应该绑定其他的P
	oldp          puintptr // 注释：在系统调用的时候把当前的P存放到这里，系统调用结束后拿出来 // the p that was attached before executing a syscall // 注释：在执行系统调用之前附加的p
	id            int64    // 注释：M的ID
	mallocing     int32    // 注释：正在申请内存标识(0否1是)，当申请内存的开头会检查这个字段，如果已经在申请了，则报错，
	throwing      int32    // 注释：-1不要转储完整的堆栈,大于0时:存储完整的堆栈（用于栈追踪使用）
	preemptoff    string   // if != "", keep curg running on this m
	locks         int32    // 注释：给M加锁;(禁用抢占)大于0时说明正在g正在被使用，系统调用后置函数的时候有使用（获取时++，释放是--）
	dying         int32
	profilehz     int32
	spinning      bool // 注释：（我开始要抢别人了）是否自旋，自旋就表示M正在找G来运行，表示当前工作线程m正在试图从其它工作线程m的本地运行队列偷取g // m is out of work and is actively looking for work
	blocked       bool // 注释：m是否被阻塞 // m is blocked on a note
	newSigstack   bool // minit on C thread called sigaltstack
	printlock     int8
	incgo         bool      // 注释： m在执行cgo吗 // m is executing a cgo call
	freeWait      uint32    // if == 0, safe to free g0 and delete m (atomic)
	fastrand      [2]uint32 // 注释：(快速随机数时使用)快速随机数的基础数，程序初始化（schedinit）或创建M（allocm）时设置，随机数是基于这两个数计算出来的，计算完成后重新回填到这两个数里
	needextram    bool
	traceback     uint8                         // 注释：堆栈追踪级别（用于栈追踪时使用）
	ncgocall      uint64                        // 注释：cgo调用的总数 // number of cgo calls in total
	ncgo          int32                         // 注释：当前cgo调用的数目 // number of cgo calls currently in progress
	cgoCallersUse uint32                        // if non-zero, cgoCallers in use temporarily
	cgoCallers    *cgoCallers                   // cgo traceback if crashing in cgo call
	doesPark      bool                          // 注释：是否使用park // non-P running threads: sysmon and newmHandoff never use .park
	park          note                          // 注释：没有g需要运行时，工作线程M睡眠在这个park成员上，其它线程通过这个park唤醒该工作线程
	alllink       *m                            // 注释：记录所有工作线程m的一个链表 // on allm
	schedlink     muintptr                      // 注释：空闲的m链表（由sched.midle指向）
	lockedg       guintptr                      // 注释：m下指定执行的g(m里锁定的g),lockedg有值说明m绑定的p被别的m抢走了，如果lockedg有值就要执行这里的g
	createstack   [32]uintptr                   // stack that created this thread.
	lockedExt     uint32                        // tracking for external LockOSThread
	lockedInt     uint32                        // tracking for internal lockOSThread
	nextwaitm     muintptr                      // next m waiting for lock
	waitunlockf   func(*g, unsafe.Pointer) bool // 注释：(解除等待钩子)解除等待函数，G0(系统栈-系统协成执)行完成后会调用该函数(函数执行万成后会清空)
	waitlock      unsafe.Pointer                // 注释：(解除等待钩子)解除等待函数参数，(函数执行万成后会清空)
	waittraceev   byte                          // 注释：等待追踪事件类型
	waittraceskip int                           // 注释：跳过几层事件追踪的结果（事件追踪结果中从哪一级返回数据，跳过的是不重要的）
	startingtrace bool                          // 注释：是否已经开始栈追踪
	syscalltick   uint32                        // 注释：保存P里的系统调度计数器，P每一次系统调用加1
	freelink      *m                            // on sched.freem // 注释：对应freem的链表(freelink->sched.freem)

	// mFixup is used to synchronize OS related m state
	// (credentials etc) use mutex to access. To avoid deadlocks
	// an atomic.Load() of used being zero in mDoFixupFn()
	// guarantees fn is nil.
	mFixup struct {
		lock mutex
		used uint32
		fn   func(bool) bool
	}

	// these are here because they are too large to be on the stack
	// of low-level NOSPLIT functions.
	libcall   libcall
	libcallpc uintptr // for cpu profiler
	libcallsp uintptr
	libcallg  guintptr
	syscall   libcall // stores syscall parameters on windows

	vdsoSP uintptr // SP for traceback while in VDSO call (0 if not in call)
	vdsoPC uintptr // PC for traceback while in VDSO call

	// preemptGen counts the number of completed preemption
	// signals. This is used to detect when a preemption is
	// requested, but fails. Accessed atomically.
	preemptGen uint32

	// Whether this is a pending preemption signal on this M.
	// Accessed atomically.
	signalPending uint32

	dlogPerM

	mOS

	// Up to 10 locks held by this m, maintained by the lock ranking code.
	locksHeldLen int
	locksHeld    [10]heldLockInfo
}

// 注释：P(处理器)结构体用于保存工作线程m执行go代码时所必需的资源，比如goroutine的运行队列，内存分配用到的缓存等等
type p struct {
	id          int32      // 注释：id也是allp的数组下标
	status      uint32     // one of pidle/prunning/...
	link        puintptr   // 注释：空闲p链表的下一个p指针
	schedtick   uint32     // 注释：用户调度计数器，每次调度的时候加1 // incremented on every scheduler call
	syscalltick uint32     // 注释：系统调度计数器，每一次系统调用加1 // incremented on every system call
	sysmontick  sysmontick // 注释：系统监控 // last tick observed by sysmon
	m           muintptr   // 回链到关联的m // back-link to associated m (nil if idle)
	mcache      *mcache    // 注释：记录申请(分配)内存的虚拟内存span的缓存，由于G同时只能在一个逻辑处理器P上运行，所已这个不需要锁
	pcache      pageCache
	raceprocctx uintptr

	deferpool    [5][]*_defer // pool of available defer structs of different sizes (see panic.go)
	deferpoolbuf [5][32]*_defer

	// Cache of goroutine ids, amortizes accesses to runtime·sched.goidgen.
	goidcache    uint64
	goidcacheend uint64

	// Queue of runnable goroutines. Accessed without lock.
	// 注释：本地g运行队列(用数组实现队列)
	runqhead uint32        // 注释：本地g队列(数组)runq的头下标
	runqtail uint32        // 注释：本地g队列(数组)runq的尾下标(如果队列装满(runqtail-runqhead)==len(runq)时会把本地队列的G的一半放到全局队列中)
	runq     [256]guintptr // 注释：本地g的指针队列，使用数组实现的循环队列
	// runnext, if non-nil, is a runnable G that was ready'd by
	// the current G and should be run next instead of what's in
	// runq if there's time remaining in the running G's time
	// slice. It will inherit the time left in the current time
	// slice. If a set of goroutines is locked in a
	// communicate-and-wait pattern, this schedules that set as a
	// unit and eliminates the (potentially large) scheduling
	// latency that otherwise arises from adding the ready'd
	// goroutines to the end of the run queue.
	runnext guintptr // 注释：g队列里的下一个指针，下一个运行的g，优先级最高

	// Available G's (status == Gdead)
	// 注释：本地空G队列
	// 注释：如果n>=64则会把本地P空G拿出一半（32个）放到全局空闲队列里,执行方法是：func gfput(_p_ *p, gp *g) {}
	// 注释：如果n==0则会到全局空闲列表里拿回32个,执行方法是：func gfget(_p_ *p) *g {}
	gFree struct { // 注释：空G队列
		gList       // 注释：空G的头指针
		n     int32 // 注释：空G的个数，最大是64程序控制。
	}

	sudogcache []*sudog // 注释：P中空闲G的切片（把要释放掉的G会缓存到这里），如果为空时则全局G缓存链表取出当前缓存的一半放进来，如果全局缓存为空，则会新new一个空的G放进来
	sudogbuf   [128]*sudog

	// Cache of mspan objects from the heap.
	mspancache struct {
		// We need an explicit length here because this field is used
		// in allocation codepaths where write barriers are not allowed,
		// and eliminating the write barrier/keeping it eliminated from
		// slice updates is tricky, moreso than just managing the length
		// ourselves.
		len int
		buf [128]*mspan
	}

	tracebuf traceBufPtr // 注释：存放栈追踪的栈缓冲区地址

	// traceSweep indicates the sweep events should be traced.
	// This is used to defer the sweep start event until a span
	// has actually been swept.
	// 注释：译：traceSweep表示应该跟踪扫描事件。这用于推迟扫描开始事件，直到实际扫描了一个跨度。
	traceSweep bool // 注释：清理是是否需要链路追踪
	// traceSwept and traceReclaimed track the number of bytes
	// swept and reclaimed by sweeping in the current sweep loop.
	// 注释：译：traceSwept和traceReclaimed跟踪当前扫描循环中通过扫描扫描和回收的字节数。
	traceSwept, traceReclaimed uintptr // 注释：traceSwept是清理时需要链路追踪的字节,0代表链路追踪的开始；traceReclaimed是回收的字节

	palloc persistentAlloc // per-P to avoid mutex

	_ uint32 // Alignment for atomic fields below

	// The when field of the first entry on the timer heap.
	// This is updated using atomic functions.
	// This is 0 if the timer heap is empty.
	timer0When uint64

	// The earliest known nextwhen field of a timer with
	// timerModifiedEarlier status. Because the timer may have been
	// modified again, there need not be any timer with this value.
	// This is updated using atomic functions.
	// This is 0 if there are no timerModifiedEarlier timers.
	timerModifiedEarliest uint64

	// Per-P GC state
	gcAssistTime         int64 // Nanoseconds in assistAlloc
	gcFractionalMarkTime int64 // Nanoseconds in fractional mark worker (atomic)

	// gcMarkWorkerMode is the mode for the next mark worker to run in.
	// That is, this is used to communicate with the worker goroutine
	// selected for immediate execution by
	// gcController.findRunnableGCWorker. When scheduling other goroutines,
	// this field must be set to gcMarkWorkerNotWorker.
	gcMarkWorkerMode gcMarkWorkerMode
	// gcMarkWorkerStartTime is the nanotime() at which the most recent
	// mark worker started.
	gcMarkWorkerStartTime int64

	// gcw is this P's GC work buffer cache. The work buffer is
	// filled by write barriers, drained by mutator assists, and
	// disposed on certain GC state transitions.
	gcw gcWork

	// wbBuf is this P's GC write barrier buffer.
	//
	// TODO: Consider caching this in the running G.
	wbBuf wbBuf

	runSafePointFn uint32 // 注释：(以避免发生竞争)是否有安全节点检查函数0否1是，如果是1则执行安全节点函数 // if 1, run sched.safePointFn at next safe point // 注释：如果为1，则在下一个安全点运行sched.safePointFn

	// statsSeq is a counter indicating whether this P is currently
	// writing any stats. Its value is even when not, odd when it is.
	// 注释：译：statsSeq是一个计数器，指示此P当前是否正在写入任何统计数据。它的值不是偶数时为偶数，是奇数时为奇数。
	statsSeq uint32 // 注释：给consistentHeapStats加锁（这个字段是P计数器锁，奇数是给consistentHeapStats加锁，偶数是给consistentHeapStats解锁）memstats.heapStats.acquire()加锁，release解锁

	// Lock for timers. We normally access the timers while running
	// on this P, but the scheduler can also do it from a different P.
	timersLock mutex

	// Actions to take at some time. This is used to implement the
	// standard library's time package.
	// Must hold timersLock to access.
	timers []*timer

	// Number of timers in P's heap.
	// Modified using atomic instructions.
	numTimers uint32

	// Number of timerDeleted timers in P's heap.
	// Modified using atomic instructions.
	deletedTimers uint32

	// Race context used while executing timer functions.
	timerRaceCtx uintptr

	// preempt is set to indicate that this P should be enter the
	// scheduler ASAP (regardless of what G is running on it).
	preempt bool // 注释：标记P上的G是异步抢占

	pad cpu.CacheLinePad
}

// 注释：全局变量
// 注释：调度器结构体对象，记录了调度器的工作状态
// 注释：记录调度器的状态和g的全局运行队列：
type schedt struct {
	// accessed atomically. keep at top to ensure alignment on 32-bit systems.
	goidgen   uint64
	lastpoll  uint64 // time of last network poll, 0 if currently polling
	pollUntil uint64 // time to which current poll is sleeping

	lock mutex // 注释：锁（把局部P加入全局P队列会用到，修改的字段是"runq和runqsize"）(系统调用时也会用到)

	// When increasing nmidle, nmidlelocked, nmsys, or nmfreed, be
	// sure to call checkdead().

	midle        muintptr // idle m's waiting for work               // 注释：由空闲的工作线程m组成链表(midle和m.schedlink组成的链表)(midle值是m,m中的m.schedlink连接下一个midle)
	nmidle       int32    // number of idle m's waiting for work     // 注释：空闲的工作线程m的数量
	nmidlelocked int32    // number of locked m's waiting for work
	mnext        int64    // number of m's that have been created and next M ID // 注释：下一个新m的主键ID值(用来创建新m时使用)
	maxmcount    int32    // maximum number of m's allowed (or die)  // 注释：最多只能创建maxmcount个工作线程m
	nmsys        int32    // number of system m's not counted for deadlock
	nmfreed      int64    // cumulative number of freed m's

	ngsys uint32 // number of system goroutines; updated atomically

	pidle      puintptr // idle p's // 注释：由空闲的p结构体对象组成的链表(这里指向的链表的头部)
	npidle     uint32   // 注释：空闲的p结构体对象的数量
	nmspinning uint32   // 注释：自旋的线程m数量（表示runtime中一共有多少个M在自旋状态）(自旋说明当前线程M已经没有需要执行的G，正在打算去其他线程M偷G了) // See "Worker thread parking/unparking" comment in proc.go.

	// Global runnable queue. // 注释：全局可运行队列
	// 注释：如果创建一个g并准备运行，这个g就会被放到调度器的全局运行队列中。
	// 注释：之后，调度器就将这些队列中的g分配给一个逻辑处理器P，并放到这个逻辑处理器P对应的本地运行队列中。本地运行队列中的g会一直等待，直到自己被分配的逻辑处理器执行。
	runq     gQueue // 注释：全局g运行队列
	runqsize int32  // 注释：全局g队列的成员个数

	// disable controls selective disabling of the scheduler.
	//
	// Use schedEnableUser to control this.
	//
	// disable is protected by sched.lock.
	disable struct {
		// user disables scheduling of user goroutines.
		user     bool
		runnable gQueue // pending runnable Gs
		n        int32  // length of runnable
	}

	// Global cache of dead G's.
	// 注释：（全局空G队列）gFree是所有已经退出的goroutine对应的g结构体对象组成的链表，用于缓存g结构体对象，避免每次创建goroutine时都重新分配内存
	// 注释：获取的时候先到stack里拿，如果stack没有则去noStack里拿，拿成功后n减1
	// 注释：把本地P上空G放到全局空G的链表里,函数是：func gfpurge(_p_ *p) { }
	gFree struct {
		lock    mutex // 注释：变更是加锁
		stack   gList // 注释：有占空间的空G(栈顶指针为空gp.stack.lo != 0) // Gs with stacks
		noStack gList // 注释：没有栈空间的空G(栈顶指针为空gp.stack.lo == 0) // Gs without stacks
		n       int32 // 注释：stack和noStack链表里空G的数量
	}

	// 注释：中央缓存(空闲G的单向链表) // Central cache of sudog structs.
	sudoglock  mutex  // 注释：全局空闲G的锁，当前P中的G列表为空时会上锁，然后取出一批
	sudogcache *sudog // 注释：全局空闲G(中央缓存)链表头指针(单向链表)，每次释放都会放到这个单向链表的头部，获取是当P（本地缓存）没有空闲G时会到这里取出本地缓存的一半；释放时正好相反

	// Central pool of available defer structs of different sizes.
	deferlock mutex
	deferpool [5]*_defer

	// freem is the list of m's waiting to be freed when their
	// m.exited is set. Linked through m.freelink.
	freem *m

	gcwaiting  uint32 // 注释：是否需要GC等待，0否1是，默认0（在GC发起后就处于等待阶段，需要把所有的P(P的数量默认是系统核数)都停止后执行GC，GC启动后会设置成1） // gc is waiting to run
	stopwait   int32  // 注释：停止等待，默认值是P的个数，如果等于0代表所有的P都被停止了(一般用于STW的时候判断是否全部停止了，然后执行后续操作)。冻结(类似于STW，但尽了最大努力，可以叫几次)时值为一个很大的值(const freezeStopWait)，STW时减1,
	stopnote   note   // 注释：用于处理GC的M节点，当STW把所有的P都停止后唤醒，默认是停止状态
	sysmonwait uint32 // 注释：(系统监控)是否有等待的M,0否，1是
	sysmonnote note   // 注释：如果有等待的M，则唤醒M并且把sysmonwait设置为0

	// While true, sysmon not ready for mFixup calls.
	// Accessed atomically.
	sysmonStarting uint32

	// safepointFn should be called on each P at the next GC
	// safepoint if p.runSafePointFn is set.
	// 注释：如果设置了P.runSafePointFn，则应在下一个GC安全点对每个P调用安全点Fn。
	safePointFn   func(*p) // 注释：执行安全节点函数，把当前P放进去，检查是否有数据冲突（检测数据竞争）
	safePointWait int32    // 注释：安全节点等待数，默认是 gomaxprocs - 1，执行一个安全节点检查后递减
	safePointNote note     //  注释：等待被唤醒的节点M，安全节点数safePointWait为0时唤醒

	profilehz int32 // cpu profiling rate

	procresizetime int64 // nanotime() of last change to gomaxprocs
	totaltime      int64 // ∫gomaxprocs dt up to procresizetime

	// sysmonlock protects sysmon's actions on the runtime.
	//
	// Acquire and hold this mutex to block sysmon from interacting
	// with the rest of the runtime.
	sysmonlock mutex
}

// Values for the flags field of a sigTabT.
const (
	_SigNotify   = 1 << iota // let signal.Notify have signal, even if from kernel
	_SigKill                 // if signal.Notify doesn't take it, exit quietly
	_SigThrow                // if signal.Notify doesn't take it, exit loudly
	_SigPanic                // if the signal is from the kernel, panic
	_SigDefault              // if the signal isn't explicitly requested, don't monitor it
	_SigGoExit               // cause all runtime procs to exit (only used on Plan 9).
	_SigSetStack             // add SA_ONSTACK to libc handler
	_SigUnblock              // always unblock; see blockableSig
	_SigIgn                  // _SIG_DFL action is to ignore the signal
)

// Layout of in-memory per-function information prepared by linker
// See https://golang.org/s/go12symtab.
// Keep in sync with linker (../cmd/link/internal/ld/pcln.go:/pclntab)
// and with package debug/gosym and with symtab.go in package runtime.
// 注释：译：链接器准备的内存中每个函数信息的布局请参阅https://golang.org/s/go12symtab.
//		在包运行时与链接器（../cmd/link/internal/ld/pcln.go:/pclntab）、包调试/gosym和symtab.go保持同步。
// 函数方法的基础信息
type _func struct {
	entry   uintptr // start pc
	nameoff int32   // 注释：方法名称头指针偏移量，这个方法名称存储在一个大的C字符串中，多个用0分隔，这里记录C字符串头指针的偏移量 // function name

	args        int32  // in/out args size
	deferreturn uint32 // offset of start of a deferreturn call instruction from entry, if any.

	pcsp      uint32
	pcfile    uint32
	pcln      uint32
	npcdata   uint32
	cuOffset  uint32  // runtime.cutab offset of this function's CU
	funcID    funcID  // set for certain special runtime functions
	_         [2]byte // pad
	nfuncdata uint8   // must be last
}

// Pseudo-Func that is returned for PCs that occur in inlined code.
// A *Func can be either a *_func or a *funcinl, and they are distinguished
// by the first uintptr.
type funcinl struct {
	zero  uintptr // set to 0 to distinguish from _func
	entry uintptr // entry of the real (the "outermost") frame.
	name  string
	file  string
	line  int
}

// layout of Itab known to compilers
// allocated in non-garbage-collected memory
// Needs to be in sync with
// ../cmd/compile/internal/gc/reflect.go:/^func.dumptabs.
// 注释：接口的核心结构
type itab struct {
	inter *interfacetype // 注释：接口本身的类型
	_type *_type         // 注释：接口存储的动态类型（具体类型）
	// 注释：哈希是动态类型的唯一标识,它是_type类型中hash的副本
	// 注释：哈希在接口类型断言时，可以使用该字段快速判断接口动态类型与具体类型_type是否一致
	hash uint32  // copy of _type.hash. Used for type switches.
	_    [4]byte // 注释：4字节用来内存对齐
	// 注释：接口动态类型中的函数指针列表，用户运行时接口调用动态函数，这里虽然在运行时只定义了大小为1的数组[1]uintptr，但是其存储的是函数首地址的指针。
	// 注释：当有多个函数时，其指针会依次在下方进行存储。在运行时，可以通过首地址+偏移找到任意的函数指针。
	fun [1]uintptr // variable sized. fun[0]==0 means _type does not implement inter.
}

// Lock-free stack node.
// Also known to export_test.go.
type lfnode struct {
	next    uint64
	pushcnt uintptr
}

type forcegcstate struct {
	lock mutex
	g    *g
	idle uint32
}

// extendRandom extends the random numbers in r[:n] to the whole slice r.
// Treats n<0 as n==0.
func extendRandom(r []byte, n int) {
	if n < 0 {
		n = 0
	}
	for n < len(r) {
		// Extend random bits using hash function & time seed
		w := n
		if w > 16 {
			w = 16
		}
		h := memhash(unsafe.Pointer(&r[n-w]), uintptr(nanotime()), uintptr(w))
		for i := 0; i < sys.PtrSize && n < len(r); i++ {
			r[n] = byte(h)
			n++
			h >>= 8
		}
	}
}

// A _defer holds an entry on the list of deferred calls.
// If you add a field here, add code to clear it in freedefer and deferProcStack
// This struct must match the code in cmd/compile/internal/gc/reflect.go:deferstruct
// and cmd/compile/internal/gc/ssa.go:(*state).call.
// Some defers will be allocated on the stack and some on the heap.
// All defers are logically part of the stack, so write barriers to
// initialize them are not required. All defers must be manually scanned,
// and for heap defers, marked.
// 注释：延迟调用的结构体
type _defer struct {
	siz     int32 // 注释：存放参数和返回值的内存大小 // includes both arguments and results
	started bool
	heap    bool // 注释：是否存储在堆上,true代表在堆上，false代表在栈上
	// openDefer indicates that this _defer is for a frame with open-coded
	// defers. We have only one defer record for the entire frame (which may
	// currently have 0, 1, or more defers active).
	// 注释：open defer优化条件，当前函数小于等于8个defer，或者return个数 * defer个数 < 15个，才被优化到函数尾部，有点像函数内联
	openDefer bool     // 注释：表示当前 defer 是否经过开放编码的优化(优化大题意思和函数内联很相似，就是把defer放到当前函数末尾位置，然后用一个8位的二进制标记运行时是否需要执行defer)
	sp        uintptr  // 注释：sp寄存器的值，栈指针 // sp at time of defer
	pc        uintptr  // 注释：pc寄存器的值，调用方的程序计数器 // pc at time of defer
	fn        *funcval // 注释：传入的函数，就是defer要执行的函数地址 // can be nil for open-coded defers
	_panic    *_panic  // 注释：是触发延迟调用的结构体，可能为空 // panic that is running defer
	link      *_defer  // 注释：defer的链表地址

	// If openDefer is true, the fields below record values about the stack
	// frame and associated function that has the open-coded defer(s). sp
	// above will be the sp for the frame, and pc will be address of the
	// deferreturn call in the function.
	fd   unsafe.Pointer // funcdata for the function associated with the frame
	varp uintptr        // value of varp for the stack frame
	// framepc is the current pc associated with the stack frame. Together,
	// with sp above (which is the sp associated with the stack frame),
	// framepc/sp can be used as pc/sp pair to continue a stack trace via
	// gentraceback().
	// 注释：framepc是与堆栈帧关联的当前pc。再加上上面的sp（与堆栈帧关联的sp），framepc/sp可以用作pc/sp对，通过gentraceback（）继续堆栈跟踪。
	framepc uintptr // 注释：
}

// A _panic holds information about an active panic.
// 注释：_panic保存活动的panic信息
// A _panic value must only ever live on the stack.
// 注释：_panic的值只保存在栈中
// The argp and link fields are stack pointers, but don't need special
// handling during stack growth: because they are pointer-typed and
// _panic values only live on the stack, regular stack pointer
// adjustment takes care of them.
// 注释：panic的结构体（panic是记录在G里的）
// 注释：字段argp很有意思代表：发生panic的时候回触发refer函数，在为调refer函数准备的参数栈地址（调用下一个函数的参数和返回值，又上一个函数栈准备，这里就是这个站准备的参数的站地址）
// 注释：当recover的时候会接受到入参argp，然后和存储的panic.argp进行比较，如果比较不通过的时候无法获取recover的数据
type _panic struct {
	argp      unsafe.Pointer // 注释：指向defer调用时参数的指针(调用refer前的的参数指针，给refer准备的参数对应的指针) // pointer to arguments of deferred call run during panic; cannot move - known to liblink
	arg       interface{}    // 注释：panic的参数，打印panic的内容 // argument to panic
	link      *_panic        // 注释：panic的单向链表  // link to earlier panic
	pc        uintptr        // 注释：recover的时候,如果绕过painc的时候需要继续之前的pc位置执行 // where to return to in runtime if this panic is bypassed
	sp        unsafe.Pointer // 注释：recover的时候,如果绕过painc的时候需要继续之前的sp栈位置 // where to return to in runtime if this panic is bypassed
	recovered bool           // 注释：当前是否被recover恢复 // whether this panic is over
	aborted   bool           // 注释：当前是否被强行终止(当defer里发生panin的时候会把上一个panic.aborted设置成true) // the panic was aborted
	goexit    bool           // 注释：是否是推出函数发生的panic
}

// stack traces
// 注释：堆栈跟踪
type stkframe struct {
	fn       funcInfo   // function being run
	pc       uintptr    // program counter within fn
	continpc uintptr    // program counter where execution can continue, or 0 if not
	lr       uintptr    // program counter at caller aka link register
	sp       uintptr    // stack pointer at pc
	fp       uintptr    // stack pointer at caller aka frame pointer
	varp     uintptr    // top of local variables
	argp     uintptr    // pointer to function arguments
	arglen   uintptr    // number of bytes at argp
	argmap   *bitvector // force use of this argmap
}

// ancestorInfo records details of where a goroutine was started.
type ancestorInfo struct {
	pcs  []uintptr // pcs from the stack of this goroutine
	goid int64     // goroutine id of this goroutine; original goroutine possibly dead
	gopc uintptr   // pc of go statement that created this goroutine
}

const (
	_TraceRuntimeFrames = 1 << iota // include frames for internal runtime functions.
	_TraceTrap                      // the initial PC, SP are from a trap, not a return PC from a call
	_TraceJumpStack                 // if traceback is on a systemstack, resume trace at g that called into it
)

// The maximum number of frames we print for a traceback
const _TracebackMaxFrames = 100

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

// 注释：等待锁的原因
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
	waitReasonTimerGoroutineIdle                      // "timer goroutine (idle)"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonPreempted                               // "preempted"
	waitReasonDebugCall                               // "debug call"
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
	waitReasonTimerGoroutineIdle:    "timer goroutine (idle)",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonPreempted:             "preempted",
	waitReasonDebugCall:             "debug call",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

var (
	// 注释：全局变量
	allm       *m    // 注释：(全局变量)所有的m构成的一个链表，包括下面的m0
	gomaxprocs int32 // 注释：(全局变量)p的最大值，默认等于ncpu，但可以通过GOMAXPROCS修改
	ncpu       int32 // 注释：(全局变量)系统中cpu核的数量，程序启动时由runtime代码初始化
	forcegc    forcegcstate
	sched      schedt // 注释：(全局变量)调度器结构体对象，记录了调度器的工作状态
	newprocs   int32

	// allpLock protects P-less reads and size changes of allp, idlepMask,
	// and timerpMask, and all writes to allp.
	allpLock mutex
	// len(allp) == gomaxprocs; may change at safe points, otherwise
	// immutable.
	allp []*p // 注释：(全局变量)保存所有的p，len(allp) == gomaxprocs
	// Bitmask of Ps in _Pidle list, one bit per P. Reads and writes must
	// be atomic. Length may change at safe points.
	//
	// Each P must update only its own bit. In order to maintain
	// consistency, a P going idle must the idle mask simultaneously with
	// updates to the idle P list under the sched.lock, otherwise a racing
	// pidleget may clear the mask before pidleput sets the mask,
	// corrupting the bitmap.
	//
	// N.B., procresize takes ownership of all Ps in stopTheWorldWithSema.
	idlepMask pMask
	// Bitmask of Ps that may have a timer, one bit per P. Reads and writes
	// must be atomic. Length may change at safe points.
	timerpMask pMask

	// Pool of GC parked background workers. Entries are type
	// *gcBgMarkWorkerNode.
	gcBgMarkWorkerPool lfstack

	// Total number of gcBgMarkWorker goroutines. Protected by worldsema.
	gcBgMarkWorkerCount int32

	// Information about what cpu features are available.
	// Packages outside the runtime should not use these
	// as they are not an external api.
	// Set on startup in asm_{386,amd64}.s
	processorVersionInfo uint32
	isIntel              bool
	lfenceBeforeRdtsc    bool

	goarm uint8 // set by cmd/link on arm systems
)

// Set by the linker so the runtime can determine the buildmode.
var (
	islibrary bool // -buildmode=c-shared
	isarchive bool // -buildmode=c-archive
)

// Must agree with cmd/internal/objabi.Framepointer_enabled.
const framepointer_enabled = GOARCH == "amd64" || GOARCH == "arm64" && (GOOS == "linux" || GOOS == "darwin" || GOOS == "ios")
