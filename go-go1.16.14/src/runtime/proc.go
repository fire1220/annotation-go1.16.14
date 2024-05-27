// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"internal/bytealg"
	"internal/cpu"
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

var buildVersion = sys.TheVersion

// set using cmd/go/internal/modload.ModInfoProg
var modinfo string

// Goroutine scheduler
// The scheduler's job is to distribute ready-to-run goroutines over worker threads.
//
// The main concepts are:
// G - goroutine.
// M - worker thread, or machine.
// P - processor, a resource that is required to execute Go code.
//     M must have an associated P to execute Go code, however it can be
//     blocked or in a syscall w/o an associated P.
//
// Design doc at https://golang.org/s/go11sched.

// Worker thread parking/unparking.
// We need to balance between keeping enough running worker threads to utilize
// available hardware parallelism and parking excessive running worker threads
// to conserve CPU resources and power. This is not simple for two reasons:
// (1) scheduler state is intentionally distributed (in particular, per-P work
// queues), so it is not possible to compute global predicates on fast paths;
// (2) for optimal thread management we would need to know the future (don't park
// a worker thread when a new goroutine will be readied in near future).
//
// Three rejected approaches that would work badly:
// 1. Centralize all scheduler state (would inhibit scalability).
// 2. Direct goroutine handoff. That is, when we ready a new goroutine and there
//    is a spare P, unpark a thread and handoff it the thread and the goroutine.
//    This would lead to thread state thrashing, as the thread that readied the
//    goroutine can be out of work the very next moment, we will need to park it.
//    Also, it would destroy locality of computation as we want to preserve
//    dependent goroutines on the same thread; and introduce additional latency.
// 3. Unpark an additional thread whenever we ready a goroutine and there is an
//    idle P, but don't do handoff. This would lead to excessive thread parking/
//    unparking as the additional threads will instantly park without discovering
//    any work to do.
//
// The current approach:
// We unpark an additional thread when we ready a goroutine if (1) there is an
// idle P and there are no "spinning" worker threads. A worker thread is considered
// spinning if it is out of local work and did not find work in global run queue/
// netpoller; the spinning state is denoted in m.spinning and in sched.nmspinning.
// Threads unparked this way are also considered spinning; we don't do goroutine
// handoff so such threads are out of work initially. Spinning threads do some
// spinning looking for work in per-P run queues before parking. If a spinning
// thread finds work it takes itself out of the spinning state and proceeds to
// execution. If it does not find work it takes itself out of the spinning state
// and then parks.
// If there is at least one spinning thread (sched.nmspinning>1), we don't unpark
// new threads when readying goroutines. To compensate for that, if the last spinning
// thread finds work and stops spinning, it must unpark a new spinning thread.
// This approach smooths out unjustified spikes of thread unparking,
// but at the same time guarantees eventual maximal CPU parallelism utilization.
//
// The main implementation complication is that we need to be very careful during
// spinning->non-spinning thread transition. This transition can race with submission
// of a new goroutine, and either one part or another needs to unpark another worker
// thread. If they both fail to do that, we can end up with semi-persistent CPU
// underutilization. The general pattern for goroutine readying is: submit a goroutine
// to local work queue, #StoreLoad-style memory barrier, check sched.nmspinning.
// The general pattern for spinning->non-spinning transition is: decrement nmspinning,
// #StoreLoad-style memory barrier, check all per-P work queues for new work.
// Note that all this complexity does not apply to global run queue as we are not
// sloppy about thread unparking when submitting to global queue. Also see comments
// for nmspinning manipulation.

var (
	m0           m       // 注释：代表进程的主线程
	g0           g       // 注释：m0的g0，也就是m0.g0 = &g0
	mcache0      *mcache // 注释：（P0的内存缓存）引导程序mcache0。P的ID为0的将获得mcache0
	raceprocctx0 uintptr
)

//go:linkname runtime_inittask runtime..inittask
var runtime_inittask initTask

//go:linkname main_inittask main..inittask
var main_inittask initTask

// main_init_done is a signal used by cgocallbackg that initialization
// has been completed. It is made before _cgo_notify_runtime_init_done,
// so all cgo calls can rely on it existing. When main_init is complete,
// it is closed, meaning cgocallbackg can reliably receive from it.
var main_init_done chan bool

//go:linkname main_main main.main
func main_main()

// mainStarted indicates that the main M has started.
var mainStarted bool // 注释：mainStarted表示主M已启动。

// runtimeInitTime is the nanotime() at which the runtime started.
var runtimeInitTime int64

// Value to use for signal mask for newly created M's.
var initSigmask sigset

// The main goroutine.
func main() {
	g := getg()

	// Racectx of m0->g0 is used only as the parent of the main goroutine.
	// It must not be used for anything else.
	g.m.g0.racectx = 0

	// Max stack size is 1 GB on 64-bit, 250 MB on 32-bit.
	// Using decimal instead of binary GB and MB because
	// they look nicer in the stack overflow failure message.
	if sys.PtrSize == 8 {
		maxstacksize = 1000000000
	} else {
		maxstacksize = 250000000
	}

	// An upper limit for max stack size. Used to avoid random crashes
	// after calling SetMaxStack and trying to allocate a stack that is too big,
	// since stackalloc works with 32-bit sizes.
	maxstackceiling = 2 * maxstacksize

	// Allow newproc to start new Ms.
	mainStarted = true

	if GOARCH != "wasm" { // no threads on wasm yet, so no sysmon
		// For runtime_syscall_doAllThreadsSyscall, we
		// register sysmon is not ready for the world to be
		// stopped.
		atomic.Store(&sched.sysmonStarting, 1)
		systemstack(func() {
			newm(sysmon, nil, -1)
		})
	}

	// Lock the main goroutine onto this, the main OS thread,
	// during initialization. Most programs won't care, but a few
	// do require certain calls to be made by the main thread.
	// Those can arrange for main.main to run in the main thread
	// by calling runtime.LockOSThread during initialization
	// to preserve the lock.
	lockOSThread()

	if g.m != &m0 {
		throw("runtime.main not on m0")
	}
	m0.doesPark = true

	// Record when the world started.
	// Must be before doInit for tracing init.
	runtimeInitTime = nanotime()
	if runtimeInitTime == 0 {
		throw("nanotime returning zero")
	}

	if debug.inittrace != 0 {
		inittrace.id = getg().goid
		inittrace.active = true
	}

	doInit(&runtime_inittask) // Must be before defer.

	// Defer unlock so that runtime.Goexit during init does the unlock too.
	needUnlock := true
	defer func() {
		if needUnlock {
			unlockOSThread()
		}
	}()

	gcenable()

	main_init_done = make(chan bool)
	if iscgo {
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		if GOOS != "windows" {
			if _cgo_setenv == nil {
				throw("_cgo_setenv missing")
			}
			if _cgo_unsetenv == nil {
				throw("_cgo_unsetenv missing")
			}
		}
		if _cgo_notify_runtime_init_done == nil {
			throw("_cgo_notify_runtime_init_done missing")
		}
		// Start the template thread in case we enter Go from
		// a C-created thread and need to create a new thread.
		startTemplateThread()
		cgocall(_cgo_notify_runtime_init_done, nil)
	}

	doInit(&main_inittask)

	// Disable init tracing after main init done to avoid overhead
	// of collecting statistics in malloc and newproc
	inittrace.active = false

	close(main_init_done)

	needUnlock = false
	unlockOSThread()

	if isarchive || islibrary {
		// A program compiled with -buildmode=c-archive or c-shared
		// has a main, but it is not executed.
		return
	}
	fn := main_main // make an indirect call, as the linker doesn't know the address of the main package when laying down the runtime
	fn()
	if raceenabled {
		racefini()
	}

	// Make racy client program work: if panicking on
	// another goroutine at the same time as main returns,
	// let the other goroutine finish printing the panic trace.
	// Once it does, it will exit. See issues 3934 and 20018.
	if atomic.Load(&runningPanicDefers) != 0 {
		// Running deferred functions should not take long.
		for c := 0; c < 1000; c++ {
			if atomic.Load(&runningPanicDefers) == 0 {
				break
			}
			Gosched()
		}
	}
	if atomic.Load(&panicking) != 0 {
		gopark(nil, nil, waitReasonPanicWait, traceEvGoStop, 1)
	}

	exit(0)
	for {
		var x *int32
		*x = 0
	}
}

// os_beforeExit is called from os.Exit(0).
//go:linkname os_beforeExit os.runtime_beforeExit
func os_beforeExit() {
	if raceenabled {
		racefini()
	}
}

// start forcegc helper goroutine
func init() {
	go forcegchelper()
}

func forcegchelper() {
	forcegc.g = getg()
	lockInit(&forcegc.lock, lockRankForcegc)
	for {
		lock(&forcegc.lock)
		if forcegc.idle != 0 {
			throw("forcegc: phase error")
		}
		atomic.Store(&forcegc.idle, 1)
		goparkunlock(&forcegc.lock, waitReasonForceGCIdle, traceEvGoBlock, 1)
		// this goroutine is explicitly resumed by sysmon
		if debug.gctrace > 0 {
			println("GC forced")
		}
		// Time-triggered, fully concurrent.
		gcStart(gcTrigger{kind: gcTriggerTime, now: nanotime()})
	}
}

//go:nosplit

// Gosched yields the processor, allowing other goroutines to run. It does not
// suspend the current goroutine, so execution resumes automatically.
// 注释：让出CPU时间片，重新等待安排任务
func Gosched() {
	checkTimeouts()
	mcall(gosched_m)
}

// goschedguarded yields the processor like gosched, but also checks
// for forbidden states and opts out of the yield in those cases.
//go:nosplit
func goschedguarded() {
	mcall(goschedguarded_m)
}

// Puts the current goroutine into a waiting state and calls unlockf on the
// system stack.
// 注释：译：将当前goroutine置于等待状态，并在系统堆栈上调用unlock。
//
// If unlockf returns false, the goroutine is resumed.
// 注释：译：如果unlock返回false，则goroutine将恢复。
//
// unlockf must not access this G's stack, as it may be moved between
// the call to gopark and the call to unlockf.
// 注释：译：unlock不能访问这个G的堆栈，因为它可能在对gopark的调用和对unlock的调用之间移动。
//
// Note that because unlockf is called after putting the G into a waiting
// state, the G may have already been readied by the time unlockf is called
// unless there is external synchronization preventing the G from being
// readied. If unlockf returns false, it must guarantee that the G cannot be
// externally readied.
// 注释：译：注意，因为unlock f是在将G置于等待状态之后调用的，所以在调用unlock f时G可能已经准备好了，除非有外部同步阻止G准备好。如果unlock返回false，它必须保证G不能从外部准备好。
//
// Reason explains why the goroutine has been parked. It is displayed in stack
// traces and heap dumps. Reasons should be unique and descriptive. Do not
// re-use reasons, add new ones.
// 注释：译：原因解释了为什么goroutine停了下来。它显示在堆栈跟踪和堆转储中。原因应具有唯一性和描述性。不要重复使用原因，添加新的原因。
//
// 注释：(让渡控制权，当前协成G阻塞)把go代码停车；延迟执行。把要执行的方法放在mp.wait...前缀里，等待唤醒的时候执行
func gopark(unlockf func(*g, unsafe.Pointer) bool, lock unsafe.Pointer, reason waitReason, traceEv byte, traceskip int) {
	if reason != waitReasonSleep {
		checkTimeouts() // timeouts may expire while two goroutines keep the scheduler busy
	}
	mp := acquirem()          // 注释：获取当前G对应的M
	gp := mp.curg             // 注释：获取M里正在工作的G
	status := readgstatus(gp) // 注释：获取gp的G的状态
	// 注释：如果G的状态（不是运行中）和（不是GC扫描加运行中）时报错
	if status != _Grunning && status != _Gscanrunning {
		throw("gopark: bad g status")
	}
	mp.waitlock = lock           // 注释：设置等待锁
	mp.waitunlockf = unlockf     // 注释：设置解除等待锁的函数，系统协成执行完成后会调用该函数
	gp.waitreason = reason       // 注释：设置锁的原因
	mp.waittraceev = traceEv     // 注释：设置等待追踪事件类型
	mp.waittraceskip = traceskip // 注释：跳过几级事件追踪结果
	releasem(mp)                 // 注释：释放掉m
	// can't do anything that might move the G between Ms here.
	mcall(park_m) // 注释：保存现场，并且变更G的状态	casgstatus(gp, _Grunning, _Gwaiting)
}

// Puts the current goroutine into a waiting state and unlocks the lock.
// The goroutine can be made runnable again by calling goready(gp).
func goparkunlock(lock *mutex, reason waitReason, traceEv byte, traceskip int) {
	gopark(parkunlock_c, unsafe.Pointer(lock), reason, traceEv, traceskip)
}

// 注释：把gp放到下traceskip个位置上等待执行
// 注释：准备下一个要执行G，并且开启一个空闲M跑空闲P
func goready(gp *g, traceskip int) {
	// 注释：系统栈切换，把gp放到下traceskip个执行的栈位置上
	systemstack(func() { // 注释：切换系统栈调用(切换到G0上执行)，每个M下都有自己G0
		ready(gp, traceskip, true) // 注释：准备下一个要执行G，并且开启一个空闲M跑空闲P
	})
}

// 注释：获取空闲带阻塞G
// 注释：如果当前P中空闲G列表存在，并且全局空闲G有数据时。(去全局空闲G链表中拿出P中的空闲G总数的一半)
// 注释：步骤：
// 		1.线程加锁，并获取P指针
// 		2.如果P里空闲待阻塞栈里没有sudog时(栈是由数组实现的)
//			a.获取全局锁
//			b.填充本地队列容量的一半，从全局链表中拿取
//			c.如果本地队列还没有sudog，则实例化1个新的sudog放到本地队列中
// 		3.(出栈)从P空闲待阻塞栈里出栈
// 		4.释放线程锁
// 		5.返回空闲待阻塞的sudog
//go:nosplit
func acquireSudog() *sudog {
	// Delicate dance: the semaphore implementation calls
	// acquireSudog, acquireSudog calls new(sudog),
	// new calls malloc, malloc can call the garbage collector,
	// and the garbage collector calls the semaphore implementation
	// in stopTheWorld.
	// Break the cycle by doing acquirem/releasem around new(sudog).
	// The acquirem/releasem increments m.locks during new(sudog),
	// which keeps the garbage collector from being invoked.
	// 注释：译：精致的舞蹈：信号量实现调用acquireSudog，acquireSudog调用new（sudog），new调用malloc，malloc可以调用垃圾收集器，垃圾收集器调用stopTheWorld中的信号量实现。
	//		通过围绕新事物（sudog）进行获取/发布来打破循环。acquirem/releasem在new（sudog）过程中增加m.locks，从而防止垃圾收集器被调用。
	mp := acquirem() // 注释：获得当前的M（当前G对应的M）对象
	pp := mp.p.ptr() // 注释：当前M对应的P指针

	// 注释：
	// 注释：如果当前P没有空闲的G时
	if len(pp.sudogcache) == 0 {
		lock(&sched.sudoglock) // 注释：锁定全局G（sudog）链表
		// First, try to grab a batch from central cache.
		// 注释：如果当前P中空闲G列表存在，并且全局空闲G有数据时。(去全局空闲G链表中拿出P中的空闲G总数的一半)
		// 注释：循环插入本地P中空闲G列表数据，插入总数是空闲G列表的一半
		for len(pp.sudogcache) < cap(pp.sudogcache)/2 && sched.sudogcache != nil {
			s := sched.sudogcache                    // 注释：取出全局空闲G的头指针
			sched.sudogcache = s.next                // 注释：把全局空闲G链表的下一个头指针设置为全局空闲G的头指针
			s.next = nil                             // 注释：取出的空闲G断开链表（形成单独的空闲G）
			pp.sudogcache = append(pp.sudogcache, s) // 注释：把从全局空闲G链表取出的单个空闲G放到当前P的列表中
		}
		unlock(&sched.sudoglock) // 注释：释放全局空闲G链表的锁
		// If the central cache is empty, allocate a new one.
		// 注释：如果当前P中依然没有空闲G，则实例化新的空闲G指针放在P中(后面会取出一个)
		if len(pp.sudogcache) == 0 {
			pp.sudogcache = append(pp.sudogcache, new(sudog))
		}
	}
	n := len(pp.sudogcache)             // 注释：当前P中空闲G的数量
	s := pp.sudogcache[n-1]             // 注释：取出当前P中最后一个空闲G
	pp.sudogcache[n-1] = nil            // 注释：把当前P中空闲G列表的最后一个位置的内存释放
	pp.sudogcache = pp.sudogcache[:n-1] // 注释：重置P中空闲G列表的数据（去掉最后空闲G元素）
	if s.elem != nil {
		throw("acquireSudog: found s.elem != nil in cache")
	}
	releasem(mp) // 注释：释放当前M对象
	return s     // 注释：返回空闲的G
}

// 注释：释放空闲G，把空闲G放到本地空闲G切片里，如果本地空闲G切片已经满了则拿出一半放到全局空闲G单向链表里（放到链表的头部）
//go:nosplit
func releaseSudog(s *sudog) {
	if s.elem != nil {
		throw("runtime: sudog with non-nil elem")
	}
	if s.isSelect {
		throw("runtime: sudog with non-false isSelect")
	}
	if s.next != nil {
		throw("runtime: sudog with non-nil next")
	}
	if s.prev != nil {
		throw("runtime: sudog with non-nil prev")
	}
	if s.waitlink != nil {
		throw("runtime: sudog with non-nil waitlink")
	}
	if s.c != nil {
		throw("runtime: sudog with non-nil c")
	}
	gp := getg()
	if gp.param != nil {
		throw("runtime: releaseSudog with non-nil gp.param")
	}
	mp := acquirem() // 注释：获取当前对应的M(避免重新安排到另一个P) // avoid rescheduling to another P
	pp := mp.p.ptr() // 注释：获取当前对应P
	// 注释：如果空前G缓存区已经满了，将一半的本地缓存传输到中央缓存（全局空闲G链表）。
	if len(pp.sudogcache) == cap(pp.sudogcache) {
		// Transfer half of local cache to the central cache.
		// 注释： 将一半的本地缓存传输到中央缓存（全局空闲G链表）。
		var first, last *sudog // 注释：临时的链表结构，把超出一半本地缓存（P）的空闲G放到链表尾部，形成单向链表
		for len(pp.sudogcache) > cap(pp.sudogcache)/2 {
			n := len(pp.sudogcache)             // 注释：P中空闲G的数量
			p := pp.sudogcache[n-1]             // 注释：获取P中最后一个空闲G
			pp.sudogcache[n-1] = nil            // 注释：释放P中最后一个空闲G所在位置的内存
			pp.sudogcache = pp.sudogcache[:n-1] // 注释：缩短P，移除最后一个空闲G
			// 注释：如果临时链表的头部为空时，头和尾都等于空闲G
			if first == nil {
				first = p
			} else {
				last.next = p // 注释：如果临时链表已经存在，把空闲G放到链表尾部（形成单向链表）
			}
			last = p // 注释：重新赋值链表尾部
		}
		lock(&sched.sudoglock)       // 注释：中央缓存（全局空闲G单向链表）上锁
		last.next = sched.sudogcache // 注释：把中央缓存（全局空闲G单向链表）接到临时链表链的尾部
		sched.sudogcache = first     // 注释：重置中央缓存（全局空闲G单向链表）的头指针
		unlock(&sched.sudoglock)     // 注释：释放锁
	}
	pp.sudogcache = append(pp.sudogcache, s) // 注释：把空闲G放到本地缓存（P中空闲G切片）的尾部
	releasem(mp)                             // 注释：释放M
}

// funcPC returns the entry PC of the function f.
// It assumes that f is a func value. Otherwise the behavior is undefined.
// CAREFUL: In programs with plugins, funcPC can return different values
// for the same function (because there are actually multiple copies of
// the same function in the address space). To be safe, don't use the
// results of this function in any == expression. It is only safe to
// use the result as an address at which to start executing code.
// 注释：译：funcPC返回函数f的入口PC。它假设f是一个func值。否则行为是未定义的。
//		小心：在带有插件的程序中，funcPC可以为同一函数返回不同的值（因为在地址空间中实际上有同一函数的多个副本）。
//		为了安全起见，不要在任何==表达式中使用此函数的结果。只有将结果用作开始执行代码的地址才是安全的。
// 注释：获取方法的PC值
//go:nosplit
func funcPC(f interface{}) uintptr {
	return *(*uintptr)(efaceOf(&f).data) // 注释：获取方法的PC值
}

// called from assembly
func badmcall(fn func(*g)) {
	throw("runtime: mcall called on m->g0 stack")
}

func badmcall2(fn func(*g)) {
	throw("runtime: mcall function returned")
}

func badreflectcall() {
	panic(plainError("arg size to reflect.call more than 1GB"))
}

var badmorestackg0Msg = "fatal: morestack on g0\n"

//go:nosplit
//go:nowritebarrierrec
func badmorestackg0() {
	sp := stringStructOf(&badmorestackg0Msg)
	write(2, sp.str, int32(sp.len))
}

var badmorestackgsignalMsg = "fatal: morestack on gsignal\n"

//go:nosplit
//go:nowritebarrierrec
func badmorestackgsignal() {
	sp := stringStructOf(&badmorestackgsignalMsg)
	write(2, sp.str, int32(sp.len))
}

//go:nosplit
func badctxt() {
	throw("ctxt != 0")
}

func lockedOSThread() bool {
	gp := getg()
	return gp.lockedm != 0 && gp.m.lockedg != 0
}

var (
	// allgs contains all Gs ever created (including dead Gs), and thus
	// never shrinks.
	//
	// Access via the slice is protected by allglock or stop-the-world.
	// Readers that cannot take the lock may (carefully!) use the atomic
	// variables below.
	// 注释：译：allgs包含所有曾经创造过的g（包括死亡的g），因此永远不会收缩。
	//		通过切片访问受到allglock或stop the world的保护。无法获取锁的读者可以（小心！）使用下面的原子变量。
	allglock mutex // 注释：全局G的切片的锁
	allgs    []*g  // 注释：保存所有的g的切片

	// allglen and allgptr are atomic variables that contain len(allg) and
	// &allg[0] respectively. Proper ordering depends on totally-ordered
	// loads and stores. Writes are protected by allglock.
	//
	// allgptr is updated before allglen. Readers should read allglen
	// before allgptr to ensure that allglen is always <= len(allgptr). New
	// Gs appended during the race can be missed. For a consistent view of
	// all Gs, allglock must be held.
	//
	// allgptr copies should always be stored as a concrete type or
	// unsafe.Pointer, not uintptr, to ensure that GC can still reach it
	// even if it points to a stale array.
	// 注释：译：allglen和allgptr是分别包含len（allg）和&allg[0]的原子变量。正确的订购取决于完全订购的装载和存储。写入受allglock保护。
	//		allgptr在allglen之前更新。读者应在allgptr之前阅读allglen，以确保allglen始终<=len（allgptr）。比赛期间附加的新Gs可能会错过。
	//		为了对所有Gs有一个一致的看法，必须持有allglock。
	//		所有gptr副本应始终存储为具体类型或不安全类型。指针，而不是uintptr，以确保即使GC指向过时的数组，它仍然可以访问它。
	allglen uintptr // 注释：全局G切片个数
	allgptr **g     // 注释：全局G切片第一个元素的指针
)

// 注释：把G放到全局G切片里
func allgadd(gp *g) {
	if readgstatus(gp) == _Gidle { // 注释：读取状态并和_Gidle比较
		throw("allgadd: bad status Gidle")
	}

	lock(&allglock)           // 注释：全局G切片加锁
	allgs = append(allgs, gp) // 注释：把G放到全局G链表里
	if &allgs[0] != allgptr { // 注释：如果全局G切片的第一个元素地址不等于allgptr(全局G第一个元素的指针)时
		atomicstorep(unsafe.Pointer(&allgptr), unsafe.Pointer(&allgs[0])) // 注释：设置allgptr(全局G第一个元素的指针)为全局G切片第一个元素的指针
	}
	atomic.Storeuintptr(&allglen, uintptr(len(allgs))) // 注释：设置全局G切面的个数
	unlock(&allglock)                                  // 注释：全局G切片解锁
}

// atomicAllG returns &allgs[0] and len(allgs) for use with atomicAllGIndex.
func atomicAllG() (**g, uintptr) {
	length := atomic.Loaduintptr(&allglen)
	ptr := (**g)(atomic.Loadp(unsafe.Pointer(&allgptr)))
	return ptr, length
}

// atomicAllGIndex returns ptr[i] with the allgptr returned from atomicAllG.
func atomicAllGIndex(ptr **g, i uintptr) *g {
	return *(**g)(add(unsafe.Pointer(ptr), i*sys.PtrSize))
}

const (
	// Number of goroutine ids to grab from sched.goidgen to local per-P cache at once.
	// 16 seems to provide enough amortization, but other than that it's mostly arbitrary number.
	// 注释：译：一次从sched.goidgen抓取到本地per-P缓存的goroutine ID数。16似乎提供了足够的摊销，但除此之外，它大多是任意数字。
	_GoidCacheBatch = 16
)

// cpuinit extracts the environment variable GODEBUG from the environment on
// Unix-like operating systems and calls internal/cpu.Initialize.
func cpuinit() {
	const prefix = "GODEBUG="
	var env string

	switch GOOS {
	case "aix", "darwin", "ios", "dragonfly", "freebsd", "netbsd", "openbsd", "illumos", "solaris", "linux":
		cpu.DebugOptions = true

		// Similar to goenv_unix but extracts the environment value for
		// GODEBUG directly.
		// TODO(moehrmann): remove when general goenvs() can be called before cpuinit()
		n := int32(0)
		for argv_index(argv, argc+1+n) != nil {
			n++
		}

		for i := int32(0); i < n; i++ {
			p := argv_index(argv, argc+1+i)
			s := *(*string)(unsafe.Pointer(&stringStruct{unsafe.Pointer(p), findnull(p)}))

			if hasPrefix(s, prefix) {
				env = gostring(p)[len(prefix):]
				break
			}
		}
	}

	cpu.Initialize(env)

	// Support cpu feature variables are used in code generated by the compiler
	// to guard execution of instructions that can not be assumed to be always supported.
	x86HasPOPCNT = cpu.X86.HasPOPCNT
	x86HasSSE41 = cpu.X86.HasSSE41
	x86HasFMA = cpu.X86.HasFMA

	armHasVFPv4 = cpu.ARM.HasVFPv4

	arm64HasATOMICS = cpu.ARM64.HasATOMICS
}

// The bootstrap sequence is:
//
//	call osinit
//	call schedinit
//	make & queue new G
//	call runtime·mstart
//
// The new G calls runtime·main.
// 注释：调度系统的初始化
func schedinit() {
	lockInit(&sched.lock, lockRankSched)
	lockInit(&sched.sysmonlock, lockRankSysmon)
	lockInit(&sched.deferlock, lockRankDefer)
	lockInit(&sched.sudoglock, lockRankSudog)
	lockInit(&deadlock, lockRankDeadlock)
	lockInit(&paniclk, lockRankPanic)
	lockInit(&allglock, lockRankAllg)
	lockInit(&allpLock, lockRankAllp)
	lockInit(&reflectOffs.lock, lockRankReflectOffs)
	lockInit(&finlock, lockRankFin)
	lockInit(&trace.bufLock, lockRankTraceBuf)
	lockInit(&trace.stringsLock, lockRankTraceStrings)
	lockInit(&trace.lock, lockRankTrace)
	lockInit(&cpuprof.lock, lockRankCpuprof)
	lockInit(&trace.stackTab.lock, lockRankTraceStackTab)
	// Enforce that this lock is always a leaf lock.
	// All of this lock's critical sections should be
	// extremely short.
	lockInit(&memstats.heapStats.noPLock, lockRankLeafRank)

	// raceinit must be the first call to race detector.
	// In particular, it must be done before mallocinit below calls racemapshadow.
	_g_ := getg()
	if raceenabled {
		_g_.racectx, raceprocctx0 = raceinit()
	}

	sched.maxmcount = 10000

	// The world starts stopped.
	worldStopped()

	moduledataverify()
	stackinit()
	mallocinit()
	fastrandinit() // must run before mcommoninit
	mcommoninit(_g_.m, -1)
	cpuinit()       // must run before alginit
	alginit()       // maps must not be used before this call
	modulesinit()   // provides activeModules
	typelinksinit() // uses maps, activeModules
	itabsinit()     // uses activeModules

	sigsave(&_g_.m.sigmask)
	initSigmask = _g_.m.sigmask

	goargs()
	goenvs()
	parsedebugvars()
	gcinit()

	lock(&sched.lock)
	sched.lastpoll = uint64(nanotime())
	procs := ncpu // 注释：确认P的个数,默认等于cpu个数，可以通过GOMAXPROCS环境变量更改
	if n, ok := atoi32(gogetenv("GOMAXPROCS")); ok && n > 0 {
		procs = n
	}
	// 注释：调整P的个数，这里是新分配procs个P
	// 注释：函数procresize很重要，所有的P都是从这里分配的，以后也不用担心没有P了
	if procresize(procs) != nil {
		throw("unknown runnable goroutine during bootstrap")
	}
	unlock(&sched.lock)

	// World is effectively started now, as P's can run.
	worldStarted()

	// For cgocheck > 1, we turn on the write barrier at all times
	// and check all pointer writes. We can't do this until after
	// procresize because the write barrier needs a P.
	if debug.cgocheck > 1 {
		writeBarrier.cgo = true
		writeBarrier.enabled = true
		for _, p := range allp {
			p.wbBuf.reset()
		}
	}

	if buildVersion == "" {
		// Condition should never trigger. This code just serves
		// to ensure runtime·buildVersion is kept in the resulting binary.
		buildVersion = "unknown"
	}
	if len(modinfo) == 1 {
		// Condition should never trigger. This code just serves
		// to ensure runtime·modinfo is kept in the resulting binary.
		modinfo = ""
	}
}

func dumpgstatus(gp *g) {
	_g_ := getg()
	print("runtime: gp: gp=", gp, ", goid=", gp.goid, ", gp->atomicstatus=", readgstatus(gp), "\n")
	print("runtime:  g:  g=", _g_, ", goid=", _g_.goid, ",  g->atomicstatus=", readgstatus(_g_), "\n")
}

// sched.lock must be held.
func checkmcount() {
	assertLockHeld(&sched.lock)

	if mcount() > sched.maxmcount {
		print("runtime: program exceeds ", sched.maxmcount, "-thread limit\n")
		throw("thread exhaustion")
	}
}

// mReserveID returns the next ID to use for a new m. This new m is immediately
// considered 'running' by checkdead.
//
// sched.lock must be held.
// 注释：获取新建m的主键ID
func mReserveID() int64 {
	assertLockHeld(&sched.lock)

	// 注释：判断是否溢出
	if sched.mnext+1 < sched.mnext {
		throw("runtime: thread ID overflow")
	}
	id := sched.mnext
	sched.mnext++
	checkmcount()
	return id
}

// Pre-allocated ID may be passed as 'id', or omitted by passing -1.
func mcommoninit(mp *m, id int64) {
	_g_ := getg()

	// g0 stack won't make sense for user (and is not necessary unwindable).
	if _g_ != _g_.m.g0 {
		callers(1, mp.createstack[:])
	}

	lock(&sched.lock)

	if id >= 0 {
		mp.id = id
	} else {
		mp.id = mReserveID()
	}

	mp.fastrand[0] = uint32(int64Hash(uint64(mp.id), fastrandseed))       // 注释：计算第一个随机数(利用M的ID和随机种子做的哈希)
	mp.fastrand[1] = uint32(int64Hash(uint64(cputicks()), ^fastrandseed)) // 注释：计算第二个随机数(利用CPU时钟周期计数器和随机种子的按位取反做的哈希)
	if mp.fastrand[0]|mp.fastrand[1] == 0 {                               // 注释：两个值都为0时，把第二个值设置为1
		mp.fastrand[1] = 1 // 注释：两个都为0时，这个设置为1
	}

	mpreinit(mp)
	if mp.gsignal != nil {
		mp.gsignal.stackguard1 = mp.gsignal.stack.lo + _StackGuard
	}

	// Add to allm so garbage collector doesn't free g->m
	// when it is just in a register or thread-local storage.
	mp.alllink = allm

	// NumCgoCall() iterates over allm w/o schedlock,
	// so we need to publish it safely.
	atomicstorep(unsafe.Pointer(&allm), unsafe.Pointer(mp))
	unlock(&sched.lock)

	// Allocate memory to hold a cgo traceback if the cgo call crashes.
	if iscgo || GOOS == "solaris" || GOOS == "illumos" || GOOS == "windows" {
		mp.cgoCallers = new(cgoCallers)
	}
}

var fastrandseed uintptr

func fastrandinit() {
	s := (*[unsafe.Sizeof(fastrandseed)]byte)(unsafe.Pointer(&fastrandseed))[:]
	getRandomData(s)
}

// Mark gp ready to run.
// 注释：译：标记gp准备运行。
// 注释：准备下一个要执行G，并且开启一个空闲M跑空闲P
// 注释：把G从等待状态变更成准备执行状态（_Grunnable）
// 注释：(典型的自己不让抢，启动一个空闲P去抢别人的 哈)
// 注释：步骤：
//		1.当前M禁止抢占
//		2.把gp放到本地P队列，并标记下一个就执行；
//		3.拿个空闲M线程运行空闲P，并且自旋，开始抢别的G了
//		4.当前M解除禁止抢占
func ready(gp *g, traceskip int, next bool) {
	if trace.enabled {
		traceGoUnpark(gp, traceskip)
	}

	status := readgstatus(gp) // 注释：获取gp的状态

	// Mark runnable.
	// 注释：译：标记为可运行。
	_g_ := getg()                    // 注释：获取G
	mp := acquirem()                 // 注释：禁止抢占 // disable preemption because it can be holding p in a local var
	if status&^_Gscan != _Gwaiting { // 注释：(如果gp状态不是等待时报错)status清空_Gscan位后如果不等于_Gwaiting时报错
		dumpgstatus(gp)                 // 注释：打印日志
		throw("bad g->status in ready") // 注释：报错
	}

	// status is Gwaiting or Gscanwaiting, make Grunnable and put on runq
	// 注释：译：状态为Gwaiting或Gscanwaiting，使Grunable变为runq
	casgstatus(gp, _Gwaiting, _Grunnable) // 注释：如果gp状态是_Gwaiting时并更状态为_Grunnable
	runqput(_g_.m.p.ptr(), gp, next)      // 注释：把G放到本地P队列里，如果next是true则下一个就执行gp
	wakep()                               // 注释：拿个空闲M线程运行空闲P，并且自旋，开始抢别的G了
	releasem(mp)                          // 注释：释放禁止抢占(典型的自己不让抢，启动一个空闲P去抢别人的哈)
}

// freezeStopWait is a large value that freezetheworld sets
// sched.stopwait to in order to request that all Gs permanently stop.
// 注释：freezetheworld是一个很大的值，freezetheworld将sched.stopwait设置为，以请求永久停止所有G。
const freezeStopWait = 0x7fffffff

// freezing is set to non-zero if the runtime is trying to freeze the
// world.
var freezing uint32

// Similar to stopTheWorld but best-effort and can be called several times. // 注释：类似于stopTheWorld，但尽了最大努力，可以叫几次。
// There is no reverse operation, used during crashing. // 注释：没有在崩溃期间使用的反向操作。
// This function must not lock any mutexes. // 注释：此函数不能锁定任何互斥对象。
func freezetheworld() {
	atomic.Store(&freezing, 1)
	// stopwait and preemption requests can be lost
	// due to races with concurrently executing threads,
	// so try several times
	for i := 0; i < 5; i++ {
		// this should tell the scheduler to not start any new goroutines
		sched.stopwait = freezeStopWait
		atomic.Store(&sched.gcwaiting, 1)
		// this should stop running goroutines
		if !preemptall() {
			break // no running goroutines
		}
		usleep(1000)
	}
	// to be sure
	usleep(1000)
	preemptall()
	usleep(1000)
}

// All reads and writes of g's status go through readgstatus, casgstatus
// castogscanstatus, casfrom_Gscanstatus.
// 注释：原子获取G的当前状态
//go:nosplit
func readgstatus(gp *g) uint32 {
	return atomic.Load(&gp.atomicstatus)
}

// The Gscanstatuses are acting like locks and this releases them.
// If it proves to be a performance hit we should be able to make these
// simple atomic stores but for now we are going to throw if
// we see an inconsistent state.
func casfrom_Gscanstatus(gp *g, oldval, newval uint32) {
	success := false

	// Check that transition is valid.
	switch oldval {
	default:
		print("runtime: casfrom_Gscanstatus bad oldval gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus:top gp->status is not in scan state")
	case _Gscanrunnable,
		_Gscanwaiting,
		_Gscanrunning,
		_Gscansyscall,
		_Gscanpreempted:
		if newval == oldval&^_Gscan {
			success = atomic.Cas(&gp.atomicstatus, oldval, newval)
		}
	}
	if !success {
		print("runtime: casfrom_Gscanstatus failed gp=", gp, ", oldval=", hex(oldval), ", newval=", hex(newval), "\n")
		dumpgstatus(gp)
		throw("casfrom_Gscanstatus: gp->status is not in scan state")
	}
	releaseLockRank(lockRankGscan)
}

// This will return false if the gp is not in the expected status and the cas fails.
// This acts like a lock acquire while the casfromgstatus acts like a lock release.
func castogscanstatus(gp *g, oldval, newval uint32) bool {
	switch oldval {
	case _Grunnable,
		_Grunning,
		_Gwaiting,
		_Gsyscall:
		if newval == oldval|_Gscan {
			r := atomic.Cas(&gp.atomicstatus, oldval, newval)
			if r {
				acquireLockRank(lockRankGscan)
			}
			return r

		}
	}
	print("runtime: castogscanstatus oldval=", hex(oldval), " newval=", hex(newval), "\n")
	throw("castogscanstatus")
	panic("not reached")
}

// If asked to move to or from a Gscanstatus this will throw. Use the castogscanstatus
// and casfrom_Gscanstatus instead.
// casgstatus will loop if the g->atomicstatus is in a Gscan status until the routine that
// put it in the Gscan state is finished.
// 注释：if gp.atomicstatus == oldval { gp = newval}
// 注释：(原子操作)修改G的状态，把状态为oldval的更改为newval
//go:nosplit
func casgstatus(gp *g, oldval, newval uint32) {
	if (oldval&_Gscan != 0) || (newval&_Gscan != 0) || oldval == newval {
		systemstack(func() {
			print("runtime: casgstatus: oldval=", hex(oldval), " newval=", hex(newval), "\n")
			throw("casgstatus: bad incoming values")
		})
	}

	acquireLockRank(lockRankGscan)
	releaseLockRank(lockRankGscan)

	// See https://golang.org/cl/21503 for justification of the yield delay.
	const yieldDelay = 5 * 1000
	var nextYield int64

	// loop if gp->atomicstatus is in a scan state giving
	// GC time to finish and change the state to oldval.
	for i := 0; !atomic.Cas(&gp.atomicstatus, oldval, newval); i++ { // 注释：(原子操作)修改G的状态，把状态oldval修改成newval
		// 注释：修改状态失败，补救逻辑
		// 注释：大概的意思就是，等待一小会在尝试修改
		if oldval == _Gwaiting && gp.atomicstatus == _Grunnable {
			throw("casgstatus: waiting for Gwaiting but is Grunnable")
		}
		if i == 0 {
			nextYield = nanotime() + yieldDelay
		}
		if nanotime() < nextYield {
			for x := 0; x < 10 && gp.atomicstatus != oldval; x++ {
				procyield(1) // 注释：cup暂停一次
			}
		} else {
			osyield()
			nextYield = nanotime() + yieldDelay/2
		}
	}
}

// casgstatus(gp, oldstatus, Gcopystack), assuming oldstatus is Gwaiting or Grunnable.
// Returns old status. Cannot call casgstatus directly, because we are racing with an
// async wakeup that might come in from netpoll. If we see Gwaiting from the readgstatus,
// it might have become Grunnable by the time we get to the cas. If we called casgstatus,
// it would loop waiting for the status to go back to Gwaiting, which it never will.
//go:nosplit
func casgcopystack(gp *g) uint32 {
	for {
		oldstatus := readgstatus(gp) &^ _Gscan
		if oldstatus != _Gwaiting && oldstatus != _Grunnable {
			throw("copystack: bad status, not Gwaiting or Grunnable")
		}
		if atomic.Cas(&gp.atomicstatus, oldstatus, _Gcopystack) {
			return oldstatus
		}
	}
}

// casGToPreemptScan transitions gp from _Grunning to _Gscan|_Gpreempted.
//
// TODO(austin): This is the only status operation that both changes
// the status and locks the _Gscan bit. Rethink this.
func casGToPreemptScan(gp *g, old, new uint32) {
	if old != _Grunning || new != _Gscan|_Gpreempted {
		throw("bad g transition")
	}
	acquireLockRank(lockRankGscan)
	for !atomic.Cas(&gp.atomicstatus, _Grunning, _Gscan|_Gpreempted) {
	}
}

// casGFromPreempted attempts to transition gp from _Gpreempted to
// _Gwaiting. If successful, the caller is responsible for
// re-scheduling gp.
func casGFromPreempted(gp *g, old, new uint32) bool {
	if old != _Gpreempted || new != _Gwaiting {
		throw("bad g transition")
	}
	return atomic.Cas(&gp.atomicstatus, _Gpreempted, _Gwaiting)
}

// stopTheWorld stops all P's from executing goroutines, interrupting
// all goroutines at GC safe points and records reason as the reason
// for the stop. On return, only the current goroutine's P is running.
// stopTheWorld must not be called from a system stack and the caller
// must not hold worldsema. The caller must call startTheWorld when
// other P's should resume execution.
//
// stopTheWorld is safe for multiple goroutines to call at the
// same time. Each will execute its own stop, and the stops will
// be serialized.
//
// This is also used by routines that do stack dumps. If the system is
// in panic or being exited, this may not reliably stop all
// goroutines.
func stopTheWorld(reason string) {
	semacquire(&worldsema)
	gp := getg()
	gp.m.preemptoff = reason
	systemstack(func() {
		// Mark the goroutine which called stopTheWorld preemptible so its
		// stack may be scanned.
		// This lets a mark worker scan us while we try to stop the world
		// since otherwise we could get in a mutual preemption deadlock.
		// We must not modify anything on the G stack because a stack shrink
		// may occur. A stack shrink is otherwise OK though because in order
		// to return from this function (and to leave the system stack) we
		// must have preempted all goroutines, including any attempting
		// to scan our stack, in which case, any stack shrinking will
		// have already completed by the time we exit.
		casgstatus(gp, _Grunning, _Gwaiting)
		stopTheWorldWithSema()
		casgstatus(gp, _Gwaiting, _Grunning)
	})
}

// startTheWorld undoes the effects of stopTheWorld.
func startTheWorld() {
	systemstack(func() { startTheWorldWithSema(false) })

	// worldsema must be held over startTheWorldWithSema to ensure
	// gomaxprocs cannot change while worldsema is held.
	//
	// Release worldsema with direct handoff to the next waiter, but
	// acquirem so that semrelease1 doesn't try to yield our time.
	//
	// Otherwise if e.g. ReadMemStats is being called in a loop,
	// it might stomp on other attempts to stop the world, such as
	// for starting or ending GC. The operation this blocks is
	// so heavy-weight that we should just try to be as fair as
	// possible here.
	//
	// We don't want to just allow us to get preempted between now
	// and releasing the semaphore because then we keep everyone
	// (including, for example, GCs) waiting longer.
	mp := acquirem()
	mp.preemptoff = ""
	semrelease1(&worldsema, true, 0)
	releasem(mp)
}

// stopTheWorldGC has the same effect as stopTheWorld, but blocks
// until the GC is not running. It also blocks a GC from starting
// until startTheWorldGC is called.
func stopTheWorldGC(reason string) {
	semacquire(&gcsema)
	stopTheWorld(reason)
}

// startTheWorldGC undoes the effects of stopTheWorldGC.
func startTheWorldGC() {
	startTheWorld()
	semrelease(&gcsema)
}

// Holding worldsema grants an M the right to try to stop the world.
var worldsema uint32 = 1

// Holding gcsema grants the M the right to block a GC, and blocks
// until the current GC is done. In particular, it prevents gomaxprocs
// from changing concurrently.
//
// TODO(mknyszek): Once gomaxprocs and the execution tracer can handle
// being changed/enabled during a GC, remove this.
var gcsema uint32 = 1

// stopTheWorldWithSema is the core implementation of stopTheWorld.
// The caller is responsible for acquiring worldsema and disabling
// preemption first and then should stopTheWorldWithSema on the system
// stack:
//
//	semacquire(&worldsema, 0)
//	m.preemptoff = "reason"
//	systemstack(stopTheWorldWithSema)
//
// When finished, the caller must either call startTheWorld or undo
// these three operations separately:
//
//	m.preemptoff = ""
//	systemstack(startTheWorldWithSema)
//	semrelease(&worldsema)
//
// It is allowed to acquire worldsema once and then execute multiple
// startTheWorldWithSema/stopTheWorldWithSema pairs.
// Other P's are able to execute between successive calls to
// startTheWorldWithSema and stopTheWorldWithSema.
// Holding worldsema causes any other goroutines invoking
// stopTheWorld to block.
func stopTheWorldWithSema() {
	_g_ := getg()

	// If we hold a lock, then we won't be able to stop another M
	// that is blocked trying to acquire the lock.
	if _g_.m.locks > 0 {
		throw("stopTheWorld: holding locks")
	}

	lock(&sched.lock)
	sched.stopwait = gomaxprocs
	atomic.Store(&sched.gcwaiting, 1)
	preemptall()
	// stop current P
	_g_.m.p.ptr().status = _Pgcstop // Pgcstop is only diagnostic.
	sched.stopwait--
	// try to retake all P's in Psyscall status
	for _, p := range allp {
		s := p.status
		if s == _Psyscall && atomic.Cas(&p.status, s, _Pgcstop) {
			if trace.enabled {
				traceGoSysBlock(p)
				traceProcStop(p)
			}
			p.syscalltick++
			sched.stopwait--
		}
	}
	// stop idle P's
	for {
		p := pidleget()
		if p == nil {
			break
		}
		p.status = _Pgcstop
		sched.stopwait--
	}
	wait := sched.stopwait > 0
	unlock(&sched.lock)

	// wait for remaining P's to stop voluntarily
	if wait {
		for {
			// wait for 100us, then try to re-preempt in case of any races
			if notetsleep(&sched.stopnote, 100*1000) {
				noteclear(&sched.stopnote)
				break
			}
			preemptall()
		}
	}

	// sanity checks
	bad := ""
	if sched.stopwait != 0 {
		bad = "stopTheWorld: not stopped (stopwait != 0)"
	} else {
		for _, p := range allp {
			if p.status != _Pgcstop {
				bad = "stopTheWorld: not stopped (status != _Pgcstop)"
			}
		}
	}
	if atomic.Load(&freezing) != 0 {
		// Some other thread is panicking. This can cause the
		// sanity checks above to fail if the panic happens in
		// the signal handler on a stopped thread. Either way,
		// we should halt this thread.
		lock(&deadlock)
		lock(&deadlock)
	}
	if bad != "" {
		throw(bad)
	}

	worldStopped()
}

func startTheWorldWithSema(emitTraceEvent bool) int64 {
	assertWorldStopped()

	mp := acquirem() // disable preemption because it can be holding p in a local var
	if netpollinited() {
		list := netpoll(0) // non-blocking
		injectglist(&list)
	}
	lock(&sched.lock)

	procs := gomaxprocs
	if newprocs != 0 {
		procs = newprocs
		newprocs = 0
	}
	p1 := procresize(procs)
	sched.gcwaiting = 0
	if sched.sysmonwait != 0 {
		sched.sysmonwait = 0
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)

	worldStarted()

	for p1 != nil {
		p := p1
		p1 = p1.link.ptr()
		if p.m != 0 {
			mp := p.m.ptr()
			p.m = 0
			if mp.nextp != 0 {
				throw("startTheWorld: inconsistent mp->nextp")
			}
			mp.nextp.set(p)
			notewakeup(&mp.park)
		} else {
			// Start M to run P.  Do not start another M below.
			newm(nil, p, -1)
		}
	}

	// Capture start-the-world time before doing clean-up tasks.
	startTime := nanotime()
	if emitTraceEvent {
		traceGCSTWDone()
	}

	// Wakeup an additional proc in case we have excessive runnable goroutines
	// in local queues or in the global queue. If we don't, the proc will park itself.
	// If we have lots of excessive work, resetspinning will unpark additional procs as necessary.
	wakep()

	releasem(mp)

	return startTime
}

// usesLibcall indicates whether this runtime performs system calls
// via libcall.
func usesLibcall() bool {
	switch GOOS {
	case "aix", "darwin", "illumos", "ios", "solaris", "windows":
		return true
	case "openbsd":
		return GOARCH == "amd64" || GOARCH == "arm64"
	}
	return false
}

// mStackIsSystemAllocated indicates whether this runtime starts on a
// system-allocated stack.
func mStackIsSystemAllocated() bool {
	switch GOOS {
	case "aix", "darwin", "plan9", "illumos", "ios", "solaris", "windows":
		return true
	case "openbsd":
		switch GOARCH {
		case "amd64", "arm64":
			return true
		}
	}
	return false
}

// mstart is the entry-point for new Ms.
//
// This must not split the stack because we may not even have stack
// bounds set up yet.
//
// May run during STW (because it doesn't have a P yet), so write
// barriers are not allowed.
//
// 注释：启动线程，并且启动调度系统
//go:nosplit
//go:nowritebarrierrec
func mstart() {
	_g_ := getg() // 注释：这里获取的g是g0，在系统堆栈(是从TLS里获取的G指针)

	osStack := _g_.stack.lo == 0
	// 注释：判断栈低地址是否有值
	if osStack {
		// Initialize stack bounds from system stack.
		// Cgo may have left stack size in stack.hi.
		// minit may update the stack bounds.
		//
		// Note: these bounds may not be very accurate.
		// We set hi to &size, but there are things above
		// it. The 1024 is supposed to compensate this,
		// but is somewhat arbitrary.
		size := _g_.stack.hi
		if size == 0 {
			size = 8192 * sys.StackGuardMultiplier
		}
		_g_.stack.hi = uintptr(noescape(unsafe.Pointer(&size)))
		_g_.stack.lo = _g_.stack.hi - size + 1024
	}
	// Initialize stack guard so that we can start calling regular
	// Go code.
	_g_.stackguard0 = _g_.stack.lo + _StackGuard
	// This is the g0, so we can also call go:systemstack
	// functions, which check stackguard1.
	_g_.stackguard1 = _g_.stackguard0
	mstart1()

	// Exit this thread.
	if mStackIsSystemAllocated() {
		// Windows, Solaris, illumos, Darwin, AIX and Plan 9 always system-allocate
		// the stack, but put it in _g_.stack before mstart,
		// so the logic above hasn't set osStack yet.
		osStack = true
	}
	mexit(osStack)
}

func mstart1() {
	_g_ := getg() // 注释：获取当前g,目前当前g是g0

	// 注释：确保g是系统栈上的g0，调度器只在g0上执行
	if _g_ != _g_.m.g0 {
		throw("bad runtime·mstart")
	}

	// Record the caller for use as the top of stack in mcall and
	// for terminating the thread.
	// We're never coming back to mstart1 after we call schedule,
	// so other calls can reuse the current frame.
	// 注释getcallerpc()获取mstart1执行完的返回地址(返回的是mstart调用mstart1时被call指令压栈的返回地址)
	// 注释：getcallersp()获取调用mstart1时的栈顶地址(函数返回的是调用mstart1函数之前mstart函数的栈顶地址)
	save(getcallerpc(), getcallersp()) // 注释：把g0保存现场：保存PC和SP到G结构体上
	asminit()                          // 注释：汇编初始化，AMD64没有代码执行(位置：TEXT runtime·asminit(SB),NOSPLIT,$0-0)
	minit()                            // 注释：初始化m，主要是设置线程的备用信号堆栈和信号掩码

	// Install signal handlers; after minit so that minit can
	// prepare the thread to be able to handle the signals.
	if _g_.m == &m0 { // 注释：判断是否是第一个m（m0）,如果是则走特殊流程,m0也是正常的m，只是创建流程不一样
		mstartm0() // 注释：第一次M0的时候执行
	}

	// 注释：判断是否有起始任务函数；如果有m的起始任务函数，则执行，比如sysmon函数。对于m0来说，是没有mstartfn的
	if fn := _g_.m.mstartfn; fn != nil {
		fn() // 注释：执行m里的函数
	}

	// 注释：如果不是m0，需要绑定p
	if _g_.m != &m0 {
		acquirep(_g_.m.nextp.ptr()) // 注释：把m里的nextp的p和m相互绑定(nextp是其他m给付的值，当m启动的时候会第一时间执行nextp)
		_g_.m.nextp = 0             // 注释：清空m里的nextp
	}
	schedule() // 注释：mstart1：进入执行调度循环程序
}

// mstartm0 implements part of mstart1 that only runs on the m0.
//
// Write barriers are allowed here because we know the GC can't be
// running yet, so they'll be no-ops.
//
// 注释：第一次M0的时候执行
//go:yeswritebarrierrec
func mstartm0() {
	// Create an extra M for callbacks on threads not created by Go.
	// An extra M is also needed on Windows for callbacks created by
	// syscall.NewCallback. See issue #6751 for details.
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		cgoHasExtraM = true
		newextram()
	}
	initsig(false) // 注释：初始化信号
}

// mPark causes a thread to park itself - temporarily waking for
// fixups but otherwise waiting to be fully woken. This is the
// only way that m's should park themselves.
//go:nosplit
func mPark() {
	g := getg()
	for {
		notesleep(&g.m.park)
		// Note, because of signal handling by this parked m,
		// a preemptive mDoFixup() may actually occur via
		// mDoFixupAndOSYield(). (See golang.org/issue/44193)
		noteclear(&g.m.park)
		if !mDoFixup() {
			return
		}
	}
}

// mexit tears down and exits the current thread.
//
// Don't call this directly to exit the thread, since it must run at
// the top of the thread stack. Instead, use gogo(&_g_.m.g0.sched) to
// unwind the stack to the point that exits the thread.
//
// It is entered with m.p != nil, so write barriers are allowed. It
// will release the P before exiting.
//
//go:yeswritebarrierrec
func mexit(osStack bool) {
	g := getg()
	m := g.m

	if m == &m0 {
		// This is the main thread. Just wedge it.
		//
		// On Linux, exiting the main thread puts the process
		// into a non-waitable zombie state. On Plan 9,
		// exiting the main thread unblocks wait even though
		// other threads are still running. On Solaris we can
		// neither exitThread nor return from mstart. Other
		// bad things probably happen on other platforms.
		//
		// We could try to clean up this M more before wedging
		// it, but that complicates signal handling.
		handoffp(releasep())
		lock(&sched.lock)
		sched.nmfreed++
		checkdead()
		unlock(&sched.lock)
		mPark()
		throw("locked m0 woke up")
	}

	sigblock(true)
	unminit()

	// Free the gsignal stack.
	if m.gsignal != nil {
		stackfree(m.gsignal.stack)
		// On some platforms, when calling into VDSO (e.g. nanotime)
		// we store our g on the gsignal stack, if there is one.
		// Now the stack is freed, unlink it from the m, so we
		// won't write to it when calling VDSO code.
		m.gsignal = nil
	}

	// Remove m from allm.
	lock(&sched.lock)
	for pprev := &allm; *pprev != nil; pprev = &(*pprev).alllink {
		if *pprev == m {
			*pprev = m.alllink
			goto found
		}
	}
	throw("m not found in allm")
found:
	if !osStack {
		// Delay reaping m until it's done with the stack.
		//
		// If this is using an OS stack, the OS will free it
		// so there's no need for reaping.
		atomic.Store(&m.freeWait, 1)
		// Put m on the free list, though it will not be reaped until
		// freeWait is 0. Note that the free list must not be linked
		// through alllink because some functions walk allm without
		// locking, so may be using alllink.
		m.freelink = sched.freem
		sched.freem = m
	}
	unlock(&sched.lock)

	// Release the P.
	handoffp(releasep())
	// After this point we must not have write barriers.

	// Invoke the deadlock detector. This must happen after
	// handoffp because it may have started a new M to take our
	// P's work.
	lock(&sched.lock)
	sched.nmfreed++
	checkdead()
	unlock(&sched.lock)

	if GOOS == "darwin" || GOOS == "ios" {
		// Make sure pendingPreemptSignals is correct when an M exits.
		// For #41702.
		if atomic.Load(&m.signalPending) != 0 {
			atomic.Xadd(&pendingPreemptSignals, -1)
		}
	}

	// Destroy all allocated resources. After this is called, we may no
	// longer take any locks.
	mdestroy(m)

	if osStack {
		// Return from mstart and let the system thread
		// library free the g0 stack and terminate the thread.
		return
	}

	// mstart is the thread's entry point, so there's nothing to
	// return to. Exit the thread directly. exitThread will clear
	// m.freeWait when it's done with the stack and the m can be
	// reaped.
	exitThread(&m.freeWait)
}

// forEachP calls fn(p) for every P p when p reaches a GC safe point.
// If a P is currently executing code, this will bring the P to a GC
// safe point and execute fn on that P. If the P is not executing code
// (it is idle or in a syscall), this will call fn(p) directly while
// preventing the P from exiting its state. This does not ensure that
// fn will run on every CPU executing Go code, but it acts as a global
// memory barrier. GC uses this as a "ragged barrier."
//
// The caller must hold worldsema.
//
//go:systemstack
func forEachP(fn func(*p)) {
	mp := acquirem()
	_p_ := getg().m.p.ptr()

	lock(&sched.lock)
	if sched.safePointWait != 0 {
		throw("forEachP: sched.safePointWait != 0")
	}
	sched.safePointWait = gomaxprocs - 1
	sched.safePointFn = fn

	// Ask all Ps to run the safe point function.
	for _, p := range allp {
		if p != _p_ {
			atomic.Store(&p.runSafePointFn, 1)
		}
	}
	preemptall()

	// Any P entering _Pidle or _Psyscall from now on will observe
	// p.runSafePointFn == 1 and will call runSafePointFn when
	// changing its status to _Pidle/_Psyscall.

	// Run safe point function for all idle Ps. sched.pidle will
	// not change because we hold sched.lock.
	for p := sched.pidle.ptr(); p != nil; p = p.link.ptr() {
		if atomic.Cas(&p.runSafePointFn, 1, 0) {
			fn(p)
			sched.safePointWait--
		}
	}

	wait := sched.safePointWait > 0
	unlock(&sched.lock)

	// Run fn for the current P.
	fn(_p_)

	// Force Ps currently in _Psyscall into _Pidle and hand them
	// off to induce safe point function execution.
	for _, p := range allp {
		s := p.status
		if s == _Psyscall && p.runSafePointFn == 1 && atomic.Cas(&p.status, s, _Pidle) {
			if trace.enabled {
				traceGoSysBlock(p)
				traceProcStop(p)
			}
			p.syscalltick++
			handoffp(p)
		}
	}

	// Wait for remaining Ps to run fn.
	if wait {
		for {
			// Wait for 100us, then try to re-preempt in
			// case of any races.
			//
			// Requires system stack.
			if notetsleep(&sched.safePointNote, 100*1000) {
				noteclear(&sched.safePointNote)
				break
			}
			preemptall()
		}
	}
	if sched.safePointWait != 0 {
		throw("forEachP: not done")
	}
	for _, p := range allp {
		if p.runSafePointFn != 0 {
			throw("forEachP: P did not run fn")
		}
	}

	lock(&sched.lock)
	sched.safePointFn = nil
	unlock(&sched.lock)
	releasem(mp)
}

// syscall_runtime_doAllThreadsSyscall serializes Go execution and
// executes a specified fn() call on all m's.
//
// The boolean argument to fn() indicates whether the function's
// return value will be consulted or not. That is, fn(true) should
// return true if fn() succeeds, and fn(true) should return false if
// it failed. When fn(false) is called, its return status will be
// ignored.
//
// syscall_runtime_doAllThreadsSyscall first invokes fn(true) on a
// single, coordinating, m, and only if it returns true does it go on
// to invoke fn(false) on all of the other m's known to the process.
//
//go:linkname syscall_runtime_doAllThreadsSyscall syscall.runtime_doAllThreadsSyscall
func syscall_runtime_doAllThreadsSyscall(fn func(bool) bool) {
	if iscgo {
		panic("doAllThreadsSyscall not supported with cgo enabled")
	}
	if fn == nil {
		return
	}
	for atomic.Load(&sched.sysmonStarting) != 0 {
		osyield()
	}

	// We don't want this thread to handle signals for the
	// duration of this critical section. The underlying issue
	// being that this locked coordinating m is the one monitoring
	// for fn() execution by all the other m's of the runtime,
	// while no regular go code execution is permitted (the world
	// is stopped). If this present m were to get distracted to
	// run signal handling code, and find itself waiting for a
	// second thread to execute go code before being able to
	// return from that signal handling, a deadlock will result.
	// (See golang.org/issue/44193.)
	lockOSThread()
	var sigmask sigset
	sigsave(&sigmask)
	sigblock(false)

	stopTheWorldGC("doAllThreadsSyscall")
	if atomic.Load(&newmHandoff.haveTemplateThread) != 0 {
		// Ensure that there are no in-flight thread
		// creations: don't want to race with allm.
		lock(&newmHandoff.lock)
		for !newmHandoff.waiting {
			unlock(&newmHandoff.lock)
			osyield()
			lock(&newmHandoff.lock)
		}
		unlock(&newmHandoff.lock)
	}
	if netpollinited() {
		netpollBreak()
	}
	sigRecvPrepareForFixup()
	_g_ := getg()
	if raceenabled {
		// For m's running without racectx, we loan out the
		// racectx of this call.
		lock(&mFixupRace.lock)
		mFixupRace.ctx = _g_.racectx
		unlock(&mFixupRace.lock)
	}
	if ok := fn(true); ok {
		tid := _g_.m.procid
		for mp := allm; mp != nil; mp = mp.alllink {
			if mp.procid == tid {
				// This m has already completed fn()
				// call.
				continue
			}
			// Be wary of mp's without procid values if
			// they are known not to park. If they are
			// marked as parking with a zero procid, then
			// they will be racing with this code to be
			// allocated a procid and we will annotate
			// them with the need to execute the fn when
			// they acquire a procid to run it.
			if mp.procid == 0 && !mp.doesPark {
				// Reaching here, we are either
				// running Windows, or cgo linked
				// code. Neither of which are
				// currently supported by this API.
				throw("unsupported runtime environment")
			}
			// stopTheWorldGC() doesn't guarantee stopping
			// all the threads, so we lock here to avoid
			// the possibility of racing with mp.
			lock(&mp.mFixup.lock)
			mp.mFixup.fn = fn
			atomic.Store(&mp.mFixup.used, 1)
			if mp.doesPark {
				// For non-service threads this will
				// cause the wakeup to be short lived
				// (once the mutex is unlocked). The
				// next real wakeup will occur after
				// startTheWorldGC() is called.
				notewakeup(&mp.park)
			}
			unlock(&mp.mFixup.lock)
		}
		for {
			done := true
			for mp := allm; done && mp != nil; mp = mp.alllink {
				if mp.procid == tid {
					continue
				}
				done = atomic.Load(&mp.mFixup.used) == 0
			}
			if done {
				break
			}
			// if needed force sysmon and/or newmHandoff to wakeup.
			lock(&sched.lock)
			if atomic.Load(&sched.sysmonwait) != 0 {
				atomic.Store(&sched.sysmonwait, 0)
				notewakeup(&sched.sysmonnote)
			}
			unlock(&sched.lock)
			lock(&newmHandoff.lock)
			if newmHandoff.waiting {
				newmHandoff.waiting = false
				notewakeup(&newmHandoff.wake)
			}
			unlock(&newmHandoff.lock)
			osyield()
		}
	}
	if raceenabled {
		lock(&mFixupRace.lock)
		mFixupRace.ctx = 0
		unlock(&mFixupRace.lock)
	}
	startTheWorldGC()
	msigrestore(sigmask)
	unlockOSThread()
}

// runSafePointFn runs the safe point function, if any, for this P.
// This should be called like
//
//     if getg().m.p.runSafePointFn != 0 {
//         runSafePointFn()
//     }
//
// runSafePointFn must be checked on any transition in to _Pidle or
// _Psyscall to avoid a race where forEachP sees that the P is running
// just before the P goes into _Pidle/_Psyscall and neither forEachP
// nor the P run the safe-point function.
// 注释：必须在向_Pidle或_Psyscall的任何转换中检查runSafePointFn，以避免发生竞争，即forEachP在P进入_Pidle/Psyscall之前看到P正在运行，并且forEachP和P都没有运行安全点函数。
// 注释：安全节点检查，避免数据竞争
func runSafePointFn() {
	p := getg().m.p.ptr()
	// Resolve the race between forEachP running the safe-point
	// function on this P's behalf and this P running the
	// safe-point function directly.
	// 注释：把p.runSafePointFn从1设置为0
	if !atomic.Cas(&p.runSafePointFn, 1, 0) { // 注释：是否有安全节点函数0否1是，如果是1则设置为0并且返回true，并把布尔值取反
		return
	}
	sched.safePointFn(p)  // 注释：执行安全节点函数，把当前P放进去，检查是否有数据冲突（检测数据竞争）
	lock(&sched.lock)     // 注释：加锁
	sched.safePointWait-- // 注释：安全节点数量递减
	if sched.safePointWait == 0 {
		notewakeup(&sched.safePointNote) // 注释：如果安全节点safePointWait为0时唤醒安全节点的M
	}
	unlock(&sched.lock) // 注释：解锁
}

// When running with cgo, we call _cgo_thread_start
// to start threads for us so that we can play nicely with
// foreign code.
var cgoThreadStart unsafe.Pointer

type cgothreadstart struct {
	g   guintptr
	tls *uint64
	fn  unsafe.Pointer
}

// Allocate a new m unassociated with any thread.
// Can use p for allocation context if needed.
// fn is recorded as the new m's m.mstartfn.
// id is optional pre-allocated m ID. Omit by passing -1.
//
// This function is allowed to have write barriers even if the caller
// isn't because it borrows _p_.
//
//go:yeswritebarrierrec
func allocm(_p_ *p, fn func(), id int64) *m {
	_g_ := getg()     // 注释：获取当前的G
	acquirem()        // disable GC because it can be called from sysmon // 注释：获取g对应的m（禁用GC）
	if _g_.m.p == 0 { // 注释：如果m没有绑定p
		acquirep(_p_) // temporarily borrow p for mallocs in this function // 注释：获取p
	}

	// Release the free M list. We need to do this somewhere and
	// this may free up a stack we can use.
	if sched.freem != nil {
		lock(&sched.lock)
		var newList *m
		for freem := sched.freem; freem != nil; {
			if freem.freeWait != 0 {
				next := freem.freelink
				freem.freelink = newList
				newList = freem
				freem = next
				continue
			}
			// stackfree must be on the system stack, but allocm is
			// reachable off the system stack transitively from
			// startm.
			systemstack(func() {
				stackfree(freem.g0.stack)
			})
			freem = freem.freelink
		}
		sched.freem = newList
		unlock(&sched.lock)
	}

	mp := new(m)
	mp.mstartfn = fn
	mcommoninit(mp, id)

	// In case of cgo or Solaris or illumos or Darwin, pthread_create will make us a stack.
	// Windows and Plan 9 will layout sched stack on OS stack.
	if iscgo || mStackIsSystemAllocated() {
		mp.g0 = malg(-1)
	} else {
		mp.g0 = malg(8192 * sys.StackGuardMultiplier)
	}
	mp.g0.m = mp

	if _p_ == _g_.m.p.ptr() {
		releasep()
	}
	releasem(_g_.m)

	return mp
}

// needm is called when a cgo callback happens on a
// thread without an m (a thread not created by Go).
// In this case, needm is expected to find an m to use
// and return with m, g initialized correctly.
// Since m and g are not set now (likely nil, but see below)
// needm is limited in what routines it can call. In particular
// it can only call nosplit functions (textflag 7) and cannot
// do any scheduling that requires an m.
//
// In order to avoid needing heavy lifting here, we adopt
// the following strategy: there is a stack of available m's
// that can be stolen. Using compare-and-swap
// to pop from the stack has ABA races, so we simulate
// a lock by doing an exchange (via Casuintptr) to steal the stack
// head and replace the top pointer with MLOCKED (1).
// This serves as a simple spin lock that we can use even
// without an m. The thread that locks the stack in this way
// unlocks the stack by storing a valid stack head pointer.
//
// In order to make sure that there is always an m structure
// available to be stolen, we maintain the invariant that there
// is always one more than needed. At the beginning of the
// program (if cgo is in use) the list is seeded with a single m.
// If needm finds that it has taken the last m off the list, its job
// is - once it has installed its own m so that it can do things like
// allocate memory - to create a spare m and put it on the list.
//
// Each of these extra m's also has a g0 and a curg that are
// pressed into service as the scheduling stack and current
// goroutine for the duration of the cgo callback.
//
// When the callback is done with the m, it calls dropm to
// put the m back on the list.
//go:nosplit
func needm() {
	if (iscgo || GOOS == "windows") && !cgoHasExtraM {
		// Can happen if C/C++ code calls Go from a global ctor.
		// Can also happen on Windows if a global ctor uses a
		// callback created by syscall.NewCallback. See issue #6751
		// for details.
		//
		// Can not throw, because scheduler is not initialized yet.
		write(2, unsafe.Pointer(&earlycgocallback[0]), int32(len(earlycgocallback)))
		exit(1)
	}

	// Save and block signals before getting an M.
	// The signal handler may call needm itself,
	// and we must avoid a deadlock. Also, once g is installed,
	// any incoming signals will try to execute,
	// but we won't have the sigaltstack settings and other data
	// set up appropriately until the end of minit, which will
	// unblock the signals. This is the same dance as when
	// starting a new m to run Go code via newosproc.
	var sigmask sigset
	sigsave(&sigmask)
	sigblock(false)

	// Lock extra list, take head, unlock popped list.
	// nilokay=false is safe here because of the invariant above,
	// that the extra list always contains or will soon contain
	// at least one m.
	mp := lockextra(false)

	// Set needextram when we've just emptied the list,
	// so that the eventual call into cgocallbackg will
	// allocate a new m for the extra list. We delay the
	// allocation until then so that it can be done
	// after exitsyscall makes sure it is okay to be
	// running at all (that is, there's no garbage collection
	// running right now).
	mp.needextram = mp.schedlink == 0
	extraMCount--
	unlockextra(mp.schedlink.ptr())

	// Store the original signal mask for use by minit.
	mp.sigmask = sigmask

	// Install g (= m->g0) and set the stack bounds
	// to match the current stack. We don't actually know
	// how big the stack is, like we don't know how big any
	// scheduling stack is, but we assume there's at least 32 kB,
	// which is more than enough for us.
	setg(mp.g0)
	_g_ := getg()
	_g_.stack.hi = getcallersp() + 1024
	_g_.stack.lo = getcallersp() - 32*1024
	_g_.stackguard0 = _g_.stack.lo + _StackGuard

	// Initialize this thread to use the m.
	asminit()
	minit()

	// mp.curg is now a real goroutine.
	casgstatus(mp.curg, _Gdead, _Gsyscall)
	atomic.Xadd(&sched.ngsys, -1)
}

var earlycgocallback = []byte("fatal error: cgo callback before cgo call\n")

// newextram allocates m's and puts them on the extra list.
// It is called with a working local m, so that it can do things
// like call schedlock and allocate.
func newextram() {
	c := atomic.Xchg(&extraMWaiters, 0)
	if c > 0 {
		for i := uint32(0); i < c; i++ {
			oneNewExtraM()
		}
	} else {
		// Make sure there is at least one extra M.
		mp := lockextra(true)
		unlockextra(mp)
		if mp == nil {
			oneNewExtraM()
		}
	}
}

// oneNewExtraM allocates an m and puts it on the extra list.
func oneNewExtraM() {
	// Create extra goroutine locked to extra m.
	// The goroutine is the context in which the cgo callback will run.
	// The sched.pc will never be returned to, but setting it to
	// goexit makes clear to the traceback routines where
	// the goroutine stack ends.
	mp := allocm(nil, nil, -1)
	gp := malg(4096)
	gp.sched.pc = funcPC(goexit) + sys.PCQuantum
	gp.sched.sp = gp.stack.hi
	gp.sched.sp -= 4 * sys.RegSize // extra space in case of reads slightly beyond frame
	gp.sched.lr = 0
	gp.sched.g = guintptr(unsafe.Pointer(gp))
	gp.syscallpc = gp.sched.pc
	gp.syscallsp = gp.sched.sp
	gp.stktopsp = gp.sched.sp
	// malg returns status as _Gidle. Change to _Gdead before
	// adding to allg where GC can see it. We use _Gdead to hide
	// this from tracebacks and stack scans since it isn't a
	// "real" goroutine until needm grabs it.
	casgstatus(gp, _Gidle, _Gdead)
	gp.m = mp
	mp.curg = gp
	mp.lockedInt++
	mp.lockedg.set(gp)
	gp.lockedm.set(mp)
	gp.goid = int64(atomic.Xadd64(&sched.goidgen, 1))
	if raceenabled {
		gp.racectx = racegostart(funcPC(newextram) + sys.PCQuantum)
	}
	// put on allg for garbage collector
	allgadd(gp)

	// gp is now on the allg list, but we don't want it to be
	// counted by gcount. It would be more "proper" to increment
	// sched.ngfree, but that requires locking. Incrementing ngsys
	// has the same effect.
	atomic.Xadd(&sched.ngsys, +1)

	// Add m to the extra list.
	mnext := lockextra(true)
	mp.schedlink.set(mnext)
	extraMCount++
	unlockextra(mp)
}

// dropm is called when a cgo callback has called needm but is now
// done with the callback and returning back into the non-Go thread.
// It puts the current m back onto the extra list.
//
// The main expense here is the call to signalstack to release the
// m's signal stack, and then the call to needm on the next callback
// from this thread. It is tempting to try to save the m for next time,
// which would eliminate both these costs, but there might not be
// a next time: the current thread (which Go does not control) might exit.
// If we saved the m for that thread, there would be an m leak each time
// such a thread exited. Instead, we acquire and release an m on each
// call. These should typically not be scheduling operations, just a few
// atomics, so the cost should be small.
//
// TODO(rsc): An alternative would be to allocate a dummy pthread per-thread
// variable using pthread_key_create. Unlike the pthread keys we already use
// on OS X, this dummy key would never be read by Go code. It would exist
// only so that we could register at thread-exit-time destructor.
// That destructor would put the m back onto the extra list.
// This is purely a performance optimization. The current version,
// in which dropm happens on each cgo call, is still correct too.
// We may have to keep the current version on systems with cgo
// but without pthreads, like Windows.
func dropm() {
	// Clear m and g, and return m to the extra list.
	// After the call to setg we can only call nosplit functions
	// with no pointer manipulation.
	mp := getg().m

	// Return mp.curg to dead state.
	casgstatus(mp.curg, _Gsyscall, _Gdead)
	mp.curg.preemptStop = false
	atomic.Xadd(&sched.ngsys, +1)

	// Block signals before unminit.
	// Unminit unregisters the signal handling stack (but needs g on some systems).
	// Setg(nil) clears g, which is the signal handler's cue not to run Go handlers.
	// It's important not to try to handle a signal between those two steps.
	sigmask := mp.sigmask
	sigblock(false)
	unminit()

	mnext := lockextra(true)
	extraMCount++
	mp.schedlink.set(mnext)

	setg(nil)

	// Commit the release of mp.
	unlockextra(mp)

	msigrestore(sigmask)
}

// A helper function for EnsureDropM.
func getm() uintptr {
	return uintptr(unsafe.Pointer(getg().m))
}

var extram uintptr
var extraMCount uint32 // Protected by lockextra
var extraMWaiters uint32

// lockextra locks the extra list and returns the list head.
// The caller must unlock the list by storing a new list head
// to extram. If nilokay is true, then lockextra will
// return a nil list head if that's what it finds. If nilokay is false,
// lockextra will keep waiting until the list head is no longer nil.
//go:nosplit
func lockextra(nilokay bool) *m {
	const locked = 1

	incr := false
	for {
		old := atomic.Loaduintptr(&extram)
		if old == locked {
			osyield()
			continue
		}
		if old == 0 && !nilokay {
			if !incr {
				// Add 1 to the number of threads
				// waiting for an M.
				// This is cleared by newextram.
				atomic.Xadd(&extraMWaiters, 1)
				incr = true
			}
			usleep(1)
			continue
		}
		if atomic.Casuintptr(&extram, old, locked) {
			return (*m)(unsafe.Pointer(old))
		}
		osyield()
		continue
	}
}

//go:nosplit
func unlockextra(mp *m) {
	atomic.Storeuintptr(&extram, uintptr(unsafe.Pointer(mp)))
}

// execLock serializes exec and clone to avoid bugs or unspecified behaviour
// around exec'ing while creating/destroying threads.  See issue #19546.
var execLock rwmutex

// newmHandoff contains a list of m structures that need new OS threads.
// This is used by newm in situations where newm itself can't safely
// start an OS thread.
var newmHandoff struct {
	lock mutex

	// newm points to a list of M structures that need new OS
	// threads. The list is linked through m.schedlink.
	newm muintptr

	// waiting indicates that wake needs to be notified when an m
	// is put on the list.
	waiting bool
	wake    note

	// haveTemplateThread indicates that the templateThread has
	// been started. This is not protected by lock. Use cas to set
	// to 1.
	haveTemplateThread uint32
}

// Create a new m. It will start off with a call to fn, or else the scheduler.
// fn needs to be static and not a heap allocated closure.
// May run with m.p==nil, so write barriers are not allowed.
//
// id is optional pre-allocated m ID. Omit by passing -1.
//go:nowritebarrierrec
func newm(fn func(), _p_ *p, id int64) {
	mp := allocm(_p_, fn, id)
	mp.doesPark = (_p_ != nil) // 注释：如果p有数据时使用mp.park
	mp.nextp.set(_p_)          // 注释：设置m启动时执行的p
	mp.sigmask = initSigmask   // 注释：初始化信号掩码
	if gp := getg(); gp != nil && gp.m != nil && (gp.m.lockedExt != 0 || gp.m.incgo) && GOOS != "plan9" {
		// We're on a locked M or a thread that may have been
		// started by C. The kernel state of this thread may
		// be strange (the user may have locked it for that
		// purpose). We don't want to clone that into another
		// thread. Instead, ask a known-good thread to create
		// the thread for us.
		//
		// This is disabled on Plan 9. See golang.org/issue/22227.
		//
		// TODO: This may be unnecessary on Windows, which
		// doesn't model thread creation off fork.
		lock(&newmHandoff.lock)
		if newmHandoff.haveTemplateThread == 0 {
			throw("on a locked thread with no template thread")
		}
		mp.schedlink = newmHandoff.newm
		newmHandoff.newm.set(mp)
		if newmHandoff.waiting {
			newmHandoff.waiting = false
			notewakeup(&newmHandoff.wake)
		}
		unlock(&newmHandoff.lock)
		return
	}
	newm1(mp)
}

func newm1(mp *m) {
	if iscgo {
		var ts cgothreadstart
		if _cgo_thread_start == nil {
			throw("_cgo_thread_start missing")
		}
		ts.g.set(mp.g0)
		ts.tls = (*uint64)(unsafe.Pointer(&mp.tls[0]))
		ts.fn = unsafe.Pointer(funcPC(mstart))
		if msanenabled {
			msanwrite(unsafe.Pointer(&ts), unsafe.Sizeof(ts))
		}
		execLock.rlock() // Prevent process clone.
		asmcgocall(_cgo_thread_start, unsafe.Pointer(&ts))
		execLock.runlock()
		return
	}
	execLock.rlock() // Prevent process clone.
	newosproc(mp)
	execLock.runlock()
}

// startTemplateThread starts the template thread if it is not already
// running.
//
// The calling thread must itself be in a known-good state.
func startTemplateThread() {
	if GOARCH == "wasm" { // no threads on wasm yet
		return
	}

	// Disable preemption to guarantee that the template thread will be
	// created before a park once haveTemplateThread is set.
	mp := acquirem()
	if !atomic.Cas(&newmHandoff.haveTemplateThread, 0, 1) {
		releasem(mp)
		return
	}
	newm(templateThread, nil, -1)
	releasem(mp)
}

// mFixupRace is used to temporarily borrow the race context from the
// coordinating m during a syscall_runtime_doAllThreadsSyscall and
// loan it out to each of the m's of the runtime so they can execute a
// mFixup.fn in that context.
var mFixupRace struct {
	lock mutex
	ctx  uintptr
}

// mDoFixup runs any outstanding fixup function for the running m.
// Returns true if a fixup was outstanding and actually executed.
//
// Note: to avoid deadlocks, and the need for the fixup function
// itself to be async safe, signals are blocked for the working m
// while it holds the mFixup lock. (See golang.org/issue/44193)
//
//go:nosplit
func mDoFixup() bool {
	_g_ := getg()
	if used := atomic.Load(&_g_.m.mFixup.used); used == 0 {
		return false
	}

	// slow path - if fixup fn is used, block signals and lock.
	var sigmask sigset
	sigsave(&sigmask)
	sigblock(false)
	lock(&_g_.m.mFixup.lock)
	fn := _g_.m.mFixup.fn
	if fn != nil {
		if gcphase != _GCoff {
			// We can't have a write barrier in this
			// context since we may not have a P, but we
			// clear fn to signal that we've executed the
			// fixup. As long as fn is kept alive
			// elsewhere, technically we should have no
			// issues with the GC, but fn is likely
			// generated in a different package altogether
			// that may change independently. Just assert
			// the GC is off so this lack of write barrier
			// is more obviously safe.
			throw("GC must be disabled to protect validity of fn value")
		}
		if _g_.racectx != 0 || !raceenabled {
			fn(false)
		} else {
			// temporarily acquire the context of the
			// originator of the
			// syscall_runtime_doAllThreadsSyscall and
			// block others from using it for the duration
			// of the fixup call.
			lock(&mFixupRace.lock)
			_g_.racectx = mFixupRace.ctx
			fn(false)
			_g_.racectx = 0
			unlock(&mFixupRace.lock)
		}
		*(*uintptr)(unsafe.Pointer(&_g_.m.mFixup.fn)) = 0
		atomic.Store(&_g_.m.mFixup.used, 0)
	}
	unlock(&_g_.m.mFixup.lock)
	msigrestore(sigmask)
	return fn != nil
}

// mDoFixupAndOSYield is called when an m is unable to send a signal
// because the allThreadsSyscall mechanism is in progress. That is, an
// mPark() has been interrupted with this signal handler so we need to
// ensure the fixup is executed from this context.
//go:nosplit
func mDoFixupAndOSYield() {
	mDoFixup()
	osyield()
}

// templateThread is a thread in a known-good state that exists solely
// to start new threads in known-good states when the calling thread
// may not be in a good state.
//
// Many programs never need this, so templateThread is started lazily
// when we first enter a state that might lead to running on a thread
// in an unknown state.
//
// templateThread runs on an M without a P, so it must not have write
// barriers.
//
//go:nowritebarrierrec
func templateThread() {
	lock(&sched.lock)
	sched.nmsys++
	checkdead()
	unlock(&sched.lock)

	for {
		lock(&newmHandoff.lock)
		for newmHandoff.newm != 0 {
			newm := newmHandoff.newm.ptr()
			newmHandoff.newm = 0
			unlock(&newmHandoff.lock)
			for newm != nil {
				next := newm.schedlink.ptr()
				newm.schedlink = 0
				newm1(newm)
				newm = next
			}
			lock(&newmHandoff.lock)
		}
		newmHandoff.waiting = true
		noteclear(&newmHandoff.wake)
		unlock(&newmHandoff.lock)
		notesleep(&newmHandoff.wake)
		mDoFixup()
	}
}

// Stops execution of the current m until new work is available.
// Returns with acquired P.
func stopm() {
	_g_ := getg()

	if _g_.m.locks != 0 {
		throw("stopm holding locks")
	}
	if _g_.m.p != 0 {
		throw("stopm holding p")
	}
	if _g_.m.spinning {
		throw("stopm spinning")
	}

	lock(&sched.lock)
	mput(_g_.m) // 注释：把当前的M加入到空闲M链表中(空闲M链表是在全局的调度器中，所以需要加锁执行)
	unlock(&sched.lock)
	mPark()
	acquirep(_g_.m.nextp.ptr()) // 注释：(获得P)当前线程m和p相互绑定，并且把p的状态从_Pidle设置成_Prunning
	_g_.m.nextp = 0
}

func mspinning() {
	// startm's caller incremented nmspinning. Set the new M's spinning.
	getg().m.spinning = true
}

// Schedules some M to run the p (creates an M if necessary).
// If p==nil, tries to get an idle P, if no idle P's does nothing.
// May run with m.p==nil, so write barriers are not allowed.
// If spinning is set, the caller has incremented nmspinning and startm will
// either decrement nmspinning or set m.spinning in the newly started M.
// Callers passing a non-nil P must call from a non-preemptible context. See
// comment on acquirem below.
// Must not have write barriers because this may be called without a P.
// 注释：译：安排一些M来运行p（如果需要，创建一个M）。如果p==nil，则尝试获取空闲p，如果没有空闲p则什么也不做。可能以m.p==nil运行，因此不允许写入障碍。如果设置了spinning，调用者将增加nmspinning，
//		startm将减少nmspinning或在新启动的m中设置m.spinning。传递非nil P的调用方必须从非抢占上下文调用。看见下面是对收购的评论。不能有写障碍，因为这可能在没有P的情况下调用。
// 注释：用M把P进行连接，然后想系统发送信号，唤醒这个M
// 注释：拿个M去跑P，如果没有拿到M则创建新的M；如果P为nil，创建一个空闲P。
// 注释：参数spinning：是否自旋，表示开始抢别的G了
// 注释：步骤：
//		1.当前M禁止抢占
//		2.获取P（如果参数_p_有值则使用，否则拿个空闲P，如果没有空闲P则解除抢占并退出)
//		3.获取新M（获取空闲M，如果没有则创建新M）
//		4.新M设置自旋（等于参数spinning）
//		5.新M设置下一个要执行的P，当新M被唤醒时第一个执行的P
//		6.唤醒线程M（以系统信号的方式，不同系统采用不同的方法实现）
//		7.当前M解除禁止抢占
//go:nowritebarrierrec
func startm(_p_ *p, spinning bool) {
	// Disable preemption.
	// 注释：禁用抢占。
	// Every owned P must have an owner that will eventually stop it in the
	// event of a GC stop request. startm takes transient ownership of a P
	// (either from argument or pidleget below) and transfers ownership to
	// a started M, which will be responsible for performing the stop.
	// 注释：每个拥有的P必须有一个所有者，在GC停止请求的情况下，该所有者最终会停止它。startm获取P的暂时所有权（来自参数或下面的pidleget），并将所有权转移到启动的M，后者将负责执行停止。
	//
	// Preemption must be disabled during this transient ownership,
	// otherwise the P this is running on may enter GC stop while still
	// holding the transient P, leaving that P in limbo and deadlocking the
	// STW.
	// 注释：在这个瞬态所有权期间，必须禁用抢占，否则正在运行的P可能会在保持瞬态P的同时进入GC停止，从而使该P处于不确定状态并死锁STW(stop the world)
	//
	// Callers passing a non-nil P must already be in non-preemptible
	// context, otherwise such preemption could occur on function entry to
	// startm. Callers passing a nil P may be preemptible, so we must
	// disable preemption before acquiring a P from pidleget below.
	// 注释：传递非nil P的调用方必须已经在非抢占上下文中，否则这种抢占可能发生在startm的函数入口。传递nil P的调用方可能是可抢占的，因此我们必须在从下面的pidleget获取P之前禁用抢占
	mp := acquirem()  // 注释：获取当前g
	lock(&sched.lock) // 注释：调度器加锁
	// 注释：如果没有找到当前工作的g
	if _p_ == nil {
		_p_ = pidleget() // 注释：到空闲p链表里获取一个p
		// 注释：如果没有找到p
		if _p_ == nil {
			unlock(&sched.lock) // 注释：解锁调度器
			// 注释：如果线程m自旋中（线程m正在工作）
			if spinning {
				// The caller incremented nmspinning, but there are no idle Ps,
				// so it's okay to just undo the increment and give up.
				// 注释：自旋数减一
				if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
					throw("startm: negative nmspinning")
				}
			}
			releasem(mp) // 注释：释放线程m
			return
		}
	}
	nmp := mget() // 注释：获取空闲的m
	// 注释：如果没有找到空闲的m则需要创建一个新m
	if nmp == nil {
		// No M is available, we must drop sched.lock and call newm.
		// However, we already own a P to assign to the M.
		//
		// Once sched.lock is released, another G (e.g., in a syscall),
		// could find no idle P while checkdead finds a runnable G but
		// no running M's because this new M hasn't started yet, thus
		// throwing in an apparent deadlock.
		//
		// Avoid this situation by pre-allocating the ID for the new M,
		// thus marking it as 'running' before we drop sched.lock. This
		// new M will eventually run the scheduler to execute any
		// queued G's.
		id := mReserveID()  // 注释：获取新建m的主键ID
		unlock(&sched.lock) // 注释：解锁调度器

		var fn func()
		// 注释：判断是否试图抢占
		if spinning {
			// The caller incremented nmspinning, so set m.spinning in the new M.
			fn = mspinning // 注释：把试图发生抢占的标记函数绑定到m结构体的mstartfn上
		}
		newm(fn, _p_, id) // 注释：新建m
		// Ownership transfer of _p_ committed by start in newm.
		// 注释：由newm中的start提交的_p_所有权转让。
		// Preemption is now safe.
		// 注释：抢占现在是安全的。
		releasem(mp) // 注释：释放线程m
		return
	}
	unlock(&sched.lock) // 注释：解锁调度器
	if nmp.spinning {
		throw("startm: m is spinning")
	}
	if nmp.nextp != 0 {
		throw("startm: m has p")
	}
	if spinning && !runqempty(_p_) {
		throw("startm: p has runnable gs")
	}
	// The caller incremented nmspinning, so set m.spinning in the new M.
	nmp.spinning = spinning // 注释；（我开始要抢别人了）新线程m设置可以试图抢占
	nmp.nextp.set(_p_)      // 注释：(设置下一个要执行的P)新线程m下一个要执行的p（起始任务函数）(nmp.nextp = _p_)
	notewakeup(&nmp.park)   // 注释：（唤醒线程M，以系统信号的方式）向系统发送信号，通知新线程m唤醒(不同操作做系统走不同的文件)
	// Ownership transfer of _p_ committed by wakeup. Preemption is now
	// safe.
	releasem(mp) // 注释：释放线程m
}

// Hands off P from syscall or locked M. // 注释：切换(移交、让渡)p从系统调用或者锁定的m
// Always runs without a P, so write barriers are not allowed.
// 注释：让渡p,调度另一个或新的m运行这个p
//go:nowritebarrierrec
func handoffp(_p_ *p) {
	// handoffp must start an M in any situation where
	// findrunnable would return a G to run on _p_.

	// if it has local work, start it straight away
	// 注释：如果是本地g运行队列有值或全局运行队列有值就直接启动
	if !runqempty(_p_) || sched.runqsize != 0 {
		startm(_p_, false) // 注释：用另一个m跑这个p
		return
	}
	// if it has GC work, start it straight away // 注释：如果是GC则直接启动
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(_p_) {
		startm(_p_, false) // 注释：用另一个m跑这个p
		return
	}
	// no local work, check that there are no spinning/idle M's,
	// otherwise our help is not required
	// 注释：如果没有自旋的m并且没有空闲的p时，执行&sched.nmspinning=1并且执行startm
	if atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) == 0 && atomic.Cas(&sched.nmspinning, 0, 1) { // TODO: fast atomic
		startm(_p_, true) // 注释：用另一个m跑这个p
		return
	}
	lock(&sched.lock) // 注释：上锁，准备对sched结构体进行修改
	if sched.gcwaiting != 0 {
		_p_.status = _Pgcstop
		sched.stopwait--
		if sched.stopwait == 0 {
			notewakeup(&sched.stopnote)
		}
		unlock(&sched.lock)
		return
	}
	if _p_.runSafePointFn != 0 && atomic.Cas(&_p_.runSafePointFn, 1, 0) {
		sched.safePointFn(_p_)
		sched.safePointWait--
		if sched.safePointWait == 0 {
			notewakeup(&sched.safePointNote)
		}
	}
	if sched.runqsize != 0 {
		unlock(&sched.lock)
		startm(_p_, false)
		return
	}
	// If this is the last running P and nobody is polling network,
	// need to wakeup another M to poll network.
	if sched.npidle == uint32(gomaxprocs-1) && atomic.Load64(&sched.lastpoll) != 0 {
		unlock(&sched.lock)
		startm(_p_, false)
		return
	}

	// The scheduler lock cannot be held when calling wakeNetPoller below
	// because wakeNetPoller may call wakep which may call startm.
	when := nobarrierWakeTime(_p_)
	pidleput(_p_) // 注释：把p放到空闲队列的头部
	unlock(&sched.lock)

	if when != 0 {
		wakeNetPoller(when)
	}
}

// Tries to add one more P to execute G's.
// Called when a G is made runnable (newproc, ready).
// 注释：译：尝试再添加一个P以执行G。当G可以运行时调用（newproc，ready）。
// 注释：拿个空闲M线程运行空闲P，并且自旋，开始抢别的G了
func wakep() {
	if atomic.Load(&sched.npidle) == 0 {
		return
	}
	// be conservative about spinning threads
	if atomic.Load(&sched.nmspinning) != 0 || !atomic.Cas(&sched.nmspinning, 0, 1) {
		return
	}
	startm(nil, true) // 注释：拿个空闲M线程运行空闲P，并且自旋，开始抢别的G了
}

// Stops execution of the current m that is locked to a g until the g is runnable again.
// Returns with acquired P.
// 注释：解除和m的锁定，并且把m.nextp上的p和m相互绑定
func stoplockedm() {
	_g_ := getg()

	// 注释：当前g锁定(绑定)的m(当前g只能在这个m上运行)不等于g绑定的m时报错
	if _g_.m.lockedg == 0 || _g_.m.lockedg.ptr().lockedm.ptr() != _g_.m {
		throw("stoplockedm: inconsistent locking")
	}
	if _g_.m.p != 0 {
		// Schedule another M to run this p. // 注释：调度另一个m运行这个p
		_p_ := releasep() // 注释：解除p(当前g对应的p)和当前m的绑定,并返回p
		handoffp(_p_)
	}
	incidlelocked(1)
	// Wait until another thread schedules lockedg again.
	mPark()
	status := readgstatus(_g_.m.lockedg.ptr())
	if status&^_Gscan != _Grunnable {
		print("runtime:stoplockedm: lockedg (atomicstatus=", status, ") is not Grunnable or Gscanrunnable\n")
		dumpgstatus(_g_.m.lockedg.ptr())
		throw("stoplockedm: not runnable")
	}
	acquirep(_g_.m.nextp.ptr()) // 注释：把m.nextp上的p和m相互绑定
	_g_.m.nextp = 0             // 注释：清空m.nextp上的p指针
}

// Schedules the locked m to run the locked gp.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func startlockedm(gp *g) {
	_g_ := getg()

	mp := gp.lockedm.ptr()
	if mp == _g_.m {
		throw("startlockedm: locked to me")
	}
	if mp.nextp != 0 {
		throw("startlockedm: m has p")
	}
	// directly handoff current P to the locked m
	incidlelocked(-1)
	_p_ := releasep()
	mp.nextp.set(_p_)
	notewakeup(&mp.park)
	stopm()
}

// Stops the current m for stopTheWorld.
// Returns when the world is restarted.
// 注释：停止（休眠）M
func gcstopm() {
	_g_ := getg()

	if sched.gcwaiting == 0 { // 注释：等于0时说明已经start the world 了
		throw("gcstopm: not waiting for gc")
	}
	if _g_.m.spinning { // 注释：如果线程M试图偷其他线程的G时，需要关闭，不能再偷了哈
		_g_.m.spinning = false
		// OK to just drop nmspinning here,
		// startTheWorld will unpark threads as necessary.
		// 注释：关闭自旋的线程M,自旋说明当前线程M没有需要执行的G,试图从其他线程M中偷取G
		if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 {
			throw("gcstopm: negative nmspinning")
		}
	}
	_p_ := releasep() // 注释：释放P,解除P和M的绑定
	lock(&sched.lock)
	_p_.status = _Pgcstop    // 注释：设置P的状态为GCstop，GC停止世界（STW）时把当前的P也停止了，并设置这个状态
	sched.stopwait--         // 注释：停止等待，默认值是cup核数，冻结时值为一个很大的值，STW时减1
	if sched.stopwait == 0 { // 注释：如果没有需要停止等待的调度
		notewakeup(&sched.stopnote)
	}
	unlock(&sched.lock)
	stopm()
}

// Schedules gp to run on the current M.
// If inheritTime is true, gp inherits the remaining time in the
// current time slice. Otherwise, it starts a new time slice.
// Never returns.
//
// Write barriers are allowed because this is called immediately after
// acquiring a P in several places.
// 注释：译：计划gp在当前M上运行。如果inheritTime为true，则gp将继承当前时间片中的剩余时间。否则，它将启动一个新的时间片。永不回头。
// 		写入障碍是允许的，因为这是在几个地方获得P后立即调用的。
//
// 注释：如果inheritTime是true则立刻执行gp
//
//go:yeswritebarrierrec
func execute(gp *g, inheritTime bool) {
	_g_ := getg()

	// Assign gp.m before entering _Grunning so running Gs have an
	// M.
	_g_.m.curg = gp                       // 注释：(还原业务G)把要执行的G绑定到当前G的M对应的当前G上
	gp.m = _g_.m                          // 注释：把要执行的G对应的M绑定到当前已经存在的G对应的M上
	casgstatus(gp, _Grunnable, _Grunning) // 注释：更新G的状态为运行中
	gp.waitsince = 0
	gp.preempt = false                         // 注释：禁止抢占
	gp.stackguard0 = gp.stack.lo + _StackGuard // 注释：设置爆栈警告
	if !inheritTime {
		_g_.m.p.ptr().schedtick++ // 注释：调度计数器递增
	}

	// Check whether the profiler needs to be turned on or off.
	hz := sched.profilehz
	if _g_.m.profilehz != hz {
		setThreadCPUProfiler(hz)
	}

	// 注释：是否开启追踪
	if trace.enabled {
		// GoSysExit has to happen when we have a P, but before GoStart.
		// So we emit it here.
		if gp.syscallsp != 0 && gp.sysblocktraced {
			traceGoSysExit(gp.sysexitticks)
		}
		traceGoStart()
	}

	gogo(&gp.sched) // 注释：(执行)真正执行G里的指令(在G休眠的时候会保存现场，保存现场就是保存到&gp.sched里，所以唤醒后执行这里的指令)
}

// Finds a runnable goroutine to execute.
// Tries to steal from other P's, get g from local or global queue, poll network.
// 注释：获取可以运行的G；获取顺序是：先从本地P中获取-》全局队列中获取-》网络轮询，已经就绪的网络连接中获取（优化方案）-》去其他线程的本地队列里窃取（偷）
func findrunnable() (gp *g, inheritTime bool) {
	_g_ := getg()

	// The conditions here and in handoffp must agree: if
	// findrunnable would return a G to run, handoffp must start
	// an M.

top:
	_p_ := _g_.m.p.ptr()      // 注释：获取当前运行的P
	if sched.gcwaiting != 0 { // 注释：GC启动STW时设置为1，等待所有M全部停止
		gcstopm() // 注释：停止（休眠）M
		goto top
	}
	if _p_.runSafePointFn != 0 {
		runSafePointFn()
	}

	now, pollUntil, _ := checkTimers(_p_, 0)

	if fingwait && fingwake {
		if gp := wakefing(); gp != nil {
			ready(gp, 0, true)
		}
	}
	if *cgo_yield != nil {
		asmcgocall(*cgo_yield, nil)
	}

	// local runq
	// 注释：在本地P队列中获取G
	if gp, inheritTime := runqget(_p_); gp != nil {
		return gp, inheritTime
	}

	// global runq
	// 注释：到全局队列中获取G
	if sched.runqsize != 0 {
		lock(&sched.lock)
		gp := globrunqget(_p_, 0) // 注释：从全局队列中获取G
		unlock(&sched.lock)
		if gp != nil {
			return gp, false
		}
	}

	// Poll network.
	// 网络轮询
	// This netpoll is only an optimization before we resort to stealing.
	// 注释：在我们采取偷窃手段之前，这个网络投票只是一种优化。
	// We can safely skip it if there are no waiters or a thread is blocked
	// in netpoll already. If there is any kind of logical race with that
	// blocked thread (e.g. it has already returned from netpoll, but does
	// not set lastpoll yet), this thread will do blocking netpoll below
	// anyway.
	// 注释：网络轮询，是个优化方案
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && atomic.Load64(&sched.lastpoll) != 0 {
		// 注释：netpoll检查就绪的网络连接,返回可运行的goroutine列表
		if list := netpoll(0); !list.empty() { // non-blocking
			gp := list.pop()
			injectglist(&list)
			casgstatus(gp, _Gwaiting, _Grunnable) // 注释：修改G的状态如果等于_Gwaiting时则修改为_Grunnable
			if trace.enabled {
				traceGoUnpark(gp, 0)
			}
			return gp, false
		}
	}

	// Steal work from other P's.
	procs := uint32(gomaxprocs)
	ranTimer := false
	// If number of spinning M's >= number of busy P's, block.
	// This is necessary to prevent excessive CPU consumption
	// when GOMAXPROCS>>1 but the program parallelism is low.
	// 注释：如果没有打算去窃取（偷）时 && 2倍自旋（空闲）的M数量 >= 活动P数（总P数 - 空闲P数）则直接停止
	if !_g_.m.spinning && 2*atomic.Load(&sched.nmspinning) >= procs-atomic.Load(&sched.npidle) {
		goto stop
	}
	// 注释：设置状态准备窃取（偷）
	if !_g_.m.spinning { // 注释：如果M为非自旋，则设置为自旋状态
		_g_.m.spinning = true             // 注释：设置为自旋，变更状态为true，说明自己已经空闲了打算去窃取（偷）其他的线程M本地的G了
		atomic.Xadd(&sched.nmspinning, 1) // 注释：自旋（空闲）数加1
	}
	const stealTries = 4 // 注释：尝试窃取（偷）的数量
	for i := 0; i < stealTries; i++ {
		stealTimersOrRunNextG := i == stealTries-1 // 注释：最后一次循环（true时false否）

		// 注释：随机拿出一个P，通过stealOrder.reset(P的总数)初始化
		for enum := stealOrder.start(fastrand()); !enum.done(); enum.next() {
			if sched.gcwaiting != 0 {
				goto top
			}
			p2 := allp[enum.position()] // 注释：所有的P数组中随机拿出一个P
			if _p_ == p2 {              // 注释：如果拿出的P是当前P则跳过；判读是否是当前的P，跳过当前的P
				continue
			}

			// Steal timers from p2. This call to checkTimers is the only place
			// where we might hold a lock on a different P's timers. We do this
			// once on the last pass before checking runnext because stealing
			// from the other P's runnext should be the last resort, so if there
			// are timers to steal do that first.
			//
			// We only check timers on one of the stealing iterations because
			// the time stored in now doesn't change in this loop and checking
			// the timers for each P more than once with the same value of now
			// is probably a waste of time.
			//
			// timerpMask tells us whether the P may have timers at all. If it
			// can't, no need to check at all.
			if stealTimersOrRunNextG && timerpMask.read(enum.position()) {
				tnow, w, ran := checkTimers(p2, now)
				now = tnow
				if w != 0 && (pollUntil == 0 || w < pollUntil) {
					pollUntil = w
				}
				if ran {
					// Running the timers may have
					// made an arbitrary number of G's
					// ready and added them to this P's
					// local run queue. That invalidates
					// the assumption of runqsteal
					// that is always has room to add
					// stolen G's. So check now if there
					// is a local G to run.
					if gp, inheritTime := runqget(_p_); gp != nil {
						return gp, inheritTime
					}
					ranTimer = true
				}
			}

			// Don't bother to attempt to steal if p2 is idle. // 注释： 如果p2空闲，不要费心去偷。
			if !idlepMask.read(enum.position()) {
				if gp := runqsteal(_p_, p2, stealTimersOrRunNextG); gp != nil { // 注释：向P2中窃取（偷）一些G
					return gp, false
				}
			}
		}
	}
	if ranTimer {
		// Running a timer may have made some goroutine ready.
		goto top
	}

	// 注释： 实在找不到G，那就休眠(此时的M一定不是自旋状态)
stop:

	// We have nothing to do. If we're in the GC mark phase, can
	// safely scan and blacken objects, and have work to do, run
	// idle-time marking rather than give up the P.
	if gcBlackenEnabled != 0 && gcMarkWorkAvailable(_p_) {
		node := (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
		if node != nil {
			_p_.gcMarkWorkerMode = gcMarkWorkerIdleMode
			gp := node.gp.ptr()
			casgstatus(gp, _Gwaiting, _Grunnable)
			if trace.enabled {
				traceGoUnpark(gp, 0)
			}
			return gp, false
		}
	}

	delta := int64(-1)
	if pollUntil != 0 {
		// checkTimers ensures that polluntil > now.
		delta = pollUntil - now
	}

	// wasm only:
	// If a callback returned and no other goroutine is awake,
	// then wake event handler goroutine which pauses execution
	// until a callback was triggered.
	gp, otherReady := beforeIdle(delta)
	if gp != nil {
		casgstatus(gp, _Gwaiting, _Grunnable)
		if trace.enabled {
			traceGoUnpark(gp, 0)
		}
		return gp, false
	}
	if otherReady {
		goto top
	}

	// Before we drop our P, make a snapshot of the allp slice,
	// which can change underfoot once we no longer block
	// safe-points. We don't need to snapshot the contents because
	// everything up to cap(allp) is immutable.
	allpSnapshot := allp
	// Also snapshot masks. Value changes are OK, but we can't allow
	// len to change out from under us.
	idlepMaskSnapshot := idlepMask
	timerpMaskSnapshot := timerpMask

	// return P and block
	lock(&sched.lock)
	if sched.gcwaiting != 0 || _p_.runSafePointFn != 0 {
		unlock(&sched.lock)
		goto top
	}
	if sched.runqsize != 0 {
		gp := globrunqget(_p_, 0)
		unlock(&sched.lock)
		return gp, false
	}
	if releasep() != _p_ {
		throw("findrunnable: wrong p")
	}
	pidleput(_p_)
	unlock(&sched.lock)

	// Delicate dance: thread transitions from spinning to non-spinning state,
	// potentially concurrently with submission of new goroutines. We must
	// drop nmspinning first and then check all per-P queues again (with
	// #StoreLoad memory barrier in between). If we do it the other way around,
	// another thread can submit a goroutine after we've checked all run queues
	// but before we drop nmspinning; as a result nobody will unpark a thread
	// to run the goroutine.
	// If we discover new work below, we need to restore m.spinning as a signal
	// for resetspinning to unpark a new worker thread (because there can be more
	// than one starving goroutine). However, if after discovering new work
	// we also observe no idle Ps, it is OK to just park the current thread:
	// the system is fully loaded so no spinning threads are required.
	// Also see "Worker thread parking/unparking" comment at the top of the file.
	wasSpinning := _g_.m.spinning
	if _g_.m.spinning { // 注释：如果M是自旋状态，则取消自旋
		_g_.m.spinning = false                             // 注释：取消自旋
		if int32(atomic.Xadd(&sched.nmspinning, -1)) < 0 { // 注释：自旋M个数减1
			throw("findrunnable: negative nmspinning")
		}
	}

	// check all runqueues once again
	// 注释：再次检查所有的P，看看有没有可以运行的G
	for id, _p_ := range allpSnapshot {
		if !idlepMaskSnapshot.read(uint32(id)) && !runqempty(_p_) {
			lock(&sched.lock)
			_p_ = pidleget()
			unlock(&sched.lock)
			if _p_ != nil {
				acquirep(_p_)
				if wasSpinning {
					_g_.m.spinning = true
					atomic.Xadd(&sched.nmspinning, 1)
				}
				goto top
			}
			break
		}
	}

	// Similar to above, check for timer creation or expiry concurrently with
	// transitioning from spinning to non-spinning. Note that we cannot use
	// checkTimers here because it calls adjusttimers which may need to allocate
	// memory, and that isn't allowed when we don't have an active P.
	for id, _p_ := range allpSnapshot {
		if timerpMaskSnapshot.read(uint32(id)) {
			w := nobarrierWakeTime(_p_)
			if w != 0 && (pollUntil == 0 || w < pollUntil) {
				pollUntil = w
			}
		}
	}
	if pollUntil != 0 {
		if now == 0 {
			now = nanotime()
		}
		delta = pollUntil - now
		if delta < 0 {
			delta = 0
		}
	}

	// Check for idle-priority GC work again.
	//
	// N.B. Since we have no P, gcBlackenEnabled may change at any time; we
	// must check again after acquiring a P.
	if atomic.Load(&gcBlackenEnabled) != 0 && gcMarkWorkAvailable(nil) {
		// Work is available; we can start an idle GC worker only if
		// there is an available P and available worker G.
		//
		// We can attempt to acquire these in either order. Workers are
		// almost always available (see comment in findRunnableGCWorker
		// for the one case there may be none). Since we're slightly
		// less likely to find a P, check for that first.
		lock(&sched.lock)
		var node *gcBgMarkWorkerNode
		_p_ = pidleget()
		if _p_ != nil {
			// Now that we own a P, gcBlackenEnabled can't change
			// (as it requires STW).
			if gcBlackenEnabled != 0 {
				node = (*gcBgMarkWorkerNode)(gcBgMarkWorkerPool.pop())
				if node == nil {
					pidleput(_p_)
					_p_ = nil
				}
			} else {
				pidleput(_p_)
				_p_ = nil
			}
		}
		unlock(&sched.lock)
		if _p_ != nil {
			acquirep(_p_)
			if wasSpinning {
				_g_.m.spinning = true
				atomic.Xadd(&sched.nmspinning, 1)
			}

			// Run the idle worker.
			_p_.gcMarkWorkerMode = gcMarkWorkerIdleMode
			gp := node.gp.ptr()
			casgstatus(gp, _Gwaiting, _Grunnable)
			if trace.enabled {
				traceGoUnpark(gp, 0)
			}
			return gp, false
		}
	}

	// poll network
	if netpollinited() && (atomic.Load(&netpollWaiters) > 0 || pollUntil != 0) && atomic.Xchg64(&sched.lastpoll, 0) != 0 {
		atomic.Store64(&sched.pollUntil, uint64(pollUntil))
		if _g_.m.p != 0 {
			throw("findrunnable: netpoll with p")
		}
		if _g_.m.spinning {
			throw("findrunnable: netpoll with spinning")
		}
		if faketime != 0 {
			// When using fake time, just poll.
			delta = 0
		}
		list := netpoll(delta) // block until new work is available
		atomic.Store64(&sched.pollUntil, 0)
		atomic.Store64(&sched.lastpoll, uint64(nanotime()))
		if faketime != 0 && list.empty() {
			// Using fake time and nothing is ready; stop M.
			// When all M's stop, checkdead will call timejump.
			stopm()
			goto top
		}
		lock(&sched.lock)
		_p_ = pidleget()
		unlock(&sched.lock)
		if _p_ == nil {
			injectglist(&list)
		} else {
			acquirep(_p_)
			if !list.empty() {
				gp := list.pop()
				injectglist(&list)
				casgstatus(gp, _Gwaiting, _Grunnable)
				if trace.enabled {
					traceGoUnpark(gp, 0)
				}
				return gp, false
			}
			if wasSpinning {
				_g_.m.spinning = true
				atomic.Xadd(&sched.nmspinning, 1)
			}
			goto top
		}
	} else if pollUntil != 0 && netpollinited() {
		pollerPollUntil := int64(atomic.Load64(&sched.pollUntil))
		if pollerPollUntil == 0 || pollerPollUntil > pollUntil {
			netpollBreak()
		}
	}
	stopm()
	goto top
}

// pollWork reports whether there is non-background work this P could
// be doing. This is a fairly lightweight check to be used for
// background work loops, like idle GC. It checks a subset of the
// conditions checked by the actual scheduler.
func pollWork() bool {
	if sched.runqsize != 0 {
		return true
	}
	p := getg().m.p.ptr()
	if !runqempty(p) {
		return true
	}
	if netpollinited() && atomic.Load(&netpollWaiters) > 0 && sched.lastpoll != 0 {
		if list := netpoll(0); !list.empty() {
			injectglist(&list)
			return true
		}
	}
	return false
}

// wakeNetPoller wakes up the thread sleeping in the network poller if it isn't
// going to wake up before the when argument; or it wakes an idle P to service
// timers and the network poller if there isn't one already.
func wakeNetPoller(when int64) {
	if atomic.Load64(&sched.lastpoll) == 0 {
		// In findrunnable we ensure that when polling the pollUntil
		// field is either zero or the time to which the current
		// poll is expected to run. This can have a spurious wakeup
		// but should never miss a wakeup.
		pollerPollUntil := int64(atomic.Load64(&sched.pollUntil))
		if pollerPollUntil == 0 || pollerPollUntil > when {
			netpollBreak()
		}
	} else {
		// There are no threads in the network poller, try to get
		// one there so it can handle new timers.
		if GOOS != "plan9" { // Temporary workaround - see issue #42303.
			wakep()
		}
	}
}

func resetspinning() {
	_g_ := getg()
	if !_g_.m.spinning {
		throw("resetspinning: not a spinning m")
	}
	_g_.m.spinning = false
	nmspinning := atomic.Xadd(&sched.nmspinning, -1)
	if int32(nmspinning) < 0 {
		throw("findrunnable: negative nmspinning")
	}
	// M wakeup policy is deliberately somewhat conservative, so check if we
	// need to wakeup another P here. See "Worker thread parking/unparking"
	// comment at the top of the file for details.
	wakep()
}

// injectglist adds each runnable G on the list to some run queue,
// and clears glist. If there is no current P, they are added to the
// global queue, and up to npidle M's are started to run them.
// Otherwise, for each idle P, this adds a G to the global queue
// and starts an M. Any remaining G's are added to the current P's
// local run queue.
// This may temporarily acquire sched.lock.
// Can run concurrently with GC.
func injectglist(glist *gList) {
	if glist.empty() {
		return
	}
	if trace.enabled {
		for gp := glist.head.ptr(); gp != nil; gp = gp.schedlink.ptr() {
			traceGoUnpark(gp, 0)
		}
	}

	// Mark all the goroutines as runnable before we put them
	// on the run queues.
	head := glist.head.ptr()
	var tail *g
	qsize := 0
	for gp := head; gp != nil; gp = gp.schedlink.ptr() {
		tail = gp
		qsize++
		casgstatus(gp, _Gwaiting, _Grunnable)
	}

	// Turn the gList into a gQueue.
	var q gQueue
	q.head.set(head)
	q.tail.set(tail)
	*glist = gList{}

	startIdle := func(n int) {
		for ; n != 0 && sched.npidle != 0; n-- {
			startm(nil, false)
		}
	}

	pp := getg().m.p.ptr()
	if pp == nil {
		lock(&sched.lock)
		globrunqputbatch(&q, int32(qsize))
		unlock(&sched.lock)
		startIdle(qsize)
		return
	}

	npidle := int(atomic.Load(&sched.npidle))
	var globq gQueue
	var n int
	for n = 0; n < npidle && !q.empty(); n++ {
		g := q.pop()
		globq.pushBack(g)
	}
	if n > 0 {
		lock(&sched.lock)
		globrunqputbatch(&globq, int32(n))
		unlock(&sched.lock)
		startIdle(n)
		qsize -= n
	}

	if !q.empty() {
		runqputbatch(pp, &q, qsize)
	}
}

// One round of scheduler: find a runnable goroutine and execute it.
// Never returns.
// 注释：一轮调度程序：找到一个可运行的goroutine并执行它
// 注释：永不返回
// 注释：每一轮调度的开始方法
func schedule() {
	_g_ := getg() // 注释：获取当前G，（TLS指针数据就是G的指针）

	if _g_.m.locks != 0 {
		throw("schedule: holding locks")
	}

	// 注释：判断当前的P是否被其他M抢走
	// 注释：g.m.lockedg有值说明m绑定的p被别的m抢走了，如果lockedg有值就要执行这里的g
	if _g_.m.lockedg != 0 {
		stoplockedm()                       // 注释：m和p解除绑定,m重新绑定m.nextp
		execute(_g_.m.lockedg.ptr(), false) // 注释：schedule:执行锁定的g // Never returns.
	}

	// We should not schedule away from a g that is executing a cgo call,
	// since the cgo call is using the m's g0 stack.
	if _g_.m.incgo {
		throw("schedule: in cgo")
	}

top:
	pp := _g_.m.p.ptr() // 注释：(当前运行的P指针)当前G绑定M里绑定的P指针
	pp.preempt = false  // 注释：禁止抢占

	// 注释：如果当前GC需要停止整个世界（STW), 则调用gcstopm休眠当前的M
	if sched.gcwaiting != 0 {
		gcstopm() // 注释：为了STW，停止当前的M
		goto top  // 注释：STW结束后回到 top
	}
	if pp.runSafePointFn != 0 {
		runSafePointFn() // 注释：如果pp.runSafePointFn != 0,运行sched.safePointFn
	}

	// Sanity check: if we are spinning, the run queue should be empty.
	// Check this before calling checkTimers, as that might call
	// goready to put a ready goroutine on the local run queue.
	// 注释：如果当前p队列还有数据时，去其他p队列里偷时报错
	if _g_.m.spinning && (pp.runnext != 0 || pp.runqhead != pp.runqtail) {
		throw("schedule: spinning with local work")
	}

	checkTimers(pp, 0)

	var gp *g
	var inheritTime bool

	// Normal goroutines will check for need to wakeP in ready,
	// but GCworkers and tracereaders will not, so the check must
	// be done here instead.
	tryWakeP := false
	if trace.enabled || trace.shutdown {
		gp = traceReader()
		if gp != nil {
			casgstatus(gp, _Gwaiting, _Grunnable)
			traceGoUnpark(gp, 0)
			tryWakeP = true
		}
	}
	if gp == nil && gcBlackenEnabled != 0 {
		gp = gcController.findRunnableGCWorker(_g_.m.p.ptr())
		tryWakeP = tryWakeP || gp != nil
	}
	// 注释：每隔61次调度尝试去全局队列中获取一个G
	if gp == nil {
		// Check the global runnable queue once in a while to ensure fairness.
		// Otherwise two goroutines can completely occupy the local runqueue
		// by constantly respawning each other.
		// 注释：每隔61次调度，尝试从全局队列种获取G，避免全局队列中的g被饿死
		if _g_.m.p.ptr().schedtick%61 == 0 && sched.runqsize > 0 {
			lock(&sched.lock)
			gp = globrunqget(_g_.m.p.ptr(), 1) // 注释：从全局队列中获取一个g
			unlock(&sched.lock)
		}
	}
	// 注释：从p的本地队列里获取G
	if gp == nil {
		// 注释：从p的本地队列中获取g(从p.runnext获取g)
		gp, inheritTime = runqget(_g_.m.p.ptr())
		// We can see gp != nil here even if the M is spinning,
		// if checkTimers added a local goroutine via goready.
	}
	// 注释：从其他地方获取G(试图从其他P中窃取(偷)，从本地或全局队列、轮询网络中获取g。)
	if gp == nil {
		// 注释：想尽办法找到可运行的G，找不到就不用返回了(调用 findrunnable找g，找不到的话就将m休眠，等待唤醒)
		// 注释：获取G；获取顺序是：先从本地P中获取-》全局队列中获取-》网络轮询，已经就绪的网络连接中获取（优化方案）-》去其他线程的本地队列里窃取（偷）
		gp, inheritTime = findrunnable() // blocks until work is available
	}

	// This thread is going to run a goroutine and is not spinning anymore,
	// so if it was marked as spinning we need to reset it now and potentially
	// start a new spinning M.
	// 注释：表示当前工作线程m正在试图从其它工作线程m的本地运行队列偷取g
	if _g_.m.spinning {
		resetspinning()
	}

	if sched.disable.user && !schedEnabled(gp) {
		// Scheduling of this goroutine is disabled. Put it on
		// the list of pending runnable goroutines for when we
		// re-enable user scheduling and look again.
		lock(&sched.lock)
		if schedEnabled(gp) {
			// Something re-enabled scheduling while we
			// were acquiring the lock.
			unlock(&sched.lock)
		} else {
			sched.disable.runnable.pushBack(gp)
			sched.disable.n++
			unlock(&sched.lock)
			goto top
		}
	}

	// If about to schedule a not-normal goroutine (a GCworker or tracereader),
	// wake a P if there is one.
	if tryWakeP {
		wakep() // 注释：尝试再添加一个P以执行G
	}
	if gp.lockedm != 0 { // 注释：g被锁定,只在这个m上运行
		// Hands off own p to the locked m,
		// then blocks waiting for a new p.
		startlockedm(gp)
		goto top
	}

	execute(gp, inheritTime) // 注释：找到了g，那就执行g上的任务函数
}

// dropg removes the association between m and the current goroutine m->curg (gp for short).
// Typically a caller sets gp's status away from Grunning and then
// immediately calls dropg to finish the job. The caller is also responsible
// for arranging that gp will be restarted using ready at an
// appropriate time. After calling dropg and arranging for gp to be
// readied later, the caller can do other work but eventually should
// call schedule to restart the scheduling of goroutines on this m.
// 注释：译：dropg删除了m和当前goroutine m->curg（简称gp）之间的关联。通常情况下，调用者将gp的状态设置为远离Grunning，然后立即调用dropg来完成作业。
//		调用方还负责安排在适当的时间使用ready重新启动gp。在调用dropg并安排稍后准备gp之后，调用者可以做其他工作，但最终应该调用schedule来重新启动该m上goroutines的调度。
// 注释：删除当前线程M的G，并把G和M的关系一并删除
// 注释：删除当前G(可能是G0、业务G)和M的联系
// 注释：(断开G和M的相互绑定关系)删除当前G
// 注释：步骤：
// 		1.获取G
// 		2.(断开G和M的关系)删除当前线程M对应G和M的关系
// 		3.(断开M和G的关系)删除当前线程M的G
func dropg() {
	_g_ := getg() // 注释：获取当前G

	setMNoWB(&_g_.m.curg.m, nil) // 注释：删除当前线程M对应G和M的关系
	setGNoWB(&_g_.m.curg, nil)   // 注释：删除当前线程M的G
}

// checkTimers runs any timers for the P that are ready.
// If now is not 0 it is the current time.
// It returns the current time or 0 if it is not known,
// and the time when the next timer should run or 0 if there is no next timer,
// and reports whether it ran any timers.
// If the time when the next timer should run is not 0,
// it is always larger than the returned time.
// We pass now in and out to avoid extra calls of nanotime.
//go:yeswritebarrierrec
func checkTimers(pp *p, now int64) (rnow, pollUntil int64, ran bool) {
	// If it's not yet time for the first timer, or the first adjusted
	// timer, then there is nothing to do.
	next := int64(atomic.Load64(&pp.timer0When))
	nextAdj := int64(atomic.Load64(&pp.timerModifiedEarliest))
	if next == 0 || (nextAdj != 0 && nextAdj < next) {
		next = nextAdj // 注释：取next和nextAdj的最小值
	}

	if next == 0 {
		// No timers to run or adjust.
		return now, 0, false
	}

	if now == 0 {
		now = nanotime()
	}
	if now < next {
		// Next timer is not ready to run, but keep going
		// if we would clear deleted timers.
		// This corresponds to the condition below where
		// we decide whether to call clearDeletedTimers.
		if pp != getg().m.p.ptr() || int(atomic.Load(&pp.deletedTimers)) <= int(atomic.Load(&pp.numTimers)/4) {
			return now, next, false
		}
	}

	lock(&pp.timersLock)

	if len(pp.timers) > 0 {
		adjusttimers(pp, now)
		for len(pp.timers) > 0 {
			// Note that runtimer may temporarily unlock
			// pp.timersLock.
			if tw := runtimer(pp, now); tw != 0 {
				if tw > 0 {
					pollUntil = tw
				}
				break
			}
			ran = true
		}
	}

	// If this is the local P, and there are a lot of deleted timers,
	// clear them out. We only do this for the local P to reduce
	// lock contention on timersLock.
	if pp == getg().m.p.ptr() && int(atomic.Load(&pp.deletedTimers)) > len(pp.timers)/4 {
		clearDeletedTimers(pp)
	}

	unlock(&pp.timersLock)

	return now, pollUntil, ran
}

func parkunlock_c(gp *g, lock unsafe.Pointer) bool {
	unlock((*mutex)(lock))
	return true
}

// park continuation on g0.
// 注释：在G0上执行，参数gp是业务G
// 注释：设置业务G的状态为等待（_Gwaiting）
func park_m(gp *g) {
	_g_ := getg()

	if trace.enabled {
		traceGoPark(_g_.m.waittraceev, _g_.m.waittraceskip)
	}

	casgstatus(gp, _Grunning, _Gwaiting) // 注释：业务G设置状态为等待（_Gwaiting）
	dropg()                              // 注释：(解除等待)删除G0和M的绑定

	// 注释：解除等待，执行钩子函数
	if fn := _g_.m.waitunlockf; fn != nil { // 注释：解除等待函数钩子，如果定义，解除等待则执行
		ok := fn(gp, _g_.m.waitlock) // 注释：执行钩子函数
		_g_.m.waitunlockf = nil      // 注释：清空钩子函数
		_g_.m.waitlock = nil         // 注释：清空钩子函数的参数
		if !ok {
			if trace.enabled {
				traceGoUnpark(gp, 2)
			}
			casgstatus(gp, _Gwaiting, _Grunnable) // 注释：如果钩子函数执行失败则把业务G状态设置为准备执行(_Grunnable)
			execute(gp, true)                     // 注释：立刻执行业务G // Schedule it back, never returns. // 注释：译：把它安排回来，永远不会回来。
		}
	}
	schedule() // 注释：执行下一轮调度
}

func goschedImpl(gp *g) {
	status := readgstatus(gp)
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}
	casgstatus(gp, _Grunning, _Grunnable)
	dropg()
	lock(&sched.lock)
	globrunqput(gp)
	unlock(&sched.lock)

	schedule()
}

// Gosched continuation on g0.
func gosched_m(gp *g) {
	if trace.enabled {
		traceGoSched()
	}
	goschedImpl(gp)
}

// goschedguarded is a forbidden-states-avoided version of gosched_m
func goschedguarded_m(gp *g) {

	if !canPreemptM(gp.m) {
		gogo(&gp.sched) // never return
	}

	if trace.enabled {
		traceGoSched()
	}
	goschedImpl(gp)
}

func gopreempt_m(gp *g) {
	if trace.enabled {
		traceGoPreempt()
	}
	goschedImpl(gp)
}

// preemptPark parks gp and puts it in _Gpreempted.
//
//go:systemstack
func preemptPark(gp *g) {
	if trace.enabled {
		traceGoPark(traceEvGoBlock, 0)
	}
	status := readgstatus(gp)
	if status&^_Gscan != _Grunning {
		dumpgstatus(gp)
		throw("bad g status")
	}
	gp.waitreason = waitReasonPreempted
	// Transition from _Grunning to _Gscan|_Gpreempted. We can't
	// be in _Grunning when we dropg because then we'd be running
	// without an M, but the moment we're in _Gpreempted,
	// something could claim this G before we've fully cleaned it
	// up. Hence, we set the scan bit to lock down further
	// transitions until we can dropg.
	casGToPreemptScan(gp, _Grunning, _Gscan|_Gpreempted)
	dropg()
	casfrom_Gscanstatus(gp, _Gscan|_Gpreempted, _Gpreempted)
	schedule()
}

// goyield is like Gosched, but it:
// - emits a GoPreempt trace event instead of a GoSched trace event
// - puts the current G on the runq of the current P instead of the globrunq
func goyield() {
	checkTimeouts()
	mcall(goyield_m)
}

func goyield_m(gp *g) {
	if trace.enabled {
		traceGoPreempt()
	}
	pp := gp.m.p.ptr()
	casgstatus(gp, _Grunning, _Grunnable)
	dropg()
	runqput(pp, gp, false)
	schedule()
}

// Finishes execution of the current goroutine.
// 注释：译：完成当前goroutine的执行
// 注释：函数退出执行goexit然后里面执行这个函数
// 注释：执行函数退出动作，并且执行下一次调度
// 注释：步骤：
//		1.用系统栈(g0)执行goexit0函数(goexit0的入参是业务G（汇编代码在tls中获取的）)(永不返回，会执行下一次调度)
func goexit1() {
	if raceenabled {
		racegoend()
	}
	if trace.enabled {
		traceGoEnd()
	}
	mcall(goexit0) // 注释：用系统栈(g0)执行goexit0函数(goexit0的入参是业务G（汇编代码在tls中获取的）)(永不返回，会执行下一次调度)
}

// goexit continuation on g0.
// 注释：协成退出时执行该函数
// 注释：步骤：
//		1.入参gp是业务G，是在mcall汇编代码中把tls里的G压入栈中，传入到这个参数里
//		2.这是业务G的状态为_Gdead
//		3.如果是系统函数调用（runtime包里的函数），则标记调用次数减1
//		4.清空业务G里的数据
//		5. (断开G和M的相互绑定关系)删除当前G
// 		6.把空闲G放到P的本地G队列里
// 		7.执行下一次系统调度
func goexit0(gp *g) {
	_g_ := getg() // 注释：获取G

	casgstatus(gp, _Grunning, _Gdead) // 注释：(原子操作)设置G的状态从_Grunning设置为_Gdead
	if isSystemGoroutine(gp, false) { // 注释：是否是系统函数调用（runtime包里的函数）
		atomic.Xadd(&sched.ngsys, -1) // 注释：标记系统函数调用的次数减1
	}
	// 注释：清空业务G里的数据
	gp.m = nil
	locked := gp.lockedm != 0
	gp.lockedm = 0
	_g_.m.lockedg = 0
	gp.preemptStop = false
	gp.paniconfault = false
	gp._defer = nil // should be true already but just in case.
	gp._panic = nil // non-nil for Goexit during panic. points at stack-allocated data.
	gp.writebuf = nil
	gp.waitreason = 0
	gp.param = nil
	gp.labels = nil
	gp.timer = nil

	if gcBlackenEnabled != 0 && gp.gcAssistBytes > 0 {
		// Flush assist credit to the global pool. This gives
		// better information to pacing if the application is
		// rapidly creating an exiting goroutines.
		assistWorkPerByte := float64frombits(atomic.Load64(&gcController.assistWorkPerByte))
		scanCredit := int64(assistWorkPerByte * float64(gp.gcAssistBytes))
		atomic.Xaddint64(&gcController.bgScanCredit, scanCredit)
		gp.gcAssistBytes = 0
	}

	dropg() // 注释：(断开G和M的相互绑定关系)删除当前G

	if GOARCH == "wasm" { // no threads yet on wasm
		gfput(_g_.m.p.ptr(), gp)
		schedule() // never returns
	}

	if _g_.m.lockedInt != 0 {
		print("invalid m->lockedInt = ", _g_.m.lockedInt, "\n")
		throw("internal lockOSThread error")
	}
	gfput(_g_.m.p.ptr(), gp) // 注释：把空闲G放到P的本地G队列里
	if locked {
		// The goroutine may have locked this thread because
		// it put it in an unusual kernel state. Kill it
		// rather than returning it to the thread pool.

		// Return to mstart, which will release the P and exit
		// the thread.
		if GOOS != "plan9" { // See golang.org/issue/22227.
			gogo(&_g_.m.g0.sched)
		} else {
			// Clear lockedExt on plan9 since we may end up re-using
			// this thread.
			_g_.m.lockedExt = 0
		}
	}
	schedule() // 注释：执行下一次系统调度
}

// save updates getg().sched to refer to pc and sp so that a following
// gogo will restore pc and sp.
//
// save must not have write barriers because invoking a write barrier
// can clobber getg().sched.
//
// 注释：保存现场，把当前G对应的PC和SP放到当前G结构体里
//go:nosplit
//go:nowritebarrierrec
func save(pc, sp uintptr) {
	_g_ := getg()

	_g_.sched.pc = pc // 注释：保存伪PC寄存器(硬件IP指令计数器寄存器)
	_g_.sched.sp = sp // 注释：保存伪SP寄存器(硬件BP栈基地址寄存器)
	_g_.sched.lr = 0
	_g_.sched.ret = 0
	_g_.sched.g = guintptr(unsafe.Pointer(_g_)) // 注释：保存现场是所在的G
	// We need to ensure ctxt is zero, but can't have a write
	// barrier here. However, it should always already be zero.
	// Assert that.
	if _g_.sched.ctxt != nil {
		badctxt()
	}
}

// The goroutine g is about to enter a system call. 			// 注释：goroutine g即将进入系统调用。
// Record that it's not using the cpu anymore.					// 注释：记录它不再使用cpu。
// This is called only from the go syscall library and cgocall, // 注释：这只能从go系统调用库和cgocall调用，
// not from the low-level system calls used by the runtime. 	// 注释：而不是来自运行时使用的低级系统调用。
//
// Entersyscall cannot split the stack: the gosave must
// make g->sched refer to the caller's stack segment, because
// entersyscall is going to return immediately after.
// 注释：Entersyscall无法拆分堆栈：gosave必须使g->sched引用调用方的堆栈段，因为Entersyscall将在之后立即返回。
//
// Nothing entersyscall calls can split the stack either. // 注释：entersyscall调用也不能拆分堆栈。
// We cannot safely move the stack during an active call to syscall, // 注释：在对系统调用的活动调用期间，我们无法安全地移动堆栈，
// because we do not know which of the uintptr arguments are
// really pointers (back into the stack). // 注释：因为我们不知道哪个uintptr参数是真正的指针（返回堆栈）。
// In practice, this means that we make the fast path run through
// entersyscall doing no-split things, and the slow path has to use systemstack
// to run bigger things on the system stack.
// 注释：在实践中，这意味着我们让快速路径通过entersyscall运行，而不进行拆分，而慢速路径必须使用systemstack在系统堆栈上运行更大的东西。
//
// reentersyscall is the entry point used by cgo callbacks, where explicitly
// saved SP and PC are restored. This is needed when exitsyscall will be called
// from a function further up in the call stack than the parent, as g->syscallsp
// must always point to a valid stack frame. entersyscall below is the normal
// entry point for syscalls, which obtains the SP and PC from the caller.
// 注释：returnersyscall是cgo回调使用的入口点，显式保存的SP和PC将在这里恢复。当exitsyscall将从调用堆栈中比父函数更靠上的函数调用时，
// 注释：需要这样做，因为g->syscallsp必须始终指向有效的堆栈帧。下面的entersyscall是syscalls的正常入口点，它从调用方获取SP和PC。
//
// Syscall tracing:
// At the start of a syscall we emit traceGoSysCall to capture the stack trace.
// If the syscall does not block, that is it, we do not emit any other events.
// If the syscall blocks (that is, P is retaken), retaker emits traceGoSysBlock;
// when syscall returns we emit traceGoSysExit and when the goroutine starts running
// (potentially instantly, if exitsyscallfast returns true) we emit traceGoStart.
// To ensure that traceGoSysExit is emitted strictly after traceGoSysBlock,
// we remember current value of syscalltick in m (_g_.m.syscalltick = _g_.m.p.ptr().syscalltick),
// whoever emits traceGoSysBlock increments p.syscalltick afterwards;
// and we wait for the increment before emitting traceGoSysExit.
// Note that the increment is done even if tracing is not enabled,
// because tracing can be enabled in the middle of syscall. We don't want the wait to hang.
//
// 注释：系统调用的前置函数，
// 注释：主要动作：
// 注释：标记栈抢占请求； 禁止栈拆分； 保存现场（PC、SP和G）； 栈追踪； 唤醒等待的M； 安全节点检查避免数据竞争； 把当前P放到m.oldp里； 解除M和P的绑定；
// 注释：把P的状态设置成系统调用(_Psyscall)；判断是否开启GC，如果GC开启则把当前的P进入停止，如果当前的P是最后一个P时则运行GC，P的数量默认是系统核数
//go:nosplit
func reentersyscall(pc, sp uintptr) {
	_g_ := getg() // 注释：获取G，在TLS中获取G指针

	// Disable preemption because during this function g is in Gsyscall status,
	// but can have inconsistent g->sched, do not let GC observe it.
	_g_.m.locks++ // 注释：禁用抢占，因为在这个函数中，g处于Gsyscall状态，但可能有不一致的g->sched，不要让GC观察它。

	// Entersyscall must not call any function that might split/grow the stack.
	// (See details in comment above.)
	// Catch calls that might, by replacing the stack guard with something that
	// will trip any stack check and leaving a flag to tell newstack to die.
	// 注释：Entersyscall不能调用任何可能拆分/增长堆栈的函数。（请参阅上面评论中的详细信息。）捕获可能的调用，
	// 注释：方法是用会触发任何堆栈检查的东西替换堆栈保护，并留下一个标志来告诉newstack死亡。
	_g_.stackguard0 = stackPreempt // 注释：标记栈抢占请求
	_g_.throwsplit = true          // 注释：禁止栈拆分

	// Leave SP around for GC and traceback. // 注释：保留SP以进行GC和回溯。
	save(pc, sp)                                                      // 注释：保存现场
	_g_.syscallsp = sp                                                // 注释：设置系统调用时的SP值
	_g_.syscallpc = pc                                                // 注释：设置系统调用时的PC值
	casgstatus(_g_, _Grunning, _Gsyscall)                             // 注释：设置系统的调用的状态码
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp { // 注释：判断SP入股没有在栈地址范围内则报错
		systemstack(func() {
			print("entersyscall inconsistent ", hex(_g_.syscallsp), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscall")
		})
	}

	if trace.enabled { // 注释：如果栈追踪开启
		systemstack(traceGoSysCall) // 注释：切换到系统栈，执行栈追踪
		// systemstack itself clobbers g.sched.{pc,sp} and we might
		// need them later when the G is genuinely blocked in a
		// syscall
		save(pc, sp) // 注释：再次保存现场
	}

	if atomic.Load(&sched.sysmonwait) != 0 { // 注释：判断是否有等待的M，如果有则唤醒它
		systemstack(entersyscall_sysmon) // 注释：（系统栈运行）唤醒等待的M
		save(pc, sp)                     // 注释：重新保存现场
	}

	if _g_.m.p.ptr().runSafePointFn != 0 { // 注释：安全节点检查，以避免发生竞争
		// runSafePointFn may stack split if run on this stack
		// 注释：如果在此堆栈上运行，runSafePointFn可能会进行堆栈拆分
		systemstack(runSafePointFn) // 注释：在系统栈上运行，安全节点检查（检查数据竞争）
		save(pc, sp)                // 注释：再次保存现场
	}

	_g_.m.syscalltick = _g_.m.p.ptr().syscalltick // 注释：保存P里的系统调度计数器，P每一次系统调用加1
	_g_.sysblocktraced = true                     // 设置系统调用的，系统追踪
	pp := _g_.m.p.ptr()                           // 注释：获取当前G对应的P
	pp.m = 0                                      // 注释：（解除P和M的绑定）断开当前G对对应P和M
	_g_.m.oldp.set(pp)                            // 注释：把当前的P存放起来
	_g_.m.p = 0                                   // 注释：（解除M和P的绑定）断开当前G对应M对应P
	atomic.Store(&pp.status, _Psyscall)           // 注释：把P的状态设置成系统调用(_Psyscall)
	if sched.gcwaiting != 0 {                     // 注释：是否开启GC，如果开启，并且是等待GC的状态非0时，执行，把当前的P也进入等待节点，如果是最后一个P时（所有的P都停止了）执行GC
		systemstack(entersyscall_gcwait) // 注释：(停止当前的P)系统栈执行停止当前P
		save(pc, sp)                     // 注释：再次保存现场
	}

	_g_.m.locks--
}

// Standard syscall entry used by the go syscall library and normal cgo calls.
//
// This is exported via linkname to assembly in the syscall package.
//
// 注释：进入系统调用前执行的前置方法【系统调用的汇编方法：TEXT ·Syscall(SB)】
//
//go:nosplit
//go:linkname entersyscall
func entersyscall() {
	reentersyscall(getcallerpc(), getcallersp()) // 注释：系统调用的前置函数
}

func entersyscall_sysmon() {
	lock(&sched.lock)
	if atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
}

// 注释：(停止当前的P)系统栈执行停止当前P（STW时进入系统调用进入这个等待函数）
func entersyscall_gcwait() {
	_g_ := getg()           // 注释：获取当前的G
	_p_ := _g_.m.oldp.ptr() // 注释：获取系统调用时存放的旧的P的对象（就是执行系统调用前的P）

	lock(&sched.lock)                                                       // 注释：全局调度锁，加锁
	if sched.stopwait > 0 && atomic.Cas(&_p_.status, _Psyscall, _Pgcstop) { // 注释：把系统调用状态更改成功停止状态（GC导致的停止）
		if trace.enabled { //注释：如果开启栈追踪
			traceGoSysBlock(_p_) // 注释：系统调用停止时的栈追踪
			traceProcStop(_p_)   // 注释：(栈追踪)线程停止事件
		}
		_p_.syscalltick++                          // 注释：系统调度计数器，每一次系统调用加1
		if sched.stopwait--; sched.stopwait == 0 { // 注释：当所有的P都停止时执行(sched.stopwait默认是P的个数)
			notewakeup(&sched.stopnote) // 注释：唤醒CG的M节点
		}
	}
	unlock(&sched.lock)
}

// The same as entersyscall(), but with a hint that the syscall is blocking.
//go:nosplit
func entersyscallblock() {
	_g_ := getg()

	_g_.m.locks++ // see comment in entersyscall
	_g_.throwsplit = true
	_g_.stackguard0 = stackPreempt // see comment in entersyscall
	_g_.m.syscalltick = _g_.m.p.ptr().syscalltick
	_g_.sysblocktraced = true
	_g_.m.p.ptr().syscalltick++

	// Leave SP around for GC and traceback.
	pc := getcallerpc()
	sp := getcallersp()
	save(pc, sp)
	_g_.syscallsp = _g_.sched.sp
	_g_.syscallpc = _g_.sched.pc
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		sp1 := sp
		sp2 := _g_.sched.sp
		sp3 := _g_.syscallsp
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp1), " ", hex(sp2), " ", hex(sp3), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}
	casgstatus(_g_, _Grunning, _Gsyscall)
	if _g_.syscallsp < _g_.stack.lo || _g_.stack.hi < _g_.syscallsp {
		systemstack(func() {
			print("entersyscallblock inconsistent ", hex(sp), " ", hex(_g_.sched.sp), " ", hex(_g_.syscallsp), " [", hex(_g_.stack.lo), ",", hex(_g_.stack.hi), "]\n")
			throw("entersyscallblock")
		})
	}

	systemstack(entersyscallblock_handoff)

	// Resave for traceback during blocked call.
	save(getcallerpc(), getcallersp())

	_g_.m.locks--
}

func entersyscallblock_handoff() {
	if trace.enabled {
		traceGoSysCall()
		traceGoSysBlock(getg().m.p.ptr())
	}
	handoffp(releasep())
}

// The goroutine g exited its system call. // 注释：goroutine g退出了系统调用。
// Arrange for it to run on a cpu again. 	// 注释：安排它再次在cpu上运行。
// This is called only from the go syscall library, not
// from the low-level system calls used by the runtime.
// 注释：这只能从go系统调用库中调用，而不能从运行时使用的低级系统调用中调用。
//
// Write barriers are not allowed because our P may have been stolen. // 注释：写障碍是不允许的，因为我们的P可能被盗了。
//
// This is exported via linkname to assembly in the syscall package. // 注释：这是通过链接名导出到系统调用包中的程序集的。
//
// 注释：系统调用后置动作
//
//go:nosplit
//go:nowritebarrierrec
//go:linkname exitsyscall
func exitsyscall() {
	_g_ := getg() // 注释：获取当前G

	_g_.m.locks++                      // 注释：给M加锁 // see comment in entersyscall
	if getcallersp() > _g_.syscallsp { // 注释：判断单签SP是否是大于系统SP(如果大于说明在系统调用之后的SP，所以需要报错)
		throw("exitsyscall: syscall frame is no longer valid")
	}

	_g_.waitsince = 0          // 注释：清空阻塞的时间
	oldp := _g_.m.oldp.ptr()   // 注释：取出系统调用前的P的指针
	_g_.m.oldp = 0             // 注释：清空存放系统调用前的P的指针
	if exitsyscallfast(oldp) { // 注释：尝试执行快速系统调用后置函数
		if trace.enabled {
			if oldp != _g_.m.p.ptr() || _g_.m.syscalltick != _g_.m.p.ptr().syscalltick {
				systemstack(traceGoStart)
			}
		}
		// There's a cpu for us, so we can run.
		_g_.m.p.ptr().syscalltick++
		// We need to cas the status and scan before resuming...
		casgstatus(_g_, _Gsyscall, _Grunning)

		// Garbage collector isn't running (since we are),
		// so okay to clear syscallsp.
		_g_.syscallsp = 0
		_g_.m.locks--
		if _g_.preempt {
			// restore the preemption request in case we've cleared it in newstack
			_g_.stackguard0 = stackPreempt
		} else {
			// otherwise restore the real _StackGuard, we've spoiled it in entersyscall/entersyscallblock
			_g_.stackguard0 = _g_.stack.lo + _StackGuard
		}
		_g_.throwsplit = false

		if sched.disable.user && !schedEnabled(_g_) {
			// Scheduling of this goroutine is disabled.
			Gosched()
		}

		return
	}

	_g_.sysexitticks = 0
	if trace.enabled {
		// Wait till traceGoSysBlock event is emitted.
		// This ensures consistency of the trace (the goroutine is started after it is blocked).
		for oldp != nil && oldp.syscalltick == _g_.m.syscalltick {
			osyield()
		}
		// We can't trace syscall exit right now because we don't have a P.
		// Tracing code can invoke write barriers that cannot run without a P.
		// So instead we remember the syscall exit time and emit the event
		// in execute when we have a P.
		_g_.sysexitticks = cputicks()
	}

	_g_.m.locks--

	// Call the scheduler.
	mcall(exitsyscall0)

	// Scheduler returned, so we're allowed to run now.
	// Delete the syscallsp information that we left for
	// the garbage collector during the system call.
	// Must wait until now because until gosched returns
	// we don't know for sure that the garbage collector
	// is not running.
	_g_.syscallsp = 0
	_g_.m.p.ptr().syscalltick++
	_g_.throwsplit = false
}

// 注释：系统调用快速后置函数
//go:nosplit
func exitsyscallfast(oldp *p) bool {
	_g_ := getg() // 注释：获取当前G

	// Freezetheworld sets stopwait but does not retake P's. // 注释：Freezetheworld设置了stopwait，但没有重夺P。
	if sched.stopwait == freezeStopWait { // 注释：如果是冻结状态的直接返回false
		return false
	}

	// Try to re-acquire the last P. // 注释：尝试重新获取最后一个P。
	if oldp != nil && oldp.status == _Psyscall && atomic.Cas(&oldp.status, _Psyscall, _Pidle) { // 注释：如果成功把系统调用前的P的状态从系统调用更改为空闲状态
		// There's a cpu for us, so we can run.
		wirep(oldp) // 注释：当前线程m和p相互绑定，并且把p的状态从_Pidle设置成_Prunning
		exitsyscallfast_reacquired()
		return true
	}

	// Try to get any other idle P.
	if sched.pidle != 0 {
		var ok bool
		systemstack(func() {
			ok = exitsyscallfast_pidle()
			if ok && trace.enabled {
				if oldp != nil {
					// Wait till traceGoSysBlock event is emitted.
					// This ensures consistency of the trace (the goroutine is started after it is blocked).
					for oldp.syscalltick == _g_.m.syscalltick {
						osyield()
					}
				}
				traceGoSysExit(0)
			}
		})
		if ok {
			return true
		}
	}
	return false
}

// exitsyscallfast_reacquired is the exitsyscall path on which this G
// has successfully reacquired the P it was running on before the
// syscall.
// 注释：exitsyscallfast_reacquired是exitsyscall路径，在该路径上，该G已成功重新获取其在6系统调用之前运行的P。
//
//go:nosplit
func exitsyscallfast_reacquired() {
	_g_ := getg()
	if _g_.m.syscalltick != _g_.m.p.ptr().syscalltick {
		if trace.enabled {
			// The p was retaken and then enter into syscall again (since _g_.m.syscalltick has changed).
			// traceGoSysBlock for this syscall was already emitted,
			// but here we effectively retake the p from the new syscall running on the same p.
			systemstack(func() {
				// Denote blocking of the new syscall.
				traceGoSysBlock(_g_.m.p.ptr())
				// Denote completion of the current syscall.
				traceGoSysExit(0)
			})
		}
		_g_.m.p.ptr().syscalltick++
	}
}

func exitsyscallfast_pidle() bool {
	lock(&sched.lock)
	_p_ := pidleget()
	if _p_ != nil && atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if _p_ != nil {
		acquirep(_p_)
		return true
	}
	return false
}

// exitsyscall slow path on g0.
// Failed to acquire P, enqueue gp as runnable.
//
//go:nowritebarrierrec
func exitsyscall0(gp *g) {
	_g_ := getg()

	casgstatus(gp, _Gsyscall, _Grunnable)
	dropg()
	lock(&sched.lock)
	var _p_ *p
	if schedEnabled(_g_) {
		_p_ = pidleget()
	}
	if _p_ == nil {
		globrunqput(gp)
	} else if atomic.Load(&sched.sysmonwait) != 0 {
		atomic.Store(&sched.sysmonwait, 0)
		notewakeup(&sched.sysmonnote)
	}
	unlock(&sched.lock)
	if _p_ != nil {
		acquirep(_p_)
		execute(gp, false) // Never returns.
	}
	if _g_.m.lockedg != 0 {
		// Wait until another thread schedules gp and so m again.
		stoplockedm()
		execute(gp, false) // Never returns.
	}
	stopm()
	schedule() // Never returns.
}

func beforefork() {
	gp := getg().m.curg

	// Block signals during a fork, so that the child does not run
	// a signal handler before exec if a signal is sent to the process
	// group. See issue #18600.
	gp.m.locks++
	sigsave(&gp.m.sigmask)
	sigblock(false)

	// This function is called before fork in syscall package.
	// Code between fork and exec must not allocate memory nor even try to grow stack.
	// Here we spoil g->_StackGuard to reliably detect any attempts to grow stack.
	// runtime_AfterFork will undo this in parent process, but not in child.
	gp.stackguard0 = stackFork
}

// Called from syscall package before fork.
//go:linkname syscall_runtime_BeforeFork syscall.runtime_BeforeFork
//go:nosplit
func syscall_runtime_BeforeFork() {
	systemstack(beforefork)
}

func afterfork() {
	gp := getg().m.curg

	// See the comments in beforefork.
	gp.stackguard0 = gp.stack.lo + _StackGuard

	msigrestore(gp.m.sigmask)

	gp.m.locks--
}

// Called from syscall package after fork in parent.
//go:linkname syscall_runtime_AfterFork syscall.runtime_AfterFork
//go:nosplit
func syscall_runtime_AfterFork() {
	systemstack(afterfork)
}

// inForkedChild is true while manipulating signals in the child process.
// This is used to avoid calling libc functions in case we are using vfork.
var inForkedChild bool

// Called from syscall package after fork in child.
// It resets non-sigignored signals to the default handler, and
// restores the signal mask in preparation for the exec.
//
// Because this might be called during a vfork, and therefore may be
// temporarily sharing address space with the parent process, this must
// not change any global variables or calling into C code that may do so.
//
//go:linkname syscall_runtime_AfterForkInChild syscall.runtime_AfterForkInChild
//go:nosplit
//go:nowritebarrierrec
func syscall_runtime_AfterForkInChild() {
	// It's OK to change the global variable inForkedChild here
	// because we are going to change it back. There is no race here,
	// because if we are sharing address space with the parent process,
	// then the parent process can not be running concurrently.
	inForkedChild = true

	clearSignalHandlers()

	// When we are the child we are the only thread running,
	// so we know that nothing else has changed gp.m.sigmask.
	msigrestore(getg().m.sigmask)

	inForkedChild = false
}

// pendingPreemptSignals is the number of preemption signals
// that have been sent but not received. This is only used on Darwin.
// For #41702.
var pendingPreemptSignals uint32

// Called from syscall package before Exec.
//go:linkname syscall_runtime_BeforeExec syscall.runtime_BeforeExec
func syscall_runtime_BeforeExec() {
	// Prevent thread creation during exec.
	execLock.lock()

	// On Darwin, wait for all pending preemption signals to
	// be received. See issue #41702.
	if GOOS == "darwin" || GOOS == "ios" {
		for int32(atomic.Load(&pendingPreemptSignals)) > 0 {
			osyield()
		}
	}
}

// Called from syscall package after Exec.
//go:linkname syscall_runtime_AfterExec syscall.runtime_AfterExec
func syscall_runtime_AfterExec() {
	execLock.unlock()
}

// Allocate a new g, with a stack big enough for stacksize bytes.
// 注释：获取一个G，并且开辟内存空间，开辟内存空间时切换到g0系统栈上开辟的
func malg(stacksize int32) *g {
	newg := new(g) // 注释：获取一个G结构的指针
	if stacksize >= 0 {
		stacksize = round2(_StackSystem + stacksize) // 注释：二进制最小容纳_StackSystem + stacksize的值
		systemstack(func() {
			newg.stack = stackalloc(uint32(stacksize)) // 注释：创建G栈的头和尾指针
		})
		newg.stackguard0 = newg.stack.lo + _StackGuard // 注释：确定爆栈警告指针地址
		newg.stackguard1 = ^uintptr(0)                 // 注释：指针最大值
		// Clear the bottom word of the stack. We record g
		// there on gsignal stack during VDSO on ARM and ARM64.
		*(*uintptr)(unsafe.Pointer(newg.stack.lo)) = 0 // 注释：低地址存储的值清空
	}
	return newg // 注释：返回存在栈空间的G
}

// Create a new g running fn with siz bytes of arguments.
// Put it on the queue of g's waiting to run.
// The compiler turns a go statement into a call to this.
//
// The stack layout of this call is unusual: it assumes that the
// arguments to pass to fn are on the stack sequentially immediately
// after &fn. Hence, they are logically part of newproc's argument
// frame, even though they don't appear in its signature (and can't
// because their types differ between call sites).
//
// This must be nosplit because this stack layout means there are
// untypedasm_amd64.s arguments in newproc's argument frame. Stack copies won't
// be able to adjust them and stack splits won't be able to copy them.
// 注释：译：用siz字节的参数创建一个运行fn的新g。把它放在等待运行的g的队列中。编译器将go语句转换为对此的调用。
//		此调用的堆栈布局不同寻常：它假设要传递给fn的参数在堆栈上的顺序紧跟在&fn之后。因此，它们在逻辑上是newproc参数框架的一部分，
//		尽管它们没有出现在其签名中（而且不能出现，因为它们的类型在调用站点之间不同）。
//		这必须是非拆分的，因为这个堆栈布局意味着在newproc的参数框架中有untypedasm_amd64.s参数。堆栈副本将无法调整它们，堆栈拆分也无法复制它们。
// 注释：参数：fn.fn是runtime.main函数指针
// 注释：参数：siz是初始堆栈大小，一般情况下是0，(入口汇编函数（runtime·rt0_go）传入的是0，debug函数有传入参数)
// 注释：(new procedure)新建G然后把G放到当前P里(所有新建G都是从这里出去的)
// 注释：把fn组装到G里，然后放到P的G本地队列里，等待唤醒，如果是runtime.main函数则直接唤醒
// 注释：步骤：
//		1.收集参数和变量：
//			a.fn要用协成跑的方法；
//			b.argp是fn地址向上（高位）移动一个指针，这里siz是0,只有siz>0的时候argp才会被使用；
//			c.gp就是TLS本地线程存储的地址
//			d.pc是runtime·rt0_go的PC值（调用当前函数(newproc)的地址(PC)）
//		2.切换系统栈调用(用g0执行fn函数),系统栈有自己独立的栈空间，就是线程栈空间，每个M下都有个g0
//			a.实例化新的空闲G
//			b.获取当前G对应的P
//			c.把新的G加入本地P的G队列里
//			d.如果是第一个main函数（runtime.main）则直接唤醒P，执行里的G
//go:nosplit
func newproc(siz int32, fn *funcval) {
	argp := add(unsafe.Pointer(&fn), sys.PtrSize) // 注释：fn地址向上一个指针大小（就是预留fn的参数位置）向后扩大一个指针大小，存放P时使用（用fn + PtrSize 获取第一个参数的地址，也就是argp）
	gp := getg()                                  // 注释：获取当前TLS数据位置指针（用来存储G的指针的）
	pc := getcallerpc()                           // 注释：调用当前函数(newproc)的地址(PC)
	// 注释：用g0的栈创建G对象
	systemstack(func() { // 注释：切换到系统堆栈（系统堆栈指的就是g0，有独立的栈空间，就是系统线程栈空间，负责调度G）
		newg := newproc1(fn, argp, siz, gp, pc) // 注释：用g0的栈创建G对象（此时已经切换g为g0）

		_p_ := getg().m.p.ptr()  // 注释：获取当前g指向的p地址
		runqput(_p_, newg, true) // 注释：把新建立的g插入本地队列的尾部，若本地队列已满，插入全局队列

		if mainStarted { // 注释：如果是runtime.main函数启动则直接唤醒，runtime.main函数的g.goid是1
			wakep() // 注释：（启动进程）唤醒P，就是拿个M运行P里的G，如果没有则自旋
		}
	})
}

// Create a new g in state _Grunnable, starting at fn, with narg bytes
// of arguments starting at argp. callerpc is the address of the go
// statement that created this. The caller is responsible for adding
// the new g to the scheduler.
//
// This must run on the system stack because it's the continuation of
// newproc, which cannot split the stack.
// 注释：译：在状态_Grunnable中创建一个新的g，从fn开始，参数的narg字节从argp开始。callerrpc是创建它的go语句的地址。调用方负责将新g添加到调度程序中。
//		这必须在系统堆栈上运行，因为它是newproc的延续，不能拆分堆栈。
//
// 注释：参数：fn.fn初始是runtime.main函数指针，后面表示调用方的PC值（例如：方法A里使用go指令生成新的G时，参数fn.fn为A方法的地址PC值）
// 注释：参数：argp是P的指针；narg是初始堆栈大小，一般情况下是0；
// 注释：参数：narg是初始堆栈大小，一般情况下是0；
// 注释：参数：callergp是调用者的G,例如：A调用B然后执行go指令，此时callerpc是A的PC值，fn.fn是B的PC值，callergp是A对应的G
// 注释：参数：callerpc是调用者的PC指令,例如：A调用B然后执行go指令，此时callerpc是A的PC值，fn.fn是B的PC值，callergp是A对应的G
// 注释：建立一个G
// 注释：步骤：
//		1.M加锁
//		2.到本地（P里的）G队列拿出一个空闲G(此时G的状态是 _Gdead)
//			a.如果没有拿到则创建新的空G
//			b.设置G状态为 _Gdead
//			c.把G放到全局G队列里
//		3.拼装 g.sched 结构体，实际执行的就是这个结构体的G
//		4.设置G状态从 _Gdead 设置到 _Grunnable
//		5.(设置 g.goid )到P中的G的ID缓存中拿取一个G的ID，并且设置 g.goid ，设置成功后 p.goidcache++
//			a.如果缓存已经满了（p.goidcache == p.goidcacheend），则到全局中拿取16个放到缓存里
//		6.释放M锁并返回新的G
//
//go:systemstack
func newproc1(fn *funcval, argp unsafe.Pointer, narg int32, callergp *g, callerpc uintptr) *g {
	// 注释：参数:narg是初始堆栈大小，一般情况下是0；
	_g_ := getg() // 注释：获取tls(FS寄存器的值)里的G的地址

	if fn == nil {
		_g_.m.throwing = -1 // do not dump full stacks // 注释：不要转储完整的堆栈
		throw("go of nil func value")
	}
	acquirem()           // 注释：获取M并加锁，这里没有用到返回值，所以是单纯的加锁，禁止被抢占 // disable preemption because it can be holding p in a local var
	siz := narg          // 注释：初始堆栈大小，一般情况下是0；(入口汇编函数（runtime·rt0_go）传入的是0，debug函数有传入参数)
	siz = (siz + 7) &^ 7 // 注释：(永远保证是8的倍数)内存对齐，8位向上取整（最小单位是8位）

	// We could allocate a larger initial stack if necessary.
	// Not worth it: this is almost always an error.
	// 4*sizeof(uintreg): extra space added below
	// sizeof(uintreg): caller's LR (arm) or return address (x86, in gostartcall).
	if siz >= _StackMin-4*sys.RegSize-sys.RegSize {
		throw("newproc: function arguments too large for new goroutine")
	}

	_p_ := _g_.m.p.ptr() // 注释：获取当前G对应的P
	newg := gfget(_p_)   // 注释：获取一个空的G
	if newg == nil {     // 注释：如果没有取到，则创建一个
		newg = malg(_StackMin)           // 注释：给G开辟栈空间并设置栈顶和栈低
		casgstatus(newg, _Gidle, _Gdead) // 注释：设置状态为_Gdead
		allgadd(newg)                    // 注释：把G放到全局G切片里 // publishes with a g->status of Gdead so GC scanner doesn't look at uninitialized stack.
	}
	if newg.stack.hi == 0 { // 注释：如果G栈底为空时报错
		throw("newproc1: newg missing stack")
	}

	if readgstatus(newg) != _Gdead { // 注释：如果G的状态不是_Gdead报错
		throw("newproc1: new g is not Gdead")
	}

	totalSize := 4*sys.RegSize + uintptr(siz) + sys.MinFrameSize // 注释：扩展的空间(存放数据的空间，就是SP对应的空间大小) // extra space in case of reads slightly beyond frame
	totalSize += -totalSize & (sys.SpAlign - 1)                  // 注释：内存数据对齐 // align to spAlign
	sp := newg.stack.hi - totalSize                              // 注释：确定SP指针地址（存放数据的栈针基地址，用于存放变量、调用其他函数的参数和返回值）
	spArg := sp                                                  // 注释：SP指针地址（参数的开始地址）
	if usesLR {                                                  // 注释：存放LR位置（调用指令返回的PC最终位于堆栈帧之上。PC通常被称为LR）
		// caller's LR
		*(*uintptr)(unsafe.Pointer(sp)) = 0 // 注释：把LR的位置清空，后面有返回值的时候填充该位置
		prepGoExitFrame(sp)                 // 注释：AMD64架构什么都没有做，只有PPC64架构才触发汇编代码(PPC64架构就是处理LR,因为这个架构没有LR寄存器所以用R2寄存器代用）
		spArg += sys.MinFrameSize           // 注释：栈基地址参数向上移动，去掉扩展里最小尺寸
	}
	if narg > 0 { // 注释：narg是初始的参数大小，一般为0，(入口汇编函数（runtime·rt0_go）传入的是0，debug函数有传入参数)，如果大于0则需要把这部分的内存放到实际参数内存中
		memmove(unsafe.Pointer(spArg), argp, uintptr(narg)) // 注释：(复制堆栈)复制narg个字节,把argp复制到spArg里
		// This is a stack-to-stack copy. If write barriers
		// are enabled and the source stack is grey (the
		// destination is always black), then perform a
		// barrier copy. We do this *after* the memmove
		// because the destination stack may have garbage on
		// it.
		// 注释：译：这是一个堆栈到堆栈的复制。如果启用了写屏障，并且源堆栈为灰色（目标始终为黑色），则执行屏障复制。我们在memmove之后这样做，因为目标堆栈上可能有垃圾。
		if writeBarrier.needed && !_g_.m.curg.gcscandone { // 注释：需要写屏障，并且GC扫描未结束时
			f := findfunc(fn.fn) // 注释：【ing】
			stkmap := (*stackmap)(funcdata(f, _FUNCDATA_ArgsPointerMaps))
			if stkmap.nbit > 0 {
				// We're in the prologue, so it's always stack map index 0.
				bv := stackmapdata(stkmap, 0)
				bulkBarrierBitmap(spArg, spArg, uintptr(bv.n)*sys.PtrSize, 0, bv.bytedata)
			}
		}
	}

	// 注释：初始化清空sched(&newg.sched向后清空newg.sched个字节),并拼装sched结构体，实际执行的就是这个结构体的G
	memclrNoHeapPointers(unsafe.Pointer(&newg.sched), unsafe.Sizeof(newg.sched)) // 注释：(初始化清空sched)清空newg.sched内存数据
	newg.sched.sp = sp                                                           // 注释：(保存现场)保存SP寄存器地址（参数的开始地址）
	newg.stktopsp = sp                                                           // 注释：(保存现场)录栈基地址，用于追溯
	// 注释：这里太巧妙了，内存地址执行指令的顺序是高地址向低地址执行，这里初始化PC是goexit的伪PC加1个内存单位（每种平台内存单位可能不一样），后面会把这个伪PC放到伪SP，然后后面跟上fn的伪PC
	// 注释：
	//                    ********************
	//      caller --->   *       bp         *    <--- (基地址)当前函数的伪SP地址
	//                    ********************
	//                    *   return addr    *    <--- 下一个函数的返回位置(通常由LR寄存器存储)
	//                    ********************
	//      callee --->   *      fn()        *    <--- 下一个函数的PC
	//                    ********************
	// 注释：后面会把这个伪PC放到伪SP，然后后面跟上fn的伪PC就相当于：
	//        go goexit(){
	//            fn()
	//        }()
	newg.sched.pc = funcPC(goexit) + sys.PCQuantum // 注释：初始化PC指令地址空间(返回后执行goexit),新建的G执行完成后执行这里退出 // +PCQuantum so that previous instruction is in same function
	newg.sched.g = guintptr(unsafe.Pointer(newg))  // 注释：(保存现场)存当前的G地址
	gostartcallfn(&newg.sched, fn)                 // 注释：(保存现场)保存pc和ctxt(记录调用链),fn是调用方的方法指针（PC）, 例如A执行go后fn为A的PC值
	newg.gopc = callerpc                           // 注释：调用者的PC值;例如：A调用B然后执行go指令，此时callerpc是A的PC值，fn.fn是B的PC值，callergp是A对应的G
	newg.ancestors = saveAncestors(callergp)       // 注释：【ing】把当前的G的信息保存到调用链上，用于debug追溯时使用
	newg.startpc = fn.fn                           // 注释：(go fn()中fn指令对应的pc值)要调用方法的PC
	if _g_.m.curg != nil {                         // 注释：如果线程M正在运行G存在时
		newg.labels = _g_.m.curg.labels // 注释：如果线程M正在运行G存在时，同步探测器标签
	}
	if isSystemGoroutine(newg, false) { // 注释：是否是系统函数调用（runtime包里的函数）
		atomic.Xadd(&sched.ngsys, +1) // 注释：标记系统函数调用的次数
	}
	casgstatus(newg, _Gdead, _Grunnable) // 注释：设置G状态为_Grunnable

	// 注释：到P中G的ID缓存中拿一个G的ID
	if _p_.goidcache == _p_.goidcacheend { // 注释：如果缓存的开始和结束相等，表示没有缓存了，需要到全局中拿取16个ID缓存起来
		// Sched.goidgen is the last allocated id,
		// this batch must be [sched.goidgen+1, sched.goidgen+GoidCacheBatch].
		// At startup sched.goidgen=0, so main goroutine receives goid=1.
		// 注释：译：Schedule.goidgen是最后一个分配的id，此批处理必须是[Sched.goidgen+1，Sched.goidgen+GoidCacheBatch]之间。
		//		在启动时，Sched.giodgen=0，因此主goroutine接收到goid=1。
		_p_.goidcache = atomic.Xadd64(&sched.goidgen, _GoidCacheBatch) // 注释：到全局中拿16个缓存到P里
		_p_.goidcache -= _GoidCacheBatch - 1                           // 注释：设置缓存的开始位置数
		_p_.goidcacheend = _p_.goidcache + _GoidCacheBatch             // 注释：设置缓存的结束位置数（G的ID<该字段）
	}
	newg.goid = int64(_p_.goidcache) // 注释：设置G的ID
	_p_.goidcache++                  // 注释：下一个空G的ID加1
	if raceenabled {
		newg.racectx = racegostart(callerpc)
	}
	if trace.enabled {
		traceGoCreate(newg, newg.startpc)
	}
	releasem(_g_.m) // 注释：释放M锁

	return newg // 注释：返回新的G
}

// saveAncestors copies previous ancestors of the given caller g and
// includes infor for the current caller into a new set of tracebacks for
// a g being created.
func saveAncestors(callergp *g) *[]ancestorInfo {
	// Copy all prior info, except for the root goroutine (goid 0).
	if debug.tracebackancestors <= 0 || callergp.goid == 0 {
		return nil
	}
	var callerAncestors []ancestorInfo
	if callergp.ancestors != nil {
		callerAncestors = *callergp.ancestors
	}
	n := int32(len(callerAncestors)) + 1
	if n > debug.tracebackancestors {
		n = debug.tracebackancestors
	}
	ancestors := make([]ancestorInfo, n)
	copy(ancestors[1:], callerAncestors)

	var pcs [_TracebackMaxFrames]uintptr
	npcs := gcallers(callergp, 0, pcs[:])
	ipcs := make([]uintptr, npcs)
	copy(ipcs, pcs[:])
	ancestors[0] = ancestorInfo{
		pcs:  ipcs,
		goid: callergp.goid,
		gopc: callergp.gopc,
	}

	ancestorsp := new([]ancestorInfo)
	*ancestorsp = ancestors
	return ancestorsp
}

// Put on gfree list.
// If local list is too long, transfer a batch to the global list.
// 注释：译：列入gfree list里。如果本地列表太长，请将一个批转移到全局列表。
// 注释：空G放到本地P空G队列里，如果到达64个时，拿出一半放到全局空G队列里
// 注释：步骤：
//		1.获取栈大小
//		2.如果栈空间不是固定大小（不同系统固定大小不同，Linux是2048），释放栈空间，并清空栈的高地址、底地址、爆栈地址
//		3.把G放到本地队列里(P的G队列)
//		4.本地空闲G数量加1
//		5.如果本地空闲G数量>=64
//			a.(加锁)获取全局空闲G队列锁
//			b.拿出一半（32个）放到全局空闲G队列里
//			c.如果G无栈空间，放到【无栈空间】【全局空G队列】里
//			d.如果G有栈空间，放到【有栈空间】【全局空G队列】里
//			e.全局空G队列个数加1
//			f.(解锁)释放全局空闲G队列锁
func gfput(_p_ *p, gp *g) {
	if readgstatus(gp) != _Gdead {
		throw("gfput: bad status (not Gdead)")
	}

	stksize := gp.stack.hi - gp.stack.lo // 注释：G使用的堆大小

	// 注释：如果G使用的堆大小不等于固定大小时清空(非标准堆栈大小-释放它)
	if stksize != _FixedStack {
		// non-standard stack size - free it.
		stackfree(gp.stack) // 注释：释放栈空间
		gp.stack.lo = 0     // 注释：空G栈顶设置为0
		gp.stack.hi = 0     // 注释：空G栈低设置为0
		gp.stackguard0 = 0  // 注释：爆栈将设置为0
	}

	_p_.gFree.push(gp)     // 注释：把空G放到本地空队列里
	_p_.gFree.n++          // 注释：本地空队列计数加1
	if _p_.gFree.n >= 64 { // 注释：如果本地队列个数到达64个时，拿出一半放到全局空队列里
		lock(&sched.gFree.lock) // 注释：(加锁)锁定全局空G队列(获取全局空闲G队列锁)
		for _p_.gFree.n >= 32 { // 注释：拿出32个放到全局空G队列里
			_p_.gFree.n--         // 注释：本地空G个数减1
			gp = _p_.gFree.pop()  // 注释：在本地空G队列中拿出一个空G
			if gp.stack.lo == 0 { // 注释：如果G没有栈顶（没有栈空间）则放到全局空G无栈空间队列 schedt.gFree.noStack 里
				sched.gFree.noStack.push(gp) // 注释：放到全局空G无栈空间队列里
			} else { // 注释：如果G有栈顶（有栈空间）则放到全局空G有栈空间队列sched.gFree.stack里
				sched.gFree.stack.push(gp) // 注释：放到全局空G有栈空间队列里
			}
			sched.gFree.n++ // 注释：全局空G队列个数加1
		}
		unlock(&sched.gFree.lock) // 注释：(解锁)全局空G队列解锁(释放全局空闲G队列锁)
	}
}

// Get from gfree list.
// If local list is empty, grab a batch from global list.
// 注释：译：从gfree列表中获取。如果本地列表为空，请从全局列表中获取一个批。
// 注释：获取空G：从gfree列表中获取。如果本地列表为空，从全局列表中获取一批
// 注释：步骤：
//		1.如果本地P里没有空闲G && 全局里有空闲G（包括有栈空间和无栈空间）
//			a.(加锁)
//			b.从全局里拿出32个（本地空闲G队列的一半）放到本地空闲G队列里
//				(1).(全局出栈)先去全局有栈空间的空闲栈里拿，如果没有则去全局无栈空间里拿，如果还没有则直接返回
//				(2).拿完以后全局空闲G列表计数器减去1
//				(3).(本地入栈)从头部加入本地链表
//			f.(解锁)
//		2.从本地出栈一个空闲G，如果没有则直接返回
//		3.本地空闲G计数器减去1
//		4.如果没有栈空间则申请栈空间
//		5.返回空闲G
func gfget(_p_ *p) *g {
retry:
	// 注释：判断本地P空G队列是否有值，如果为空并且全局空G队里里有值时，把全局空G队列里的空G拿出一半(32个)放到本地P空G队列里，然后跳到retry处重新执行
	if _p_.gFree.empty() && (!sched.gFree.stack.empty() || !sched.gFree.noStack.empty()) {
		lock(&sched.gFree.lock) // 注释：锁定全局G链表
		// Move a batch of free Gs to the P.
		for _p_.gFree.n < 32 { // 注释：如果本地空G数量到达一半(32个)时，跳出循环，跳到retry处重新执行
			// Prefer Gs with stacks.
			gp := sched.gFree.stack.pop() // 注释：先到sched.gFree.stack里取空G
			if gp == nil {
				gp = sched.gFree.noStack.pop() // 注释：如果stack里没有取到，则到sched.gFree.noStack里取
				if gp == nil {                 // 注释：如果没有取到则跳出循环
					break
				}
			}
			sched.gFree.n--    // 注释：全局空G计数减1
			_p_.gFree.push(gp) // 注释：放到本地空队列里
			_p_.gFree.n++      // 注释：本地队列计数加1
		}
		unlock(&sched.gFree.lock) // 注释：全局空G解锁
		goto retry                // 注释：跳到retry从新执行(其实这里不用重试就可以，即便重试也无法进入本if语句中，所以无需重试)
	}
	gp := _p_.gFree.pop() // 注释：到本地P中取出一个空G，如果没有取到则退出
	if gp == nil {
		return nil
	}
	_p_.gFree.n--         // 注释：本地空G计数减1
	if gp.stack.lo == 0 { // 注释：如果空G栈空间为空时
		// Stack was deallocated in gfput. Allocate a new one.
		systemstack(func() {
			gp.stack = stackalloc(_FixedStack) // 注释：申请栈空间
		})
		gp.stackguard0 = gp.stack.lo + _StackGuard // 注释：设置爆栈警戒线
	} else {
		if raceenabled {
			racemalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
		if msanenabled {
			msanmalloc(unsafe.Pointer(gp.stack.lo), gp.stack.hi-gp.stack.lo)
		}
	}
	return gp // 注释：返回一个空G
}

// Purge all cached G's from gfree list to the global list.
// 注释：译：将所有缓存的G从gfree列表中清除到全局列表中。
// 注释：(清空本地P的空G队列)把本地P上空G放到全局空G的链表里，把有栈空间的空G放到sched.gFree.stack里，把没有栈空间的空G放到sched.gFree.noStack里
// 注释：步骤：
//		1.加锁
//		2.遍历本地空闲G列表
//			a.本地空闲G出栈
//			b.本地空闲G计数器减去1
//			c.如果无栈空间，则入栈到全局无栈空间链表里
//			b.如果有栈空间，则入栈到全局有占空间链表里
//			b.全局空闲G链表计数器加1
//		3.解锁
func gfpurge(_p_ *p) {
	lock(&sched.gFree.lock)  // 注释：全局空G链表锁：修改前上锁
	for !_p_.gFree.empty() { // 注释：如果局部空G有数据时
		gp := _p_.gFree.pop() // 注释：移除一个空G
		_p_.gFree.n--         // 注释：计数减1
		if gp.stack.lo == 0 { // 注释：判断是否有栈空间
			sched.gFree.noStack.push(gp) // 注释：如果没有栈空间则放到全局空G队列的sched.gFree.noStack里
		} else {
			sched.gFree.stack.push(gp) // 注释：如果有栈空间，则放到全局空G队列的sched.gFree.stack里
		}
		sched.gFree.n++ // 注释：全局空G链表计数加1
	}
	unlock(&sched.gFree.lock) // 注释：全局空G链表锁：解锁
}

// Breakpoint executes a breakpoint trap.
func Breakpoint() {
	breakpoint()
}

// dolockOSThread is called by LockOSThread and lockOSThread below
// after they modify m.locked. Do not allow preemption during this call,
// or else the m might be different in this function than in the caller.
//go:nosplit
func dolockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	_g_ := getg()
	_g_.m.lockedg.set(_g_)
	_g_.lockedm.set(_g_.m)
}

//go:nosplit

// LockOSThread wires the calling goroutine to its current operating system thread.
// The calling goroutine will always execute in that thread,
// and no other goroutine will execute in it,
// until the calling goroutine has made as many calls to
// UnlockOSThread as to LockOSThread.
// If the calling goroutine exits without unlocking the thread,
// the thread will be terminated.
//
// All init functions are run on the startup thread. Calling LockOSThread
// from an init function will cause the main function to be invoked on
// that thread.
//
// A goroutine should call LockOSThread before calling OS services or
// non-Go library functions that depend on per-thread state.
func LockOSThread() {
	if atomic.Load(&newmHandoff.haveTemplateThread) == 0 && GOOS != "plan9" {
		// If we need to start a new thread from the locked
		// thread, we need the template thread. Start it now
		// while we're in a known-good state.
		startTemplateThread()
	}
	_g_ := getg()
	_g_.m.lockedExt++
	if _g_.m.lockedExt == 0 {
		_g_.m.lockedExt--
		panic("LockOSThread nesting overflow")
	}
	dolockOSThread()
}

//go:nosplit
func lockOSThread() {
	getg().m.lockedInt++
	dolockOSThread()
}

// dounlockOSThread is called by UnlockOSThread and unlockOSThread below
// after they update m->locked. Do not allow preemption during this call,
// or else the m might be in different in this function than in the caller.
//go:nosplit
func dounlockOSThread() {
	if GOARCH == "wasm" {
		return // no threads on wasm yet
	}
	_g_ := getg()
	if _g_.m.lockedInt != 0 || _g_.m.lockedExt != 0 {
		return
	}
	_g_.m.lockedg = 0
	_g_.lockedm = 0
}

//go:nosplit

// UnlockOSThread undoes an earlier call to LockOSThread.
// If this drops the number of active LockOSThread calls on the
// calling goroutine to zero, it unwires the calling goroutine from
// its fixed operating system thread.
// If there are no active LockOSThread calls, this is a no-op.
//
// Before calling UnlockOSThread, the caller must ensure that the OS
// thread is suitable for running other goroutines. If the caller made
// any permanent changes to the state of the thread that would affect
// other goroutines, it should not call this function and thus leave
// the goroutine locked to the OS thread until the goroutine (and
// hence the thread) exits.
func UnlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedExt == 0 {
		return
	}
	_g_.m.lockedExt--
	dounlockOSThread()
}

//go:nosplit
func unlockOSThread() {
	_g_ := getg()
	if _g_.m.lockedInt == 0 {
		systemstack(badunlockosthread)
	}
	_g_.m.lockedInt--
	dounlockOSThread()
}

func badunlockosthread() {
	throw("runtime: internal error: misuse of lockOSThread/unlockOSThread")
}

// 注释：统计已使用G的个数
// 注释：步骤：
//		1.全局业务G个数(已使用的) = 获取全部G点个数 - 全局空闲G个数 - 系统G个数
//		2.所有业务G个数(已使用的) = 遍历所有P，减去P里空闲的G个数
//		3.如果数量小于1则设置为1，并返回
func gcount() int32 {
	// 注释：全局业务G个数(已使用的) = 获取全部G点个数 - 全局空闲G个数 - 系统G个数
	n := int32(atomic.Loaduintptr(&allglen)) - sched.gFree.n - int32(atomic.Load(&sched.ngsys))
	for _, _p_ := range allp { // 注释：遍历所有P，减去P里空闲的G个数
		n -= _p_.gFree.n // 注释：减去P里空闲的G个数
	}

	// All these variables can be changed concurrently, so the result can be inconsistent.
	// But at least the current goroutine is running.
	if n < 1 {
		n = 1
	}
	return n
}

// 注释：所有已使用的M的数量
func mcount() int32 {
	return int32(sched.mnext - sched.nmfreed) // 注释：所有已使用的M的数量 = 下一个空M的ID - 已经释放的M数量
}

var prof struct {
	signalLock uint32
	hz         int32
}

func _System()                    { _System() }
func _ExternalCode()              { _ExternalCode() }
func _LostExternalCode()          { _LostExternalCode() }
func _GC()                        { _GC() }
func _LostSIGPROFDuringAtomic64() { _LostSIGPROFDuringAtomic64() }
func _VDSO()                      { _VDSO() }

// Called if we receive a SIGPROF signal.
// Called by the signal handler, may run during STW.
//go:nowritebarrierrec
func sigprof(pc, sp, lr uintptr, gp *g, mp *m) {
	if prof.hz == 0 {
		return
	}

	// If mp.profilehz is 0, then profiling is not enabled for this thread.
	// We must check this to avoid a deadlock between setcpuprofilerate
	// and the call to cpuprof.add, below.
	if mp != nil && mp.profilehz == 0 {
		return
	}

	// On mips{,le}/arm, 64bit atomics are emulated with spinlocks, in
	// runtime/internal/atomic. If SIGPROF arrives while the program is inside
	// the critical section, it creates a deadlock (when writing the sample).
	// As a workaround, create a counter of SIGPROFs while in critical section
	// to store the count, and pass it to sigprof.add() later when SIGPROF is
	// received from somewhere else (with _LostSIGPROFDuringAtomic64 as pc).
	if GOARCH == "mips" || GOARCH == "mipsle" || GOARCH == "arm" {
		if f := findfunc(pc); f.valid() {
			if hasPrefix(funcname(f), "runtime/internal/atomic") {
				cpuprof.lostAtomic++
				return
			}
		}
		if GOARCH == "arm" && goarm < 7 && GOOS == "linux" && pc&0xffff0000 == 0xffff0000 {
			// runtime/internal/atomic functions call into kernel
			// helpers on arm < 7. See
			// runtime/internal/atomic/sys_linux_arm.s.
			cpuprof.lostAtomic++
			return
		}
	}

	// Profiling runs concurrently with GC, so it must not allocate.
	// Set a trap in case the code does allocate.
	// Note that on windows, one thread takes profiles of all the
	// other threads, so mp is usually not getg().m.
	// In fact mp may not even be stopped.
	// See golang.org/issue/17165.
	getg().m.mallocing++

	// Define that a "user g" is a user-created goroutine, and a "system g"
	// is one that is m->g0 or m->gsignal.
	//
	// We might be interrupted for profiling halfway through a
	// goroutine switch. The switch involves updating three (or four) values:
	// g, PC, SP, and (on arm) LR. The PC must be the last to be updated,
	// because once it gets updated the new g is running.
	//
	// When switching from a user g to a system g, LR is not considered live,
	// so the update only affects g, SP, and PC. Since PC must be last, there
	// the possible partial transitions in ordinary execution are (1) g alone is updated,
	// (2) both g and SP are updated, and (3) SP alone is updated.
	// If SP or g alone is updated, we can detect the partial transition by checking
	// whether the SP is within g's stack bounds. (We could also require that SP
	// be changed only after g, but the stack bounds check is needed by other
	// cases, so there is no need to impose an additional requirement.)
	//
	// There is one exceptional transition to a system g, not in ordinary execution.
	// When a signal arrives, the operating system starts the signal handler running
	// with an updated PC and SP. The g is updated last, at the beginning of the
	// handler. There are two reasons this is okay. First, until g is updated the
	// g and SP do not match, so the stack bounds check detects the partial transition.
	// Second, signal handlers currently run with signals disabled, so a profiling
	// signal cannot arrive during the handler.
	//
	// When switching from a system g to a user g, there are three possibilities.
	//
	// First, it may be that the g switch has no PC update, because the SP
	// either corresponds to a user g throughout (as in asmcgocall)
	// or because it has been arranged to look like a user g frame
	// (as in cgocallback). In this case, since the entire
	// transition is a g+SP update, a partial transition updating just one of
	// those will be detected by the stack bounds check.
	//
	// Second, when returning from a signal handler, the PC and SP updates
	// are performed by the operating system in an atomic update, so the g
	// update must be done before them. The stack bounds check detects
	// the partial transition here, and (again) signal handlers run with signals
	// disabled, so a profiling signal cannot arrive then anyway.
	//
	// Third, the common case: it may be that the switch updates g, SP, and PC
	// separately. If the PC is within any of the functions that does this,
	// we don't ask for a traceback. C.F. the function setsSP for more about this.
	//
	// There is another apparently viable approach, recorded here in case
	// the "PC within setsSP function" check turns out not to be usable.
	// It would be possible to delay the update of either g or SP until immediately
	// before the PC update instruction. Then, because of the stack bounds check,
	// the only problematic interrupt point is just before that PC update instruction,
	// and the sigprof handler can detect that instruction and simulate stepping past
	// it in order to reach a consistent state. On ARM, the update of g must be made
	// in two places (in R10 and also in a TLS slot), so the delayed update would
	// need to be the SP update. The sigprof handler must read the instruction at
	// the current PC and if it was the known instruction (for example, JMP BX or
	// MOV R2, PC), use that other register in place of the PC value.
	// The biggest drawback to this solution is that it requires that we can tell
	// whether it's safe to read from the memory pointed at by PC.
	// In a correct program, we can test PC == nil and otherwise read,
	// but if a profiling signal happens at the instant that a program executes
	// a bad jump (before the program manages to handle the resulting fault)
	// the profiling handler could fault trying to read nonexistent memory.
	//
	// To recap, there are no constraints on the assembly being used for the
	// transition. We simply require that g and SP match and that the PC is not
	// in gogo.
	traceback := true
	if gp == nil || sp < gp.stack.lo || gp.stack.hi < sp || setsSP(pc) || (mp != nil && mp.vdsoSP != 0) {
		traceback = false
	}
	var stk [maxCPUProfStack]uintptr
	n := 0
	if mp.ncgo > 0 && mp.curg != nil && mp.curg.syscallpc != 0 && mp.curg.syscallsp != 0 {
		cgoOff := 0
		// Check cgoCallersUse to make sure that we are not
		// interrupting other code that is fiddling with
		// cgoCallers.  We are running in a signal handler
		// with all signals blocked, so we don't have to worry
		// about any other code interrupting us.
		if atomic.Load(&mp.cgoCallersUse) == 0 && mp.cgoCallers != nil && mp.cgoCallers[0] != 0 {
			for cgoOff < len(mp.cgoCallers) && mp.cgoCallers[cgoOff] != 0 {
				cgoOff++
			}
			copy(stk[:], mp.cgoCallers[:cgoOff])
			mp.cgoCallers[0] = 0
		}

		// Collect Go stack that leads to the cgo call.
		n = gentraceback(mp.curg.syscallpc, mp.curg.syscallsp, 0, mp.curg, 0, &stk[cgoOff], len(stk)-cgoOff, nil, nil, 0)
		if n > 0 {
			n += cgoOff
		}
	} else if traceback {
		n = gentraceback(pc, sp, lr, gp, 0, &stk[0], len(stk), nil, nil, _TraceTrap|_TraceJumpStack)
	}

	if n <= 0 {
		// Normal traceback is impossible or has failed.
		// See if it falls into several common cases.
		n = 0
		if usesLibcall() && mp.libcallg != 0 && mp.libcallpc != 0 && mp.libcallsp != 0 {
			// Libcall, i.e. runtime syscall on windows.
			// Collect Go stack that leads to the call.
			n = gentraceback(mp.libcallpc, mp.libcallsp, 0, mp.libcallg.ptr(), 0, &stk[0], len(stk), nil, nil, 0)
		}
		if n == 0 && mp != nil && mp.vdsoSP != 0 {
			n = gentraceback(mp.vdsoPC, mp.vdsoSP, 0, gp, 0, &stk[0], len(stk), nil, nil, _TraceTrap|_TraceJumpStack)
		}
		if n == 0 {
			// If all of the above has failed, account it against abstract "System" or "GC".
			n = 2
			if inVDSOPage(pc) {
				pc = funcPC(_VDSO) + sys.PCQuantum
			} else if pc > firstmoduledata.etext {
				// "ExternalCode" is better than "etext".
				pc = funcPC(_ExternalCode) + sys.PCQuantum
			}
			stk[0] = pc
			if mp.preemptoff != "" {
				stk[1] = funcPC(_GC) + sys.PCQuantum
			} else {
				stk[1] = funcPC(_System) + sys.PCQuantum
			}
		}
	}

	if prof.hz != 0 {
		cpuprof.add(gp, stk[:n])
	}
	getg().m.mallocing--
}

// If the signal handler receives a SIGPROF signal on a non-Go thread,
// it tries to collect a traceback into sigprofCallers.
// sigprofCallersUse is set to non-zero while sigprofCallers holds a traceback.
var sigprofCallers cgoCallers
var sigprofCallersUse uint32

// sigprofNonGo is called if we receive a SIGPROF signal on a non-Go thread,
// and the signal handler collected a stack trace in sigprofCallers.
// When this is called, sigprofCallersUse will be non-zero.
// g is nil, and what we can do is very limited.
//go:nosplit
//go:nowritebarrierrec
func sigprofNonGo() {
	if prof.hz != 0 {
		n := 0
		for n < len(sigprofCallers) && sigprofCallers[n] != 0 {
			n++
		}
		cpuprof.addNonGo(sigprofCallers[:n])
	}

	atomic.Store(&sigprofCallersUse, 0)
}

// sigprofNonGoPC is called when a profiling signal arrived on a
// non-Go thread and we have a single PC value, not a stack trace.
// g is nil, and what we can do is very limited.
//go:nosplit
//go:nowritebarrierrec
func sigprofNonGoPC(pc uintptr) {
	if prof.hz != 0 {
		stk := []uintptr{
			pc,
			funcPC(_ExternalCode) + sys.PCQuantum,
		}
		cpuprof.addNonGo(stk)
	}
}

// Reports whether a function will set the SP
// to an absolute value. Important that
// we don't traceback when these are at the bottom
// of the stack since we can't be sure that we will
// find the caller.
//
// If the function is not on the bottom of the stack
// we assume that it will have set it up so that traceback will be consistent,
// either by being a traceback terminating function
// or putting one on the stack at the right offset.
func setsSP(pc uintptr) bool {
	f := findfunc(pc)
	if !f.valid() {
		// couldn't find the function for this PC,
		// so assume the worst and stop traceback
		return true
	}
	switch f.funcID {
	case funcID_gogo, funcID_systemstack, funcID_mcall, funcID_morestack:
		return true
	}
	return false
}

// setcpuprofilerate sets the CPU profiling rate to hz times per second.
// If hz <= 0, setcpuprofilerate turns off CPU profiling.
func setcpuprofilerate(hz int32) {
	// Force sane arguments.
	if hz < 0 {
		hz = 0
	}

	// Disable preemption, otherwise we can be rescheduled to another thread
	// that has profiling enabled.
	_g_ := getg()
	_g_.m.locks++

	// Stop profiler on this thread so that it is safe to lock prof.
	// if a profiling signal came in while we had prof locked,
	// it would deadlock.
	setThreadCPUProfiler(0)

	for !atomic.Cas(&prof.signalLock, 0, 1) {
		osyield()
	}
	if prof.hz != hz {
		setProcessCPUProfiler(hz)
		prof.hz = hz
	}
	atomic.Store(&prof.signalLock, 0)

	lock(&sched.lock)
	sched.profilehz = hz
	unlock(&sched.lock)

	if hz != 0 {
		setThreadCPUProfiler(hz)
	}

	_g_.m.locks--
}

// init initializes pp, which may be a freshly allocated p or a
// previously destroyed p, and transitions it to status _Pgcstop.
func (pp *p) init(id int32) {
	pp.id = id
	pp.status = _Pgcstop
	pp.sudogcache = pp.sudogbuf[:0]
	for i := range pp.deferpool {
		pp.deferpool[i] = pp.deferpoolbuf[i][:0]
	}
	pp.wbBuf.reset()
	if pp.mcache == nil {
		if id == 0 {
			if mcache0 == nil {
				throw("missing mcache?")
			}
			// Use the bootstrap mcache0. Only one P will get
			// mcache0: the one with ID 0.
			// 注释：使用引导程序mcache0。只有一个P将获得mcache0：ID为0的那个。
			pp.mcache = mcache0
		} else {
			pp.mcache = allocmcache()
		}
	}
	if raceenabled && pp.raceprocctx == 0 {
		if id == 0 {
			pp.raceprocctx = raceprocctx0
			raceprocctx0 = 0 // bootstrap
		} else {
			pp.raceprocctx = raceproccreate()
		}
	}
	lockInit(&pp.timersLock, lockRankTimers)

	// This P may get timers when it starts running. Set the mask here
	// since the P may not go through pidleget (notably P 0 on startup).
	timerpMask.set(id)
	// Similarly, we may not go through pidleget before this P starts
	// running if it is P 0 on startup.
	idlepMask.clear(id)
}

// destroy releases all of the resources associated with pp and
// transitions it to status _Pdead.
//
// sched.lock must be held and the world must be stopped.
func (pp *p) destroy() {
	assertLockHeld(&sched.lock)
	assertWorldStopped()

	// Move all runnable goroutines to the global queue
	for pp.runqhead != pp.runqtail {
		// Pop from tail of local queue
		pp.runqtail--
		gp := pp.runq[pp.runqtail%uint32(len(pp.runq))].ptr()
		// Push onto head of global queue
		globrunqputhead(gp)
	}
	if pp.runnext != 0 {
		globrunqputhead(pp.runnext.ptr())
		pp.runnext = 0
	}
	if len(pp.timers) > 0 {
		plocal := getg().m.p.ptr()
		// The world is stopped, but we acquire timersLock to
		// protect against sysmon calling timeSleepUntil.
		// This is the only case where we hold the timersLock of
		// more than one P, so there are no deadlock concerns.
		lock(&plocal.timersLock)
		lock(&pp.timersLock)
		moveTimers(plocal, pp.timers)
		pp.timers = nil
		pp.numTimers = 0
		pp.deletedTimers = 0
		atomic.Store64(&pp.timer0When, 0)
		unlock(&pp.timersLock)
		unlock(&plocal.timersLock)
	}
	// Flush p's write barrier buffer.
	if gcphase != _GCoff {
		wbBufFlush1(pp)
		pp.gcw.dispose()
	}
	for i := range pp.sudogbuf {
		pp.sudogbuf[i] = nil
	}
	pp.sudogcache = pp.sudogbuf[:0]
	for i := range pp.deferpool {
		for j := range pp.deferpoolbuf[i] {
			pp.deferpoolbuf[i][j] = nil
		}
		pp.deferpool[i] = pp.deferpoolbuf[i][:0]
	}
	systemstack(func() {
		for i := 0; i < pp.mspancache.len; i++ {
			// Safe to call since the world is stopped.
			mheap_.spanalloc.free(unsafe.Pointer(pp.mspancache.buf[i]))
		}
		pp.mspancache.len = 0
		lock(&mheap_.lock)
		pp.pcache.flush(&mheap_.pages)
		unlock(&mheap_.lock)
	})
	freemcache(pp.mcache)
	pp.mcache = nil
	gfpurge(pp)
	traceProcFree(pp)
	if raceenabled {
		if pp.timerRaceCtx != 0 {
			// The race detector code uses a callback to fetch
			// the proc context, so arrange for that callback
			// to see the right thing.
			// This hack only works because we are the only
			// thread running.
			mp := getg().m
			phold := mp.p.ptr()
			mp.p.set(pp)

			racectxend(pp.timerRaceCtx)
			pp.timerRaceCtx = 0

			mp.p.set(phold)
		}
		raceprocdestroy(pp.raceprocctx)
		pp.raceprocctx = 0
	}
	pp.gcAssistTime = 0
	pp.status = _Pdead
}

// Change number of processors.
//
// sched.lock must be held, and the world must be stopped.
//
// gcworkbufs must not be being modified by either the GC or the write barrier
// code, so the GC must not be running if the number of Ps actually changes.
//
// Returns list of Ps with local work, they need to be scheduled by the caller.
func procresize(nprocs int32) *p {
	assertLockHeld(&sched.lock)
	assertWorldStopped()

	old := gomaxprocs
	if old < 0 || nprocs <= 0 {
		throw("procresize: invalid arg")
	}
	if trace.enabled {
		traceGomaxprocs(nprocs)
	}

	// update statistics
	now := nanotime()
	if sched.procresizetime != 0 {
		sched.totaltime += int64(old) * (now - sched.procresizetime)
	}
	sched.procresizetime = now

	maskWords := (nprocs + 31) / 32

	// Grow allp if necessary.
	if nprocs > int32(len(allp)) {
		// Synchronize with retake, which could be running
		// concurrently since it doesn't run on a P.
		lock(&allpLock)
		if nprocs <= int32(cap(allp)) {
			allp = allp[:nprocs]
		} else {
			nallp := make([]*p, nprocs)
			// Copy everything up to allp's cap so we
			// never lose old allocated Ps.
			copy(nallp, allp[:cap(allp)])
			allp = nallp
		}

		if maskWords <= int32(cap(idlepMask)) {
			idlepMask = idlepMask[:maskWords]
			timerpMask = timerpMask[:maskWords]
		} else {
			nidlepMask := make([]uint32, maskWords)
			// No need to copy beyond len, old Ps are irrelevant.
			copy(nidlepMask, idlepMask)
			idlepMask = nidlepMask

			ntimerpMask := make([]uint32, maskWords)
			copy(ntimerpMask, timerpMask)
			timerpMask = ntimerpMask
		}
		unlock(&allpLock)
	}

	// initialize new P's
	for i := old; i < nprocs; i++ {
		pp := allp[i]
		if pp == nil {
			pp = new(p)
		}
		pp.init(i)
		atomicstorep(unsafe.Pointer(&allp[i]), unsafe.Pointer(pp))
	}

	_g_ := getg()
	if _g_.m.p != 0 && _g_.m.p.ptr().id < nprocs {
		// continue to use the current P
		_g_.m.p.ptr().status = _Prunning
		_g_.m.p.ptr().mcache.prepareForSweep()
	} else {
		// release the current P and acquire allp[0].
		//
		// We must do this before destroying our current P
		// because p.destroy itself has write barriers, so we
		// need to do that from a valid P.
		if _g_.m.p != 0 {
			if trace.enabled {
				// Pretend that we were descheduled
				// and then scheduled again to keep
				// the trace sane.
				traceGoSched()
				traceProcStop(_g_.m.p.ptr())
			}
			_g_.m.p.ptr().m = 0
		}
		_g_.m.p = 0
		p := allp[0]
		p.m = 0
		p.status = _Pidle
		acquirep(p)
		if trace.enabled {
			traceGoStart()
		}
	}

	// g.m.p is now set, so we no longer need mcache0 for bootstrapping.
	mcache0 = nil // 注释：g.m.p现已设置，因此我们不再需要mcache0进行引导。

	// release resources from unused P's
	for i := nprocs; i < old; i++ {
		p := allp[i]
		p.destroy()
		// can't free P itself because it can be referenced by an M in syscall
	}

	// Trim allp.
	if int32(len(allp)) != nprocs {
		lock(&allpLock)
		allp = allp[:nprocs]
		idlepMask = idlepMask[:maskWords]
		timerpMask = timerpMask[:maskWords]
		unlock(&allpLock)
	}

	var runnablePs *p
	for i := nprocs - 1; i >= 0; i-- {
		p := allp[i]
		if _g_.m.p.ptr() == p {
			continue
		}
		p.status = _Pidle
		if runqempty(p) {
			pidleput(p)
		} else {
			p.m.set(mget())
			p.link.set(runnablePs)
			runnablePs = p
		}
	}
	stealOrder.reset(uint32(nprocs))
	var int32p *int32 = &gomaxprocs // make compiler check that gomaxprocs is an int32
	atomic.Store((*uint32)(unsafe.Pointer(int32p)), uint32(nprocs))
	return runnablePs
}

// Associate p and the current m.
//
// This function is allowed to have write barriers even if the caller
// isn't because it immediately acquires _p_.
//
// 注释：(获得P)当前线程m和p相互绑定，并且把p的状态从_Pidle设置成_Prunning
//go:yeswritebarrierrec
func acquirep(_p_ *p) {
	// Do the part that isn't allowed to have write barriers.
	wirep(_p_) // 注释：当前线程m和p相互绑定，并且把p的状态从_Pidle设置成_Prunning

	// Have p; write barriers now allowed.

	// Perform deferred mcache flush before this P can allocate
	// from a potentially stale mcache.
	_p_.mcache.prepareForSweep()

	if trace.enabled {
		traceProcStart()
	}
}

// wirep is the first step of acquirep, which actually associates the
// current M to _p_. This is broken out so we can disallow write
// barriers for this part, since we don't yet have a P.
//
// 注释：当前线程m和p相互绑定，并且把p的状态从_Pidle设置成_Prunning
//go:nowritebarrierrec
//go:nosplit
func wirep(_p_ *p) {
	_g_ := getg()

	// 注释：如果当前p对应的m已经有绑定其他的p时报错
	if _g_.m.p != 0 {
		throw("wirep: already in go")
	}
	// 注释：如果p绑定了其他的m，或者p的状态不是_Pidle(空闲)
	if _p_.m != 0 || _p_.status != _Pidle {
		id := int64(0)
		// 注释：如果p绑定了其他的m
		if _p_.m != 0 {
			id = _p_.m.ptr().id // 注释：获取m的id
		}
		print("wirep: p->m=", _p_.m, "(", id, ") p->status=", _p_.status, "\n")
		throw("wirep: invalid p state")
	}
	_g_.m.p.set(_p_)       // 注释：m绑定p
	_p_.m.set(_g_.m)       // 注释：p绑定m
	_p_.status = _Prunning // 注释：修改p的状态为运行中
}

// Disassociate p and the current m.
// 注释：释放P，解除P和M的绑定；解除p(当前g对应的p)和当前m的绑定,并返回p
func releasep() *p {
	_g_ := getg() // 注释：获取当前g

	// 注释：如果当前g对应p为0是报错(当前g只能通过m来获取p)
	if _g_.m.p == 0 {
		throw("releasep: invalid arg")
	}
	_p_ := _g_.m.p.ptr() // 注释：获取当前g对应的m绑定的p的指针
	// 注释：如果p和m没有相互绑定或者p的状态不是_Prunning时报错
	if _p_.m.ptr() != _g_.m || _p_.status != _Prunning {
		print("releasep: m=", _g_.m, " m->p=", _g_.m.p.ptr(), " p->m=", hex(_p_.m), " p->status=", _p_.status, "\n")
		throw("releasep: invalid p state")
	}
	// 注释：判断是否开启追踪
	if trace.enabled {
		traceProcStop(_g_.m.p.ptr())
	}
	// 注释：g和m相互解除绑定
	_g_.m.p = 0         // 注释：解除m和p的绑定
	_p_.m = 0           // 注释：解除p和m的绑定
	_p_.status = _Pidle // 注释：把当前p的状态更改为空闲_Pidle
	return _p_          // 注释：返回解除绑定后的p
}

func incidlelocked(v int32) {
	lock(&sched.lock)
	sched.nmidlelocked += v
	if v > 0 {
		checkdead()
	}
	unlock(&sched.lock)
}

// Check for deadlock situation.
// The check is based on number of running M's, if 0 -> deadlock.
// sched.lock must be held.
func checkdead() {
	assertLockHeld(&sched.lock)

	// For -buildmode=c-shared or -buildmode=c-archive it's OK if
	// there are no running goroutines. The calling program is
	// assumed to be running.
	if islibrary || isarchive {
		return
	}

	// If we are dying because of a signal caught on an already idle thread,
	// freezetheworld will cause all running threads to block.
	// And runtime will essentially enter into deadlock state,
	// except that there is a thread that will call exit soon.
	if panicking > 0 {
		return
	}

	// If we are not running under cgo, but we have an extra M then account
	// for it. (It is possible to have an extra M on Windows without cgo to
	// accommodate callbacks created by syscall.NewCallback. See issue #6751
	// for details.)
	var run0 int32
	if !iscgo && cgoHasExtraM {
		mp := lockextra(true)
		haveExtraM := extraMCount > 0
		unlockextra(mp)
		if haveExtraM {
			run0 = 1
		}
	}

	run := mcount() - sched.nmidle - sched.nmidlelocked - sched.nmsys
	if run > run0 {
		return
	}
	if run < 0 {
		print("runtime: checkdead: nmidle=", sched.nmidle, " nmidlelocked=", sched.nmidlelocked, " mcount=", mcount(), " nmsys=", sched.nmsys, "\n")
		throw("checkdead: inconsistent counts")
	}

	grunning := 0
	lock(&allglock)
	for i := 0; i < len(allgs); i++ {
		gp := allgs[i]
		if isSystemGoroutine(gp, false) {
			continue
		}
		s := readgstatus(gp)
		switch s &^ _Gscan {
		case _Gwaiting,
			_Gpreempted:
			grunning++
		case _Grunnable,
			_Grunning,
			_Gsyscall:
			print("runtime: checkdead: find g ", gp.goid, " in status ", s, "\n")
			throw("checkdead: runnable g")
		}
	}
	unlock(&allglock)
	if grunning == 0 { // possible if main goroutine calls runtime·Goexit()
		unlock(&sched.lock) // unlock so that GODEBUG=scheddetail=1 doesn't hang
		throw("no goroutines (main called runtime.Goexit) - deadlock!")
	}

	// Maybe jump time forward for playground.
	if faketime != 0 {
		when, _p_ := timeSleepUntil()
		if _p_ != nil {
			faketime = when
			for pp := &sched.pidle; *pp != 0; pp = &(*pp).ptr().link {
				if (*pp).ptr() == _p_ {
					*pp = _p_.link
					break
				}
			}
			mp := mget()
			if mp == nil {
				// There should always be a free M since
				// nothing is running.
				throw("checkdead: no m for timer")
			}
			mp.nextp.set(_p_)
			notewakeup(&mp.park)
			return
		}
	}

	// There are no goroutines running, so we can look at the P's.
	for _, _p_ := range allp {
		if len(_p_.timers) > 0 {
			return
		}
	}

	getg().m.throwing = -1 // do not dump full stacks
	unlock(&sched.lock)    // unlock so that GODEBUG=scheddetail=1 doesn't hang
	throw("all goroutines are asleep - deadlock!")
}

// forcegcperiod is the maximum time in nanoseconds between garbage
// collections. If we go this long without a garbage collection, one
// is forced to run.
//
// This is a variable for testing purposes. It normally doesn't change.
var forcegcperiod int64 = 2 * 60 * 1e9

// Always runs without a P, so write barriers are not allowed.
// 注释：译：总是在没有P的情况下运行，因此不允许出现写障碍。
//
// 注释：系统监控（system monitor）【ing】
//go:nowritebarrierrec
func sysmon() {
	lock(&sched.lock)
	sched.nmsys++
	checkdead()
	unlock(&sched.lock)

	// For syscall_runtime_doAllThreadsSyscall, sysmon is
	// sufficiently up to participate in fixups.
	atomic.Store(&sched.sysmonStarting, 0)

	lasttrace := int64(0)
	idle := 0 // how many cycles in succession we had not wokeup somebody
	delay := uint32(0)

	for {
		if idle == 0 { // start with 20us sleep...
			delay = 20
		} else if idle > 50 { // start doubling the sleep after 1ms...
			delay *= 2
		}
		if delay > 10*1000 { // up to 10ms
			delay = 10 * 1000
		}
		usleep(delay)
		mDoFixup()

		// sysmon should not enter deep sleep if schedtrace is enabled so that
		// it can print that information at the right time.
		//
		// It should also not enter deep sleep if there are any active P's so
		// that it can retake P's from syscalls, preempt long running G's, and
		// poll the network if all P's are busy for long stretches.
		//
		// It should wakeup from deep sleep if any P's become active either due
		// to exiting a syscall or waking up due to a timer expiring so that it
		// can resume performing those duties. If it wakes from a syscall it
		// resets idle and delay as a bet that since it had retaken a P from a
		// syscall before, it may need to do it again shortly after the
		// application starts work again. It does not reset idle when waking
		// from a timer to avoid adding system load to applications that spend
		// most of their time sleeping.
		now := nanotime()
		if debug.schedtrace <= 0 && (sched.gcwaiting != 0 || atomic.Load(&sched.npidle) == uint32(gomaxprocs)) {
			lock(&sched.lock)
			if atomic.Load(&sched.gcwaiting) != 0 || atomic.Load(&sched.npidle) == uint32(gomaxprocs) {
				syscallWake := false
				next, _ := timeSleepUntil()
				if next > now {
					atomic.Store(&sched.sysmonwait, 1)
					unlock(&sched.lock)
					// Make wake-up period small enough
					// for the sampling to be correct.
					sleep := forcegcperiod / 2
					if next-now < sleep {
						sleep = next - now
					}
					shouldRelax := sleep >= osRelaxMinNS
					if shouldRelax {
						osRelax(true)
					}
					syscallWake = notetsleep(&sched.sysmonnote, sleep)
					mDoFixup()
					if shouldRelax {
						osRelax(false)
					}
					lock(&sched.lock)
					atomic.Store(&sched.sysmonwait, 0)
					noteclear(&sched.sysmonnote)
				}
				if syscallWake {
					idle = 0
					delay = 20
				}
			}
			unlock(&sched.lock)
		}

		lock(&sched.sysmonlock)
		// Update now in case we blocked on sysmonnote or spent a long time
		// blocked on schedlock or sysmonlock above.
		now = nanotime()

		// trigger libc interceptors if needed
		if *cgo_yield != nil {
			asmcgocall(*cgo_yield, nil)
		}
		// poll network if not polled for more than 10ms
		lastpoll := int64(atomic.Load64(&sched.lastpoll))
		if netpollinited() && lastpoll != 0 && lastpoll+10*1000*1000 < now {
			atomic.Cas64(&sched.lastpoll, uint64(lastpoll), uint64(now))
			list := netpoll(0) // non-blocking - returns list of goroutines
			if !list.empty() {
				// Need to decrement number of idle locked M's
				// (pretending that one more is running) before injectglist.
				// Otherwise it can lead to the following situation:
				// injectglist grabs all P's but before it starts M's to run the P's,
				// another M returns from syscall, finishes running its G,
				// observes that there is no work to do and no other running M's
				// and reports deadlock.
				incidlelocked(-1)
				injectglist(&list)
				incidlelocked(1)
			}
		}
		mDoFixup()
		if GOOS == "netbsd" {
			// netpoll is responsible for waiting for timer
			// expiration, so we typically don't have to worry
			// about starting an M to service timers. (Note that
			// sleep for timeSleepUntil above simply ensures sysmon
			// starts running again when that timer expiration may
			// cause Go code to run again).
			//
			// However, netbsd has a kernel bug that sometimes
			// misses netpollBreak wake-ups, which can lead to
			// unbounded delays servicing timers. If we detect this
			// overrun, then startm to get something to handle the
			// timer.
			//
			// See issue 42515 and
			// https://gnats.netbsd.org/cgi-bin/query-pr-single.pl?number=50094.
			if next, _ := timeSleepUntil(); next < now {
				startm(nil, false)
			}
		}
		if atomic.Load(&scavenge.sysmonWake) != 0 {
			// Kick the scavenger awake if someone requested it.
			wakeScavenger()
		}
		// retake P's blocked in syscalls
		// and preempt long running G's
		if retake(now) != 0 {
			idle = 0
		} else {
			idle++
		}
		// check if we need to force a GC
		if t := (gcTrigger{kind: gcTriggerTime, now: now}); t.test() && atomic.Load(&forcegc.idle) != 0 {
			lock(&forcegc.lock)
			forcegc.idle = 0
			var list gList
			list.push(forcegc.g)
			injectglist(&list)
			unlock(&forcegc.lock)
		}
		if debug.schedtrace > 0 && lasttrace+int64(debug.schedtrace)*1000000 <= now {
			lasttrace = now
			schedtrace(debug.scheddetail > 0)
		}
		unlock(&sched.sysmonlock)
	}
}

// 注释：系统监控
type sysmontick struct {
	schedtick   uint32 // 注释：处理器P调度此时
	schedwhen   int64  // 注释：处理器P上次调度时间
	syscalltick uint32 // 注释：系统调度次数
	syscallwhen int64  // 注释：系统调度时间
}

// forcePreemptNS is the time slice given to a G before it is
// preempted.
const forcePreemptNS = 10 * 1000 * 1000 // 10ms

func retake(now int64) uint32 {
	n := 0
	// Prevent allp slice changes. This lock will be completely
	// uncontended unless we're already stopping the world.
	lock(&allpLock)
	// We can't use a range loop over allp because we may
	// temporarily drop the allpLock. Hence, we need to re-fetch
	// allp each time around the loop.
	for i := 0; i < len(allp); i++ {
		_p_ := allp[i]
		if _p_ == nil {
			// This can happen if procresize has grown
			// allp but not yet created new Ps.
			continue
		}
		pd := &_p_.sysmontick
		s := _p_.status
		sysretake := false
		if s == _Prunning || s == _Psyscall {
			// Preempt G if it's running for too long.
			t := int64(_p_.schedtick)
			if int64(pd.schedtick) != t {
				pd.schedtick = uint32(t)
				pd.schedwhen = now
			} else if pd.schedwhen+forcePreemptNS <= now {
				preemptone(_p_)
				// In case of syscall, preemptone() doesn't
				// work, because there is no M wired to P.
				sysretake = true
			}
		}
		if s == _Psyscall {
			// Retake P from syscall if it's there for more than 1 sysmon tick (at least 20us).
			t := int64(_p_.syscalltick)
			if !sysretake && int64(pd.syscalltick) != t {
				pd.syscalltick = uint32(t)
				pd.syscallwhen = now
				continue
			}
			// On the one hand we don't want to retake Ps if there is no other work to do,
			// but on the other hand we want to retake them eventually
			// because they can prevent the sysmon thread from deep sleep.
			if runqempty(_p_) && atomic.Load(&sched.nmspinning)+atomic.Load(&sched.npidle) > 0 && pd.syscallwhen+10*1000*1000 > now {
				continue
			}
			// Drop allpLock so we can take sched.lock.
			unlock(&allpLock)
			// Need to decrement number of idle locked M's
			// (pretending that one more is running) before the CAS.
			// Otherwise the M from which we retake can exit the syscall,
			// increment nmidle and report deadlock.
			incidlelocked(-1)
			if atomic.Cas(&_p_.status, s, _Pidle) {
				if trace.enabled {
					traceGoSysBlock(_p_)
					traceProcStop(_p_)
				}
				n++
				_p_.syscalltick++
				handoffp(_p_)
			}
			incidlelocked(1)
			lock(&allpLock)
		}
	}
	unlock(&allpLock)
	return uint32(n)
}

// Tell all goroutines that they have been preempted and they should stop.
// This function is purely best-effort. It can fail to inform a goroutine if a
// processor just started running it.
// No locks need to be held.
// Returns true if preemption request was issued to at least one goroutine.
// 注释：把所有全局运行中的P设置为可抢占，只要有一个成功则返回成功
func preemptall() bool {
	res := false
	for _, _p_ := range allp { // 注释：循环全局P
		if _p_.status != _Prunning { // 注释：非运行中的P跳过
			continue
		}
		if preemptone(_p_) { // 注释：设置一个P对应的M为可抢占
			res = true
		}
	}
	return res
}

// Tell the goroutine running on processor P to stop.
// This function is purely best-effort. It can incorrectly fail to inform the
// goroutine. It can send inform the wrong goroutine. Even if it informs the
// correct goroutine, that goroutine might ignore the request if it is
// simultaneously executing newstack.
// No lock needs to be held.
// Returns true if preemption request was issued.
// The actual preemption will happen at some point in the future
// and will be indicated by the gp->status no longer being
// Grunning
// 注释：设置一个P对应的M里对应的G为可抢占,或P中所有的G设置为异步可抢占
func preemptone(_p_ *p) bool {
	mp := _p_.m.ptr() // 注释：P对应M
	// 注释：如果M为空表示M已经被别的P抢占了
	// 注释：如果M等于当前的G对应的M时表示已经抢占过来了
	if mp == nil || mp == getg().m {
		return false
	}
	gp := mp.curg // 注释：获取M上要执行的G
	// 注释：如果G不存在表M上没有要执行的G
	// 注释：如果G是G0则跳过，G0不能被抢占
	if gp == nil || gp == mp.g0 {
		return false
	}

	gp.preempt = true // 注释：标记P的M可以被抢占

	// Every call in a go routine checks for stack overflow by
	// comparing the current stack pointer to gp->stackguard0.
	// Setting gp->stackguard0 to StackPreempt folds
	// preemption into the normal stack overflow check.
	gp.stackguard0 = stackPreempt // 注释：爆栈警告，标记P的M可以被抢占；意味着当前g发出了抢占请求

	// Request an async preemption of this P.
	if preemptMSupported && debug.asyncpreemptoff == 0 {
		_p_.preempt = true // 注释：把P上的抢占标记设置为True是表示P上的所有G异步可抢占
		preemptM(mp)
	}

	return true
}

var starttime int64

func schedtrace(detailed bool) {
	now := nanotime()
	if starttime == 0 {
		starttime = now
	}

	lock(&sched.lock)
	print("SCHED ", (now-starttime)/1e6, "ms: gomaxprocs=", gomaxprocs, " idleprocs=", sched.npidle, " threads=", mcount(), " spinningthreads=", sched.nmspinning, " idlethreads=", sched.nmidle, " runqueue=", sched.runqsize)
	if detailed {
		print(" gcwaiting=", sched.gcwaiting, " nmidlelocked=", sched.nmidlelocked, " stopwait=", sched.stopwait, " sysmonwait=", sched.sysmonwait, "\n")
	}
	// We must be careful while reading data from P's, M's and G's.
	// Even if we hold schedlock, most data can be changed concurrently.
	// E.g. (p->m ? p->m->id : -1) can crash if p->m changes from non-nil to nil.
	for i, _p_ := range allp {
		mp := _p_.m.ptr()
		h := atomic.Load(&_p_.runqhead)
		t := atomic.Load(&_p_.runqtail)
		if detailed {
			id := int64(-1)
			if mp != nil {
				id = mp.id
			}
			print("  P", i, ": status=", _p_.status, " schedtick=", _p_.schedtick, " syscalltick=", _p_.syscalltick, " m=", id, " runqsize=", t-h, " gfreecnt=", _p_.gFree.n, " timerslen=", len(_p_.timers), "\n")
		} else {
			// In non-detailed mode format lengths of per-P run queues as:
			// [len1 len2 len3 len4]
			print(" ")
			if i == 0 {
				print("[")
			}
			print(t - h)
			if i == len(allp)-1 {
				print("]\n")
			}
		}
	}

	if !detailed {
		unlock(&sched.lock)
		return
	}

	for mp := allm; mp != nil; mp = mp.alllink {
		_p_ := mp.p.ptr()
		gp := mp.curg
		lockedg := mp.lockedg.ptr()
		id1 := int32(-1)
		if _p_ != nil {
			id1 = _p_.id
		}
		id2 := int64(-1)
		if gp != nil {
			id2 = gp.goid
		}
		id3 := int64(-1)
		if lockedg != nil {
			id3 = lockedg.goid
		}
		print("  M", mp.id, ": p=", id1, " curg=", id2, " mallocing=", mp.mallocing, " throwing=", mp.throwing, " preemptoff=", mp.preemptoff, ""+" locks=", mp.locks, " dying=", mp.dying, " spinning=", mp.spinning, " blocked=", mp.blocked, " lockedg=", id3, "\n")
	}

	lock(&allglock)
	for gi := 0; gi < len(allgs); gi++ {
		gp := allgs[gi]
		mp := gp.m
		lockedm := gp.lockedm.ptr()
		id1 := int64(-1)
		if mp != nil {
			id1 = mp.id
		}
		id2 := int64(-1)
		if lockedm != nil {
			id2 = lockedm.id
		}
		print("  G", gp.goid, ": status=", readgstatus(gp), "(", gp.waitreason.String(), ") m=", id1, " lockedm=", id2, "\n")
	}
	unlock(&allglock)
	unlock(&sched.lock)
}

// schedEnableUser enables or disables the scheduling of user
// goroutines.
//
// This does not stop already running user goroutines, so the caller
// should first stop the world when disabling user goroutines.
func schedEnableUser(enable bool) {
	lock(&sched.lock)
	if sched.disable.user == !enable {
		unlock(&sched.lock)
		return
	}
	sched.disable.user = !enable
	if enable {
		n := sched.disable.n
		sched.disable.n = 0
		globrunqputbatch(&sched.disable.runnable, n)
		unlock(&sched.lock)
		for ; n != 0 && sched.npidle != 0; n-- {
			startm(nil, false)
		}
	} else {
		unlock(&sched.lock)
	}
}

// schedEnabled reports whether gp should be scheduled. It returns
// false is scheduling of gp is disabled.
//
// sched.lock must be held.
func schedEnabled(gp *g) bool {
	assertLockHeld(&sched.lock)

	if sched.disable.user {
		return isSystemGoroutine(gp, true)
	}
	return true
}

// Put mp on midle list.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
// 注释：把M加入到空闲M链表中
//go:nowritebarrierrec
func mput(mp *m) {
	assertLockHeld(&sched.lock)

	// 注释：把M加入空闲链表中
	mp.schedlink = sched.midle // 注释：把M加入空闲链表中,把旧链表的头放在mp.schedlink中,形成新的链表头(此时未和链表建立连接)
	sched.midle.set(mp)        // 注释：把M加入空闲链表中,把新的链表头放在链表头位置
	sched.nmidle++             // 注释：空闲M的数量加一
	checkdead()
}

// Try to get an m from midle list.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
// 注释：获取空闲的m
//go:nowritebarrierrec
func mget() *m {
	assertLockHeld(&sched.lock)

	mp := sched.midle.ptr() // 注释：到调度器中的空闲m链表中获取一个m
	if mp != nil {          // 注释：如果拿到空闲的m
		sched.midle = mp.schedlink // 注释：重置链表头，链表头向后移动（拿出链表的头）
		sched.nmidle--             // 注释：空闲的m个数减一
	}
	return mp
}

// Put gp on the global runnable queue.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func globrunqput(gp *g) {
	assertLockHeld(&sched.lock)

	sched.runq.pushBack(gp)
	sched.runqsize++
}

// Put gp at the head of the global runnable queue.
// sched.lock must be held.
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func globrunqputhead(gp *g) {
	assertLockHeld(&sched.lock)

	sched.runq.push(gp)
	sched.runqsize++
}

// Put a batch of runnable goroutines on the global runnable queue.
// This clears *batch.
// sched.lock must be held.
// 注释：把新链表（batch）加入到全局链表中，并设置全局链表的元素数量；n代表新链表的个数
func globrunqputbatch(batch *gQueue, n int32) {
	assertLockHeld(&sched.lock)

	sched.runq.pushBackAll(*batch) // 注释：把新链表加入到全局链表中
	sched.runqsize += n            // 注释：把新链表的数量加到全局链表的数量里
	*batch = gQueue{}              // 注释：清空新的链表
}

// Try get a batch of G's from the global runnable queue.
// sched.lock must be held.
// 注释：从全局队列中获取G；返回取出的头指针；max代表指定从全局队列中那的最多G的个数，0代表不设置
func globrunqget(_p_ *p, max int32) *g {
	assertLockHeld(&sched.lock)

	if sched.runqsize == 0 {
		return nil
	}

	// 注释：n代表从全局队列中拿去多少个G；全局G平均每个核数的数量
	n := sched.runqsize/gomaxprocs + 1 // 注释：计算单个核应该拿到的G的数量
	if n > sched.runqsize {            // 注释：如果拿出的数量超过全局P队列的G的数据时，设置要拿的G的数据为全局P的数量
		n = sched.runqsize
	}
	if max > 0 && n > max { // 注释：如果传入的自定义拿去的最大数量，如果全局P的G数据大于该设置时，设置拿去的数量为自定义的数量
		n = max
	}
	// 注释：如果n大于本地队列容量的一半的时候
	if n > int32(len(_p_.runq))/2 {
		n = int32(len(_p_.runq)) / 2 // 注释：设置拿走G的数量为本地队列一半
	}

	sched.runqsize -= n // 注释：(设置全局P队列数量为拿走之后的数据)全局队列个数减少n

	gp := sched.runq.pop() // 注释：全局G队列出栈1个(取出第一个G，后面有返回该数据)（准备执行，其余的放到本地队列里面）
	n--
	for ; n > 0; n-- { // 注释：剩下的放到本地P队列里
		gp1 := sched.runq.pop()  // 注释：全局G队列循环出栈
		runqput(_p_, gp1, false) // 注释：把其余的G放到本地P队列中
	}
	// 注释：拿走第一个G，其余的G放到本地P队列中
	return gp
}

// pMask is an atomic bitstring with one bit per P.
type pMask []uint32

// read returns true if P id's bit is set.
func (p pMask) read(id uint32) bool {
	word := id / 32
	mask := uint32(1) << (id % 32)
	return (atomic.Load(&p[word]) & mask) != 0
}

// 注释：p[]数组，每个元素里有32个标识位(每32为一段，分别存的是p的自增ID)
// set sets P id's bit.
func (p pMask) set(id int32) {
	word := id / 32                // 注释：高位，确定在哪个数组元素里
	mask := uint32(1) << (id % 32) // 注释：低位，在数组元素里确定位置（共32个位置）
	atomic.Or(&p[word], mask)      // 注释：原子操作：按位或运算(把id所在元素对应的位置设置为1)
}

// 注释：p[]数组，每个元素里有32个标识位(每32为一段，分别存的是p的自增ID)
// clear clears P id's bit.
func (p pMask) clear(id int32) {
	word := id / 32                // 注释：高位，确定在哪个数组元素里
	mask := uint32(1) << (id % 32) // 注释：低位，在数组元素里确定位置（共32个位置）
	atomic.And(&p[word], ^mask)    // 注释：原子操作，清空位运算(把id所在的元素对应的位置清空)
}

// updateTimerPMask clears pp's timer mask if it has no timers on its heap.
//
// Ideally, the timer mask would be kept immediately consistent on any timer
// operations. Unfortunately, updating a shared global data structure in the
// timer hot path adds too much overhead in applications frequently switching
// between no timers and some timers.
//
// As a compromise, the timer mask is updated only on pidleget / pidleput. A
// running P (returned by pidleget) may add a timer at any time, so its mask
// must be set. An idle P (passed to pidleput) cannot add new timers while
// idle, so if it has no timers at that time, its mask may be cleared.
//
// Thus, we get the following effects on timer-stealing in findrunnable:
//
// * Idle Ps with no timers when they go idle are never checked in findrunnable
//   (for work- or timer-stealing; this is the ideal case).
// * Running Ps must always be checked.
// * Idle Ps whose timers are stolen must continue to be checked until they run
//   again, even after timer expiration.
//
// When the P starts running again, the mask should be set, as a timer may be
// added at any time.
//
// TODO(prattmic): Additional targeted updates may improve the above cases.
// e.g., updating the mask when stealing a timer.
func updateTimerPMask(pp *p) {
	if atomic.Load(&pp.numTimers) > 0 {
		return
	}

	// Looks like there are no timers, however another P may transiently
	// decrement numTimers when handling a timerModified timer in
	// checkTimers. We must take timersLock to serialize with these changes.
	lock(&pp.timersLock)
	if atomic.Load(&pp.numTimers) == 0 {
		timerpMask.clear(pp.id)
	}
	unlock(&pp.timersLock)
}

// pidleput puts p to on the _Pidle list.
//
// This releases ownership of p. Once sched.lock is released it is no longer
// safe to use p.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
//go:nowritebarrierrec
func pidleput(_p_ *p) {
	assertLockHeld(&sched.lock)

	if !runqempty(_p_) {
		throw("pidleput: P has non-empty run queue")
	}
	updateTimerPMask(_p_)         // clear if there are no timers. // 注释：把p的id从定时器掩码中移除
	idlepMask.set(_p_.id)         // 注释：设置空闲p的掩码(空闲的标记)，把p的id放在空闲p里
	_p_.link = sched.pidle        // 注释：在链表的头部压入一个
	sched.pidle.set(_p_)          // 注释：设置链表头部（把刚刚压入的那个链接上）
	atomic.Xadd(&sched.npidle, 1) // TODO: fast atomic // 注释：原子操作，空闲p计数加一
}

// pidleget tries to get a p from the _Pidle list, acquiring ownership.
//
// sched.lock must be held.
//
// May run during STW, so write barriers are not allowed.
// 注释：从空闲p里获取p
//go:nowritebarrierrec
func pidleget() *p {
	assertLockHeld(&sched.lock)

	_p_ := sched.pidle.ptr() // 注释：从调度器里的空p链表里获取一个p
	if _p_ != nil {
		// Timer may get added at any time now.
		timerpMask.set(_p_.id)         // 注释：相当于索引，启用的标记。设置启用的p的自增ID
		idlepMask.clear(_p_.id)        // 注释：相当于索引，空闲的标记。设置移除启用的p的自增ID
		sched.pidle = _p_.link         // 注释：链表移除一个，设置链表的头
		atomic.Xadd(&sched.npidle, -1) // TODO: fast atomic // 注释：原子操作，加法（空闲p减1）
	}
	return _p_
}

// runqempty reports whether _p_ has no Gs on its local run queue.
// It never returns true spuriously.
// 注释：本地的g运行队列为空时返回true，否则返回false
func runqempty(_p_ *p) bool {
	// Defend against a race where 1) _p_ has G1 in runqnext but runqhead == runqtail,
	// 2) runqput on _p_ kicks G1 to the runq, 3) runqget on _p_ empties runqnext.
	// Simply observing that runqhead == runqtail and then observing that runqnext == nil
	// does not mean the queue is empty.
	for {
		head := atomic.Load(&_p_.runqhead)
		tail := atomic.Load(&_p_.runqtail)
		runnext := atomic.Loaduintptr((*uintptr)(unsafe.Pointer(&_p_.runnext)))
		if tail == atomic.Load(&_p_.runqtail) {
			return head == tail && runnext == 0
		}
	}
}

// To shake out latent assumptions about scheduling order,
// we introduce some randomness into scheduling decisions
// when running with the race detector.
// The need for this was made obvious by changing the
// (deterministic) scheduling order in Go 1.5 and breaking
// many poorly-written tests.
// With the randomness here, as long as the tests pass
// consistently with -race, they shouldn't have latent scheduling
// assumptions.
const randomizeScheduler = raceenabled // 注释：随机打乱P队列的G位置

// runqput tries to put g on the local runnable queue.
// 注释：runqput尝试将g放入本地可运行队列
// If next is false, runqput adds g to the tail of the runnable queue.
// 注释：如果next为false，则runqput将g添加到可运行队列的尾部
// If next is true, runqput puts g in the _p_.runnext slot.
// 注释：如果next为true，runqput将g放入_p_中。运行下一个插槽
// If the run queue is full, runnext puts g on the global queue.
// 注释：如果运行队列已满，runnext会将g放入全局队列
// Executed only by the owner P.
// 注释：仅提供P的所有者执行
//
// 注释：把gp放到本地队列_p_里，并且标记是否下一就执行
// 注释：把G放到P队列里，next表示是否下一个就马上处理gp
func runqput(_p_ *p, gp *g, next bool) {
	if randomizeScheduler && next && fastrand()%2 == 0 {
		next = false
	}

	if next { // 注释：是否需要处理p.runnext字段
	retryNext:
		oldnext := _p_.runnext                                       // 注释：获取P的runnext值,旧值拿出来
		if !_p_.runnext.cas(oldnext, guintptr(unsafe.Pointer(gp))) { // 注释：把gp放到下一个要处理的位置，（交换失败重试）原子比较交换数据(如果数据被串改则重试)
			goto retryNext
		}
		if oldnext == 0 { // 注释：如果之前(旧)下一个要处理的位置为空则直接返回。（如果G被执行的时候就会直接执行这里的gp）
			return
		}
		// Kick the old runnext out to the regular run queue.
		gp = oldnext.ptr() // 注释：把之前(旧)的要处理的位置的G放到P队列尾部(新的要处理的G把旧的要处理的G挤掉，就的G会放到P队列里)
	}

retry:
	// 注释：下面操作就是G放到本地P队列中，如果本地P队列满了就会放到全局P队列里，放本地P队列成功后返回，否则会把本地P的一半放到全局队列后，再把G放到本地P中
	h := atomic.LoadAcq(&_p_.runqhead) // 注释：返回P队列头部下标（原子操作） // load-acquire, synchronize with consumers
	t := _p_.runqtail // 注释：获取P队列尾部下标
	// 注释：G的个数小于本地队列时（t-h代表本地的G个数），如果等于本地队列则需要把本地队列的G一半放到全局队列中去
	if t-h < uint32(len(_p_.runq)) { // 注释：如果G个数小于数组容量，就直接更改尾部下标对应的为gp，然后移动尾部下标指向下一个空位置（len(数组)返回数组的容量）
		_p_.runq[t%uint32(len(_p_.runq))].set(gp) // 注释：把尾部的下标位置设置为gp
		atomic.StoreRel(&_p_.runqtail, t+1)       // 注释：（尾部永远指向下一个空位置）修改值为t+1（原子操作） // store-release, makes the item available for consumption
		return
	}
	// 注释：(把G放到全局队列尾部)（把G放到本地P队列一半的后面然后一起放到全局P尾部）将G和本地P队列的一半放到全局队列中，
	if runqputslow(_p_, gp, h, t) {
		return
	}
	// the queue is not full, now the put above must succeed
	// 注释：如果把本地P队列数据放到全局队列失败的时候重试
	goto retry
}

// Put g and a batch of work from local runnable queue on global queue.
// 注释：将g和本地可运行队列中的一批工作放到全局队列中。
// Executed only by the owner P.
// 注释：把本地队列中的一半和gp一起放到全局队列尾部，gp放到队列最尾部
func runqputslow(_p_ *p, gp *g, h, t uint32) bool {
	var batch [len(_p_.runq)/2 + 1]*g // 注释：声明数组，本地队列的一半

	// First, grab a batch from local queue.
	n := t - h
	n = n / 2
	// 注释：验证判读如果运行的G的一半不等于本地队列的一半时异常
	if n != uint32(len(_p_.runq)/2) {
		throw("runqputslow: queue is not full")
	}
	for i := uint32(0); i < n; i++ {
		batch[i] = _p_.runq[(h+i)%uint32(len(_p_.runq))].ptr() // 注释：从本地队列中的头部开始获取n个数据，放在batch数组里
	}
	if !atomic.CasRel(&_p_.runqhead, h, h+n) { // cas-release, commits consume // 注释：比较替换提交消费，就是重置队列的头部，原子操作。
		return false
	}
	batch[n] = gp // 注释：把gp放在数组尾部(gp是要从本地队列迁移到全局队列的G)（这里的意思是如果把本地队列的一半放到全局队列的同时要把gp放到最后一位）

	// 注释：随机打乱P队列里G的位置（开启数据竞争检测时执行）
	if randomizeScheduler {
		for i := uint32(1); i <= n; i++ {
			j := fastrandn(i + 1)                   // 注释：随机数
			batch[i], batch[j] = batch[j], batch[i] // 注释：打乱数据位置
		}
	}

	// Link the goroutines.
	for i := uint32(0); i < n; i++ {
		batch[i].schedlink.set(batch[i+1]) // 注释：把本地队列（数组）里的G用链表结构连接起来
	}
	var q gQueue         // 注释：创建新链表（临时链表）
	q.head.set(batch[0]) // 注释：链表的头
	q.tail.set(batch[n]) // 注释：链表的尾

	// Now put the batch on global queue.
	lock(&sched.lock)                // 注释：加锁(全局调度器)
	globrunqputbatch(&q, int32(n+1)) // 注释：把链表加入到全局链表中(全局调度器)，并设置全局链表的数量
	unlock(&sched.lock)              // 注释：解锁(全局调度器)
	return true
}

// runqputbatch tries to put all the G's on q on the local runnable queue.
// If the queue is full, they are put on the global queue; in that case
// this will temporarily acquire the scheduler lock.
// Executed only by the owner P.
func runqputbatch(pp *p, q *gQueue, qsize int) {
	h := atomic.LoadAcq(&pp.runqhead)
	t := pp.runqtail
	n := uint32(0)
	for !q.empty() && t-h < uint32(len(pp.runq)) {
		gp := q.pop()
		pp.runq[t%uint32(len(pp.runq))].set(gp)
		t++
		n++
	}
	qsize -= int(n)

	if randomizeScheduler {
		off := func(o uint32) uint32 {
			return (pp.runqtail + o) % uint32(len(pp.runq))
		}
		for i := uint32(1); i < n; i++ {
			j := fastrandn(i + 1)
			pp.runq[off(i)], pp.runq[off(j)] = pp.runq[off(j)], pp.runq[off(i)]
		}
	}

	atomic.StoreRel(&pp.runqtail, t)
	if !q.empty() {
		lock(&sched.lock)
		globrunqputbatch(q, int32(qsize))
		unlock(&sched.lock)
	}
}

// Get g from local runnable queue.
// If inheritTime is true, gp should inherit the remaining time in the
// current time slice. Otherwise, it should start a new time slice.
// Executed only by the owner P.
// 注释：在本地P中获取G，gp就是本地队列的首指针（第一个G的地址）
func runqget(_p_ *p) (gp *g, inheritTime bool) {
	// If there's a runnext, it's the next G to run.
	// 注释：如果下一个要执行的G位置有数据就直接返回该G
	for {
		next := _p_.runnext // 注释：获取下一个要执行的G
		if next == 0 {      // 注释：没有下一个要执行的G时跳出循环
			break
		}
		// 注释：如果_p_指针等于next指针则把_p_设置为0,并返回next指针
		if _p_.runnext.cas(next, 0) {
			return next.ptr(), true // 注释：直接返回下一要执行的G
		}
	}

	for {
		// 注释：本地队列中的头,这里返回的时候指针(&_p_.runqhead)对应的值，（值是_p_.runq的数组下标）
		h := atomic.LoadAcq(&_p_.runqhead) // 注释：取本地队列头下标值 // load-acquire, synchronize with other consumers
		t := _p_.runqtail                  // 注释：取本地队列尾下标值
		// 注释：如果头和尾相等，则说明本地队列里没有数据了
		if t == h {
			return nil, false
		}
		// 注释：把h对应的下标赋值给gp,最终返回的是gp;"[h%uint32(len(_p_.runq))]" 是为了防止数组越界
		gp := _p_.runq[h%uint32(len(_p_.runq))].ptr() // 注释：(取出第一个)出栈
		// 注释：_p_.runqhead 加 1
		if atomic.CasRel(&_p_.runqhead, h, h+1) { // 注释：(头下标加1)更改本地队列头下标值 // cas-release, commits consume
			return gp, false
		}
	}
}

// Grabs a batch of goroutines from _p_'s runnable queue into batch.
// Batch is a ring buffer starting at batchHead.
// Returns number of grabbed goroutines.
// Can be executed by any P.
// 注释：窃取（偷），目前只有窃取函数runqsteal调用此函数。_p_代表其他线程M下P，batch是本地队列,stealRunNextG代表是否是最有一次窃取
func runqgrab(_p_ *p, batch *[256]guintptr, batchHead uint32, stealRunNextG bool) uint32 {
	for {
		h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with other consumers
		t := atomic.LoadAcq(&_p_.runqtail) // load-acquire, synchronize with the producer
		n := t - h
		n = n - n/2
		if n == 0 {
			if stealRunNextG { // 注释：判断是否是最后一次窃取
				// Try to steal from _p_.runnext.
				if next := _p_.runnext; next != 0 { // 注释：尝试窃取P2下一个要运行的G
					if _p_.status == _Prunning { // 注释：如果P2处于运行中,就等它一会
						// Sleep to ensure that _p_ isn't about to run the g
						// we are about to steal.
						// The important use case here is when the g running
						// on _p_ ready()s another g and then almost
						// immediately blocks. Instead of stealing runnext
						// in this window, back off to give _p_ a chance to
						// schedule runnext. This will avoid thrashing gs
						// between different Ps.
						// A sync chan send/recv takes ~50ns as of time of
						// writing, so 3us gives ~50x overshoot.
						if GOOS != "windows" {
							usleep(3)
						} else {
							// On windows system timer granularity is
							// 1-15ms, which is way too much for this
							// optimization. So just yield.
							osyield()
						}
					}
					// 注释：如果处于运行中并且已经等了一会后，发现还没有被运行，则强过来
					if !_p_.runnext.cas(next, 0) { // 注释：把_p_.runnext设置为0,原子操作,如果失败则跳过
						continue
					}
					batch[batchHead%uint32(len(batch))] = next // 注释：把P2下一个要运行的G抢过来
					return 1
				}
			}
			return 0
		}
		if n > uint32(len(_p_.runq)/2) { // read inconsistent h and t
			continue
		}
		for i := uint32(0); i < n; i++ {
			g := _p_.runq[(h+i)%uint32(len(_p_.runq))]  // 注释：取出G
			batch[(batchHead+i)%uint32(len(batch))] = g // 注释：把取出的G放到batch的尾部,(把偷过来的G放到本地队列P后面)
		}
		if atomic.CasRel(&_p_.runqhead, h, h+n) { // cas-release, commits consume // 注释：从新设置P2的队列头部偏移量&_p_.runqhead = h+n
			return n // 注释：返回窃取（偷）的数量
		}
	}
}

// Steal half of elements from local runnable queue of p2
// and put onto local runnable queue of p.
// Returns one of the stolen elements (or nil if failed).
// 注释：从P2中窃取（偷）一些G
func runqsteal(_p_, p2 *p, stealRunNextG bool) *g {
	t := _p_.runqtail
	n := runqgrab(p2, &_p_.runq, t, stealRunNextG) // 注释：从P2中窃取（偷）一下，如果P2中队列中没有，则尝试窃取下一个要运行的G（P2.runnext）
	if n == 0 {
		return nil
	}
	n--
	gp := _p_.runq[(t+n)%uint32(len(_p_.runq))].ptr() // 注释：取出最后一个（这时候已经窃取（偷）完并且已经放在本地队列里了）
	if n == 0 {
		return gp
	}
	h := atomic.LoadAcq(&_p_.runqhead) // load-acquire, synchronize with consumers
	if t-h+n >= uint32(len(_p_.runq)) {
		throw("runqsteal: runq overflow")
	}
	atomic.StoreRel(&_p_.runqtail, t+n) // store-release, makes the item available for consumption // 注释：&_p_.runqtail = t+n
	return gp
}

// A gQueue is a dequeue of Gs linked through g.schedlink. A G can only
// be on one gQueue or gList at a time.
type gQueue struct {
	head guintptr
	tail guintptr
}

// empty reports whether q is empty.
func (q *gQueue) empty() bool {
	return q.head == 0
}

// push adds gp to the head of q.
func (q *gQueue) push(gp *g) {
	gp.schedlink = q.head
	q.head.set(gp)
	if q.tail == 0 {
		q.tail.set(gp)
	}
}

// pushBack adds gp to the tail of q.
func (q *gQueue) pushBack(gp *g) {
	gp.schedlink = 0
	if q.tail != 0 {
		q.tail.ptr().schedlink.set(gp)
	} else {
		q.head.set(gp)
	}
	q.tail.set(gp)
}

// pushBackAll adds all Gs in l2 to the tail of q. After this q2 must
// not be used.
// 注释：把新链表加入到全局链表中
func (q *gQueue) pushBackAll(q2 gQueue) {
	if q2.tail == 0 {
		return
	}
	q2.tail.ptr().schedlink = 0
	// 注释：如果全局链表有数据
	if q.tail != 0 {
		q.tail.ptr().schedlink = q2.head // 注释：把新链表的头连接到全局链表的尾部
	} else {
		q.head = q2.head // 注释：如果全局链表没有数据时，把新链表的头放到全局链表里
	}
	q.tail = q2.tail // 注释：把新链表的尾部放到全局链表的尾部
}

// pop removes and returns the head of queue q. It returns nil if
// q is empty.
// 注释：G队列出栈(取出第一个G)，返回出栈的指针
func (q *gQueue) pop() *g {
	gp := q.head.ptr() // 注释：G队列的头指针
	if gp != nil {
		q.head = gp.schedlink // 注释：重新定义头指针（取下一个G的指针作为当前G队列的头指针）
		if q.head == 0 {      // 注释：如果头指针为0时，把尾指针也设置为0（说明G队列已经没有数据了）
			q.tail = 0
		}
	}
	return gp
}

// popList takes all Gs in q and returns them as a gList.
func (q *gQueue) popList() gList {
	stack := gList{q.head}
	*q = gQueue{}
	return stack
}

// A gList is a list of Gs linked through g.schedlink. A G can only be
// on one gQueue or gList at a time.
// 注释：G队列结构体
type gList struct {
	head guintptr // 注释：（G列表的头部G指针）head是G的指针
}

// empty reports whether l is empty.
func (l *gList) empty() bool {
	return l.head == 0
}

// push adds gp to the head of l.
func (l *gList) push(gp *g) {
	gp.schedlink = l.head
	l.head.set(gp)
}

// pushAll prepends all Gs in q to l.
func (l *gList) pushAll(q gQueue) {
	if !q.empty() {
		q.tail.ptr().schedlink = l.head
		l.head = q.head
	}
}

// pop removes and returns the head of l. If l is empty, it returns nil.
// 注释：译：pop移除并返回l的头。如果l为空，则返回nil。
// 注释：(出栈)从头部移出1个G
func (l *gList) pop() *g {
	gp := l.head.ptr() // 注释：g列表头指针
	if gp != nil {     // 注释：如果存在则，从头部移出一个G。否则直接返回nil
		l.head = gp.schedlink // 注释：从头部移出一个G
	}
	return gp
}

//go:linkname setMaxThreads runtime/debug.setMaxThreads
func setMaxThreads(in int) (out int) {
	lock(&sched.lock)
	out = int(sched.maxmcount)
	if in > 0x7fffffff { // MaxInt32
		sched.maxmcount = 0x7fffffff
	} else {
		sched.maxmcount = int32(in)
	}
	checkmcount()
	unlock(&sched.lock)
	return
}

func haveexperiment(name string) bool {
	x := sys.Goexperiment
	for x != "" {
		xname := ""
		i := bytealg.IndexByteString(x, ',')
		if i < 0 {
			xname, x = x, ""
		} else {
			xname, x = x[:i], x[i+1:]
		}
		if xname == name {
			return true
		}
		if len(xname) > 2 && xname[:2] == "no" && xname[2:] == name {
			return false
		}
	}
	return false
}

//go:nosplit
func procPin() int {
	_g_ := getg()
	mp := _g_.m

	mp.locks++
	return int(mp.p.ptr().id)
}

//go:nosplit
func procUnpin() {
	_g_ := getg()
	_g_.m.locks--
}

//go:linkname sync_runtime_procPin sync.runtime_procPin
//go:nosplit
func sync_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_runtime_procUnpin sync.runtime_procUnpin
//go:nosplit
func sync_runtime_procUnpin() {
	procUnpin()
}

//go:linkname sync_atomic_runtime_procPin sync/atomic.runtime_procPin
//go:nosplit
func sync_atomic_runtime_procPin() int {
	return procPin()
}

//go:linkname sync_atomic_runtime_procUnpin sync/atomic.runtime_procUnpin
//go:nosplit
func sync_atomic_runtime_procUnpin() {
	procUnpin()
}

// Active spinning for sync.Mutex.
//go:linkname sync_runtime_canSpin sync.runtime_canSpin
//go:nosplit
func sync_runtime_canSpin(i int) bool {
	// sync.Mutex is cooperative, so we are conservative with spinning.
	// Spin only few times and only if running on a multicore machine and
	// GOMAXPROCS>1 and there is at least one other running P and local runq is empty.
	// As opposed to runtime mutex we don't do passive spinning here,
	// because there can be work on global runq or on other Ps.
	// 注释：当前Goroutine为了获取该锁进入自旋的次数大于等于4次；
	// 注释：运行在单CPU的机器上；
	// 注释：空闲的p加自旋数加1大于处理器P的数量
	if i >= active_spin || ncpu <= 1 || gomaxprocs <= int32(sched.npidle+sched.nmspinning)+1 {
		return false
	}
	// 注释：并且处理的运行队列不为空；
	if p := getg().m.p.ptr(); !runqempty(p) {
		return false
	}
	// 注释：运行在多 CPU 的机器上；
	// 注释：当前 Goroutine 为了获取该锁进入自旋的次数小于四次；
	// 注释：当前机器上至少存在一个正在运行的处理器 P
	// 注释：并且处理的运行队列为空；
	return true
}

//go:linkname sync_runtime_doSpin sync.runtime_doSpin
//go:nosplit
func sync_runtime_doSpin() {
	procyield(active_spin_cnt) // 注释：执行30次PAUSE系统指令；TEXT runtime·procyield(SB)
}

var stealOrder randomOrder

// randomOrder/randomEnum are helper types for randomized work stealing.
// They allow to enumerate all Ps in different pseudo-random orders without repetitions.
// The algorithm is based on the fact that if we have X such that X and GOMAXPROCS
// are coprime, then a sequences of (i + X) % GOMAXPROCS gives the required enumeration.
type randomOrder struct {
	count    uint32
	coprimes []uint32
}

type randomEnum struct {
	i     uint32
	count uint32
	pos   uint32
	inc   uint32
}

func (ord *randomOrder) reset(count uint32) {
	ord.count = count
	ord.coprimes = ord.coprimes[:0]
	for i := uint32(1); i <= count; i++ {
		if gcd(i, count) == 1 {
			ord.coprimes = append(ord.coprimes, i)
		}
	}
}

func (ord *randomOrder) start(i uint32) randomEnum {
	return randomEnum{
		count: ord.count,
		pos:   i % ord.count,
		inc:   ord.coprimes[i%uint32(len(ord.coprimes))],
	}
}

func (enum *randomEnum) done() bool {
	return enum.i == enum.count
}

func (enum *randomEnum) next() {
	enum.i++
	enum.pos = (enum.pos + enum.inc) % enum.count
}

func (enum *randomEnum) position() uint32 {
	return enum.pos
}

func gcd(a, b uint32) uint32 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// An initTask represents the set of initializations that need to be done for a package.
// Keep in sync with ../../test/initempty.go:initTask
type initTask struct {
	// TODO: pack the first 3 fields more tightly?
	state uintptr // 0 = uninitialized, 1 = in progress, 2 = done
	ndeps uintptr
	nfns  uintptr
	// followed by ndeps instances of an *initTask, one per package depended on
	// followed by nfns pcs, one per init function to run
}

// inittrace stores statistics for init functions which are
// updated by malloc and newproc when active is true.
var inittrace tracestat

type tracestat struct {
	active bool   // init tracing activation status
	id     int64  // init go routine id
	allocs uint64 // heap allocations
	bytes  uint64 // heap allocated bytes
}

func doInit(t *initTask) {
	switch t.state {
	case 2: // fully initialized
		return
	case 1: // initialization in progress
		throw("recursive call during initialization - linker skew")
	default: // not initialized yet
		t.state = 1 // initialization in progress

		for i := uintptr(0); i < t.ndeps; i++ {
			p := add(unsafe.Pointer(t), (3+i)*sys.PtrSize)
			t2 := *(**initTask)(p)
			doInit(t2)
		}

		if t.nfns == 0 {
			t.state = 2 // initialization done
			return
		}

		var (
			start  int64
			before tracestat
		)

		if inittrace.active {
			start = nanotime()
			// Load stats non-atomically since tracinit is updated only by this init go routine.
			before = inittrace
		}

		firstFunc := add(unsafe.Pointer(t), (3+t.ndeps)*sys.PtrSize)
		for i := uintptr(0); i < t.nfns; i++ {
			p := add(firstFunc, i*sys.PtrSize)
			f := *(*func())(unsafe.Pointer(&p))
			f()
		}

		if inittrace.active {
			end := nanotime()
			// Load stats non-atomically since tracinit is updated only by this init go routine.
			after := inittrace

			pkg := funcpkgpath(findfunc(funcPC(firstFunc)))

			var sbuf [24]byte
			print("init ", pkg, " @")
			print(string(fmtNSAsMS(sbuf[:], uint64(start-runtimeInitTime))), " ms, ")
			print(string(fmtNSAsMS(sbuf[:], uint64(end-start))), " ms clock, ")
			print(string(itoa(sbuf[:], after.bytes-before.bytes)), " bytes, ")
			print(string(itoa(sbuf[:], after.allocs-before.allocs)), " allocs")
			print("\n")
		}

		t.state = 2 // initialization done
	}
}
