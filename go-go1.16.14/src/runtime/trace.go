// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Go execution tracer.
// The tracer captures a wide range of execution events like goroutine
// creation/blocking/unblocking, syscall enter/exit/block, GC-related events,
// changes of heap size, processor start/stop, etc and writes them to a buffer
// in a compact form. A precise nanosecond-precision timestamp and a stack
// trace is captured for most events.
// See https://golang.org/s/go15trace for more info.

package runtime

import (
	"runtime/internal/atomic"
	"runtime/internal/sys"
	"unsafe"
)

// Event types in the trace, args are given in square brackets.
// 注释：(事件类型常量)追踪的事件类型，参数在方括号中给出
const (
	traceEvNone              = 0  // unused
	traceEvBatch             = 1  // start of per-P batch of events [pid, timestamp]
	traceEvFrequency         = 2  // contains tracer timer frequency [frequency (ticks per second)]
	traceEvStack             = 3  // stack [stack id, number of PCs, array of {PC, func string ID, file string ID, line}]
	traceEvGomaxprocs        = 4  // current value of GOMAXPROCS [timestamp, GOMAXPROCS, stack id]
	traceEvProcStart         = 5  // start of P [timestamp, thread id]
	traceEvProcStop          = 6  // 注释：(栈追踪)线程停止事件 // stop of P [timestamp]
	traceEvGCStart           = 7  // GC start [timestamp, seq, stack id]
	traceEvGCDone            = 8  // GC done [timestamp]
	traceEvGCSTWStart        = 9  // GC STW start [timestamp, kind]
	traceEvGCSTWDone         = 10 // GC STW done [timestamp]
	traceEvGCSweepStart      = 11 // GC sweep start [timestamp, stack id]
	traceEvGCSweepDone       = 12 // GC sweep done [timestamp, swept, reclaimed]
	traceEvGoCreate          = 13 // goroutine creation [timestamp, new goroutine id, new stack id, stack id]
	traceEvGoStart           = 14 // goroutine starts running [timestamp, goroutine id, seq]
	traceEvGoEnd             = 15 // goroutine ends [timestamp]
	traceEvGoStop            = 16 // goroutine stops (like in select{}) [timestamp, stack]
	traceEvGoSched           = 17 // goroutine calls Gosched [timestamp, stack]
	traceEvGoPreempt         = 18 // goroutine is preempted [timestamp, stack]
	traceEvGoSleep           = 19 // goroutine calls Sleep [timestamp, stack]
	traceEvGoBlock           = 20 // goroutine blocks [timestamp, stack]
	traceEvGoUnblock         = 21 // goroutine is unblocked [timestamp, goroutine id, seq, stack]
	traceEvGoBlockSend       = 22 // goroutine blocks on chan send [timestamp, stack]
	traceEvGoBlockRecv       = 23 // goroutine blocks on chan recv [timestamp, stack]
	traceEvGoBlockSelect     = 24 // goroutine blocks on select [timestamp, stack]
	traceEvGoBlockSync       = 25 // goroutine blocks on Mutex/RWMutex [timestamp, stack]
	traceEvGoBlockCond       = 26 // goroutine blocks on Cond [timestamp, stack]
	traceEvGoBlockNet        = 27 // goroutine blocks on network [timestamp, stack]
	traceEvGoSysCall         = 28 // 注释：系统栈追踪 // syscall enter [timestamp, stack]
	traceEvGoSysExit         = 29 // syscall exit [timestamp, goroutine id, seq, real timestamp]
	traceEvGoSysBlock        = 30 // 注释：系统调用停止时的栈追踪 // syscall blocks [timestamp]
	traceEvGoWaiting         = 31 // denotes that goroutine is blocked when tracing starts [timestamp, goroutine id]
	traceEvGoInSyscall       = 32 // denotes that goroutine is in syscall when tracing starts [timestamp, goroutine id]
	traceEvHeapAlloc         = 33 // memstats.heap_live change [timestamp, heap_alloc]
	traceEvNextGC            = 34 // memstats.next_gc change [timestamp, next_gc]
	traceEvTimerGoroutine    = 35 // not currently used; previously denoted timer goroutine [timer goroutine id]
	traceEvFutileWakeup      = 36 // denotes that the previous wakeup of this goroutine was futile [timestamp]
	traceEvString            = 37 // string dictionary entry [ID, length, string]
	traceEvGoStartLocal      = 38 // goroutine starts running on the same P as the last event [timestamp, goroutine id]
	traceEvGoUnblockLocal    = 39 // goroutine is unblocked on the same P as the last event [timestamp, goroutine id, stack]
	traceEvGoSysExitLocal    = 40 // syscall exit on the same P as the last event [timestamp, goroutine id, real timestamp]
	traceEvGoStartLabel      = 41 // goroutine starts running with label [timestamp, goroutine id, seq, label string id]
	traceEvGoBlockGC         = 42 // goroutine blocks on GC assist [timestamp, stack]
	traceEvGCMarkAssistStart = 43 // GC mark assist start [timestamp, stack]
	traceEvGCMarkAssistDone  = 44 // GC mark assist done [timestamp]
	traceEvUserTaskCreate    = 45 // trace.NewContext [timestamp, internal task id, internal parent task id, stack, name string]
	traceEvUserTaskEnd       = 46 // end of a task [timestamp, internal task id, stack]
	traceEvUserRegion        = 47 // trace.WithRegion [timestamp, internal task id, mode(0:start, 1:end), stack, name string]
	traceEvUserLog           = 48 // trace.Log [timestamp, internal task id, key string id, stack, value string]
	traceEvCount             = 49
	// Byte is used but only 6 bits are available for event type.
	// The remaining 2 bits are used to specify the number of arguments.
	// That means, the max event type value is 63.
)

const (
	// Timestamps in trace are cputicks/traceTickDiv.
	// This makes absolute values of timestamp diffs smaller,
	// and so they are encoded in less number of bytes.
	// 64 on x86 is somewhat arbitrary (one tick is ~20ns on a 3GHz machine).
	// The suggested increment frequency for PowerPC's time base register is
	// 512 MHz according to Power ISA v2.07 section 6.2, so we use 16 on ppc64
	// and ppc64le.
	// Tracing won't work reliably for architectures where cputicks is emulated
	// by nanotime, so the value doesn't matter for those architectures.
	traceTickDiv = 16 + 48*(sys.Goarch386|sys.GoarchAmd64)
	// Maximum number of PCs in a single stack trace. // 注释：单个堆栈跟踪中的最大PC数。
	// Since events contain only stack id rather than whole stack trace, // 注释：由于事件只包含堆栈id而不是整个堆栈跟踪，
	// we can allow quite large values here. // 注释：我们可以在这里允许相当大的值。
	traceStackSize = 128 // 注释：栈追踪数组数据缓冲区（8位（128）个数的指针数组），用来存储栈ID的数组
	// Identifier of a fake P that is used when we trace without a real P.
	traceGlobProc = -1 // 注释：当我们在没有真实P的情况下进行跟踪时使用的伪P的标识符(占位符)。
	// Maximum number of bytes to encode uint64 in base-128.
	traceBytesPerNumber = 10
	// Shift of the number of arguments in the first event byte.
	traceArgCountShift = 6 // 注释：第一个事件字节中参数数量的偏移。
	// Flag passed to traceGoPark to denote that the previous wakeup of this
	// goroutine was futile. For example, a goroutine was unblocked on a mutex,
	// but another goroutine got ahead and acquired the mutex before the first
	// goroutine is scheduled, so the first goroutine has to block again.
	// Such wakeups happen on buffered channels and sync.Mutex,
	// but are generally not interesting for end user.
	traceFutileWakeup byte = 128
)

// trace is global tracing context.
// 注释：栈追踪的全局上下文结构体
var trace struct {
	lock          mutex           // protects the following members
	lockOwner     *g              // 注释：主G地址 // to avoid deadlocks during recursive lock locks // 注释：在递归锁期间避免死锁
	enabled       bool            // 注释：是否开启栈追踪 // when set runtime traces events
	shutdown      bool            // set when we are waiting for trace reader to finish after setting enabled to false
	headerWritten bool            // whether ReadTrace has emitted trace header
	footerWritten bool            // whether ReadTrace has emitted trace footer
	shutdownSema  uint32          // used to wait for ReadTrace completion
	seqStart      uint64          // sequence number when tracing was started
	ticksStart    int64           // cputicks when tracing was started
	ticksEnd      int64           // cputicks when tracing was stopped
	timeStart     int64           // nanotime when tracing was started
	timeEnd       int64           // nanotime when tracing was stopped
	seqGC         uint64          // GC start/done sequencer
	reading       traceBufPtr     // buffer currently handed off to user
	empty         traceBufPtr     // 注释：空缓冲区堆栈 // stack of empty buffers
	fullHead      traceBufPtr     // 注释：缓冲区已满时的缓冲区地址队列的头部 // queue of full buffers
	fullTail      traceBufPtr     // 注释：缓冲区队列的尾部地址对象
	reader        guintptr        // goroutine that called ReadTrace, or nil
	stackTab      traceStackTable // maps stack traces to unique ids // 注释：将堆栈跟踪映射到唯一id

	// Dictionary for traceEvString.
	//
	// TODO: central lock to access the map is not ideal.
	//   option: pre-assign ids to all user annotation region names and tags
	//   option: per-P cache
	//   option: sync.Map like data structure
	stringsLock mutex
	strings     map[string]uint64
	stringSeq   uint64

	// markWorkerLabels maps gcMarkWorkerMode to string ID.
	markWorkerLabels [len(gcMarkWorkerModeStrings)]uint64

	bufLock mutex       // 注释：栈追踪锁(exitsyscall中跟踪期间StartTrace/StopTrace的争用通过在traceLockBuffer中锁定trace.bufLock来解决) // protects buf
	buf     traceBufPtr // 注释：全局跟踪缓冲区，在没有p的情况下运行时使用 // global trace buffer, used when running without a p
}

// traceBufHeader is per-P tracing buffer.
type traceBufHeader struct {
	link      traceBufPtr             // 注释：记录的缓冲区地址对象 in trace.empty/full
	lastTicks uint64                  // 注释：当我们写最后一个事件的时间 // when we wrote the last event
	pos       int                     // 注释：arr中的下一个写入偏移下标 // next write offset in arr
	stk       [traceStackSize]uintptr // 注释：栈追踪数组数据缓冲区(用来存储栈ID的数组)（8位（128）个数的指针数组）scratch buffer for traceback
}

// traceBuf is per-P tracing buffer.
//
//go:notinheap
type traceBuf struct {
	traceBufHeader
	arr [64<<10 - unsafe.Sizeof(traceBufHeader{})]byte // 注释：traceBufHeader.buf的基础缓冲区 // underlying buffer for traceBufHeader.buf
}

// traceBufPtr is a *traceBuf that is not traced by the garbage
// collector and doesn't have write barriers. traceBufs are not
// allocated from the GC'd heap, so this is safe, and are often
// manipulated in contexts where write barriers are not allowed, so
// this is necessary.
//
// TODO: Since traceBuf is now go:notinheap, this isn't necessary.
type traceBufPtr uintptr // 栈追踪的全局缓冲区地址

func (tp traceBufPtr) ptr() *traceBuf   { return (*traceBuf)(unsafe.Pointer(tp)) }
func (tp *traceBufPtr) set(b *traceBuf) { *tp = traceBufPtr(unsafe.Pointer(b)) }
func traceBufPtrOf(b *traceBuf) traceBufPtr {
	return traceBufPtr(unsafe.Pointer(b))
}

// StartTrace enables tracing for the current process.
// While tracing, the data will be buffered and available via ReadTrace.
// StartTrace returns an error if tracing is already enabled.
// Most clients should use the runtime/trace package or the testing package's
// -test.trace flag instead of calling StartTrace directly.
func StartTrace() error {
	// Stop the world so that we can take a consistent snapshot
	// of all goroutines at the beginning of the trace.
	// Do not stop the world during GC so we ensure we always see
	// a consistent view of GC-related events (e.g. a start is always
	// paired with an end).
	stopTheWorldGC("start tracing")

	// Prevent sysmon from running any code that could generate events.
	lock(&sched.sysmonlock)

	// We are in stop-the-world, but syscalls can finish and write to trace concurrently.
	// Exitsyscall could check trace.enabled long before and then suddenly wake up
	// and decide to write to trace at a random point in time.
	// However, such syscall will use the global trace.buf buffer, because we've
	// acquired all p's by doing stop-the-world. So this protects us from such races.
	lock(&trace.bufLock)

	if trace.enabled || trace.shutdown {
		unlock(&trace.bufLock)
		unlock(&sched.sysmonlock)
		startTheWorldGC()
		return errorString("tracing is already enabled")
	}

	// Can't set trace.enabled yet. While the world is stopped, exitsyscall could
	// already emit a delayed event (see exitTicks in exitsyscall) if we set trace.enabled here.
	// That would lead to an inconsistent trace:
	// - either GoSysExit appears before EvGoInSyscall,
	// - or GoSysExit appears for a goroutine for which we don't emit EvGoInSyscall below.
	// To instruct traceEvent that it must not ignore events below, we set startingtrace.
	// trace.enabled is set afterwards once we have emitted all preliminary events.
	_g_ := getg()
	_g_.m.startingtrace = true

	// Obtain current stack ID to use in all traceEvGoCreate events below.
	mp := acquirem()
	stkBuf := make([]uintptr, traceStackSize)
	stackID := traceStackID(mp, stkBuf, 2)
	releasem(mp)

	for _, gp := range allgs {
		status := readgstatus(gp)
		if status != _Gdead {
			gp.traceseq = 0
			gp.tracelastp = getg().m.p
			// +PCQuantum because traceFrameForPC expects return PCs and subtracts PCQuantum.
			id := trace.stackTab.put([]uintptr{gp.startpc + sys.PCQuantum})
			traceEvent(traceEvGoCreate, -1, uint64(gp.goid), uint64(id), stackID)
		}
		if status == _Gwaiting {
			// traceEvGoWaiting is implied to have seq=1.
			gp.traceseq++
			traceEvent(traceEvGoWaiting, -1, uint64(gp.goid))
		}
		if status == _Gsyscall {
			gp.traceseq++
			traceEvent(traceEvGoInSyscall, -1, uint64(gp.goid))
		} else {
			gp.sysblocktraced = false
		}
	}
	traceProcStart()
	traceGoStart()
	// Note: ticksStart needs to be set after we emit traceEvGoInSyscall events.
	// If we do it the other way around, it is possible that exitsyscall will
	// query sysexitticks after ticksStart but before traceEvGoInSyscall timestamp.
	// It will lead to a false conclusion that cputicks is broken.
	trace.ticksStart = cputicks()
	trace.timeStart = nanotime()
	trace.headerWritten = false
	trace.footerWritten = false

	// string to id mapping
	//  0 : reserved for an empty string
	//  remaining: other strings registered by traceString
	trace.stringSeq = 0
	trace.strings = make(map[string]uint64)

	trace.seqGC = 0
	_g_.m.startingtrace = false
	trace.enabled = true

	// Register runtime goroutine labels.
	_, pid, bufp := traceAcquireBuffer()
	for i, label := range gcMarkWorkerModeStrings[:] {
		trace.markWorkerLabels[i], bufp = traceString(bufp, pid, label)
	}
	traceReleaseBuffer(pid)

	unlock(&trace.bufLock)

	unlock(&sched.sysmonlock)

	startTheWorldGC()
	return nil
}

// StopTrace stops tracing, if it was previously enabled.
// StopTrace only returns after all the reads for the trace have completed.
func StopTrace() {
	// Stop the world so that we can collect the trace buffers from all p's below,
	// and also to avoid races with traceEvent.
	stopTheWorldGC("stop tracing")

	// See the comment in StartTrace.
	lock(&sched.sysmonlock)

	// See the comment in StartTrace.
	lock(&trace.bufLock)

	if !trace.enabled {
		unlock(&trace.bufLock)
		unlock(&sched.sysmonlock)
		startTheWorldGC()
		return
	}

	traceGoSched()

	// Loop over all allocated Ps because dead Ps may still have
	// trace buffers.
	for _, p := range allp[:cap(allp)] {
		buf := p.tracebuf
		if buf != 0 {
			traceFullQueue(buf)
			p.tracebuf = 0
		}
	}
	if trace.buf != 0 {
		buf := trace.buf
		trace.buf = 0
		if buf.ptr().pos != 0 {
			traceFullQueue(buf)
		}
	}

	for {
		trace.ticksEnd = cputicks()
		trace.timeEnd = nanotime()
		// Windows time can tick only every 15ms, wait for at least one tick.
		if trace.timeEnd != trace.timeStart {
			break
		}
		osyield()
	}

	trace.enabled = false
	trace.shutdown = true
	unlock(&trace.bufLock)

	unlock(&sched.sysmonlock)

	startTheWorldGC()

	// The world is started but we've set trace.shutdown, so new tracing can't start.
	// Wait for the trace reader to flush pending buffers and stop.
	semacquire(&trace.shutdownSema)
	if raceenabled {
		raceacquire(unsafe.Pointer(&trace.shutdownSema))
	}

	// The lock protects us from races with StartTrace/StopTrace because they do stop-the-world.
	lock(&trace.lock)
	for _, p := range allp[:cap(allp)] {
		if p.tracebuf != 0 {
			throw("trace: non-empty trace buffer in proc")
		}
	}
	if trace.buf != 0 {
		throw("trace: non-empty global trace buffer")
	}
	if trace.fullHead != 0 || trace.fullTail != 0 {
		throw("trace: non-empty full trace buffer")
	}
	if trace.reading != 0 || trace.reader != 0 {
		throw("trace: reading after shutdown")
	}
	for trace.empty != 0 {
		buf := trace.empty
		trace.empty = buf.ptr().link
		sysFree(unsafe.Pointer(buf), unsafe.Sizeof(*buf.ptr()), &memstats.other_sys)
	}
	trace.strings = nil
	trace.shutdown = false
	unlock(&trace.lock)
}

// ReadTrace returns the next chunk of binary tracing data, blocking until data
// is available. If tracing is turned off and all the data accumulated while it
// was on has been returned, ReadTrace returns nil. The caller must copy the
// returned data before calling ReadTrace again.
// ReadTrace must be called from one goroutine at a time.
func ReadTrace() []byte {
	// This function may need to lock trace.lock recursively
	// (goparkunlock -> traceGoPark -> traceEvent -> traceFlush).
	// To allow this we use trace.lockOwner.
	// Also this function must not allocate while holding trace.lock:
	// allocation can call heap allocate, which will try to emit a trace
	// event while holding heap lock.
	lock(&trace.lock)
	trace.lockOwner = getg()

	if trace.reader != 0 {
		// More than one goroutine reads trace. This is bad.
		// But we rather do not crash the program because of tracing,
		// because tracing can be enabled at runtime on prod servers.
		trace.lockOwner = nil
		unlock(&trace.lock)
		println("runtime: ReadTrace called from multiple goroutines simultaneously")
		return nil
	}
	// Recycle the old buffer.
	if buf := trace.reading; buf != 0 {
		buf.ptr().link = trace.empty
		trace.empty = buf
		trace.reading = 0
	}
	// Write trace header.
	if !trace.headerWritten {
		trace.headerWritten = true
		trace.lockOwner = nil
		unlock(&trace.lock)
		return []byte("go 1.11 trace\x00\x00\x00")
	}
	// Wait for new data.
	if trace.fullHead == 0 && !trace.shutdown {
		trace.reader.set(getg())
		goparkunlock(&trace.lock, waitReasonTraceReaderBlocked, traceEvGoBlock, 2)
		lock(&trace.lock)
	}
	// Write a buffer.
	if trace.fullHead != 0 {
		buf := traceFullDequeue()
		trace.reading = buf
		trace.lockOwner = nil
		unlock(&trace.lock)
		return buf.ptr().arr[:buf.ptr().pos]
	}
	// Write footer with timer frequency.
	if !trace.footerWritten {
		trace.footerWritten = true
		// Use float64 because (trace.ticksEnd - trace.ticksStart) * 1e9 can overflow int64.
		freq := float64(trace.ticksEnd-trace.ticksStart) * 1e9 / float64(trace.timeEnd-trace.timeStart) / traceTickDiv
		trace.lockOwner = nil
		unlock(&trace.lock)
		var data []byte
		data = append(data, traceEvFrequency|0<<traceArgCountShift)
		data = traceAppend(data, uint64(freq))
		// This will emit a bunch of full buffers, we will pick them up
		// on the next iteration.
		trace.stackTab.dump()
		return data
	}
	// Done.
	if trace.shutdown {
		trace.lockOwner = nil
		unlock(&trace.lock)
		if raceenabled {
			// Model synchronization on trace.shutdownSema, which race
			// detector does not see. This is required to avoid false
			// race reports on writer passed to trace.Start.
			racerelease(unsafe.Pointer(&trace.shutdownSema))
		}
		// trace.enabled is already reset, so can call traceable functions.
		semrelease(&trace.shutdownSema)
		return nil
	}
	// Also bad, but see the comment above.
	trace.lockOwner = nil
	unlock(&trace.lock)
	println("runtime: spurious wakeup of trace reader")
	return nil
}

// traceReader returns the trace reader that should be woken up, if any.
func traceReader() *g {
	if trace.reader == 0 || (trace.fullHead == 0 && !trace.shutdown) {
		return nil
	}
	lock(&trace.lock)
	if trace.reader == 0 || (trace.fullHead == 0 && !trace.shutdown) {
		unlock(&trace.lock)
		return nil
	}
	gp := trace.reader.ptr()
	trace.reader.set(nil)
	unlock(&trace.lock)
	return gp
}

// traceProcFree frees trace buffer associated with pp.
func traceProcFree(pp *p) {
	buf := pp.tracebuf
	pp.tracebuf = 0
	if buf == 0 {
		return
	}
	lock(&trace.lock)
	traceFullQueue(buf)
	unlock(&trace.lock)
}

// traceFullQueue queues buf into queue of full buffers. // 注释：traceFullQueue将buf排队到缓冲区已满的队列中。
func traceFullQueue(buf traceBufPtr) {
	buf.ptr().link = 0
	if trace.fullHead == 0 {
		trace.fullHead = buf
	} else {
		trace.fullTail.ptr().link = buf
	}
	trace.fullTail = buf
}

// traceFullDequeue dequeues from queue of full buffers.
func traceFullDequeue() traceBufPtr {
	buf := trace.fullHead
	if buf == 0 {
		return 0
	}
	trace.fullHead = buf.ptr().link
	if trace.fullHead == 0 {
		trace.fullTail = 0
	}
	buf.ptr().link = 0
	return buf
}

// traceEvent writes a single event to trace buffer, flushing the buffer if necessary.
// 注释：traceEvent将单个事件写入跟踪缓冲区，必要时刷新缓冲区。
// ev is event type. // 注释：ev是事件类型。
// If skip > 0, write current stack id as the last argument (skipping skip top frames).
// 注释：如果skip>0，则写入当前堆栈id作为最后一个参数（跳过顶部帧）。
// If skip = 0, this event type should contain a stack, but we don't want
// to collect and remember it for this particular call.
// 注释：如果skip=0，则此事件类型应该包含一个堆栈，但我们不希望为该特定调用收集并记住它。
//
// 注释：栈追踪事件
func traceEvent(ev byte, skip int, args ...uint64) {
	mp, pid, bufp := traceAcquireBuffer() // 注释：获取当前G的M；M对应P的ID；全局栈追踪的缓冲区地址的指针(对象)
	// Double-check trace.enabled now that we've done m.locks++ and acquired bufLock.
	// This protects from races between traceEvent and StartTrace/StopTrace.
	// 注释：译：既然我们已经完成了m.locks++并获得了bufLock，请仔细检查trace.enabled。这可以防止traceEvent和StartTrace/StopTrace之间的竞争。
	//
	// The caller checked that trace.enabled == true, but trace.enabled might have been
	// turned off between the check and now. Check again. traceLockBuffer did mp.locks++,
	// StopTrace does stopTheWorld, and stopTheWorld waits for mp.locks to go back to zero,
	// so if we see trace.enabled == true now, we know it's true for the rest of the function.
	// Exitsyscall can run even during stopTheWorld. The race with StartTrace/StopTrace
	// during tracing in exitsyscall is resolved by locking trace.bufLock in traceLockBuffer.
	// 注释：译：调用方检查trace.enabled==true，但trace.enabed可能在检查期间和现在之间已关闭。再次检查。
	//		traceLockBuffer做了mp.locks++，StopTrace做了stopTheWorld，stopTheWorld等待mp.locks归零，
	//		所以如果我们现在看到trace.enabled==true，我们就知道函数的其余部分也是如此。Exitsyscall甚至可以在stopTheWorld期间运行。
	//		在exitsyscall中跟踪期间与StartTrace/StopTrace的竞争通过在traceLockBuffer中锁定trace.bufLock来解决。
	//
	// Note trace_userTaskCreate runs the same check.
	// 注释：译：请注意trace_userTaskCreate运行相同的检查。
	if !trace.enabled && !mp.startingtrace { // 注释：如果栈追踪已经关闭，并且没有开始执行栈追踪则释放并返回
		traceReleaseBuffer(pid)
		return
	}

	if skip > 0 {
		if getg() == mp.curg { // 注释：如果线程本地存储中的G和当前M里的当前G相等则标记skip++(跳过顶部帧和当前指针)
			skip++ // +1 because stack is captured in traceEventLocked.
		}
	}
	traceEventLocked(0, mp, pid, bufp, ev, skip, args...)
	traceReleaseBuffer(pid)
}

func traceEventLocked(extraBytes int, mp *m, pid int32, bufp *traceBufPtr, ev byte, skip int, args ...uint64) {
	buf := bufp.ptr() // 注释获取缓冲区对象
	// TODO: test on non-zero extraBytes param.
	maxSize := 2 + 5*traceBytesPerNumber + extraBytes // 注释：事件类型、长度、序列、时间戳、堆栈id和两个添加参数 // event type, length, sequence, timestamp, stack id and two add params
	if buf == nil || len(buf.arr)-buf.pos < maxSize { // 注释：如果没有缓冲区对象，或缓冲区数组无法容纳时（数组长度 - 要写的位置 - 基础的数据 < 0）
		buf = traceFlush(traceBufPtrOf(buf), pid).ptr() // 注释：把当前缓冲区放到队列中，返回空缓冲区地址
		bufp.set(buf)                                   // 注释：重新设置缓冲区地址对象
	}
	// 注释：上面就是保证缓冲区的空间有足够的空间

	ticks := uint64(cputicks()) / traceTickDiv // 注释：获取系统时钟时间
	tickDiff := ticks - buf.lastTicks          // 注释：本次消耗的时间 = 系统时间 - 最后一次写入时间
	buf.lastTicks = ticks                      // 注释：记录本次执行的时间
	narg := byte(len(args))                    // 注释：获取扩展入参数量的byte大小
	if skip >= 0 {
		narg++
	}
	// We have only 2 bits for number of arguments. // 注释：我们只有2位作为自变量的数量。
	// If number is >= 3, then the event type is followed by event length in bytes. // 注释：如果数字>=3，则事件类型后面跟着以字节为单位的事件长度。
	// 注释：现在最大位数；最大位数是两位（3=011）
	if narg > 3 {
		narg = 3
	}
	startPos := buf.pos                     // 注释：获取开始写入的偏移量下标，后面记录本次偏移量大小时使用
	buf.byte(ev | narg<<traceArgCountShift) // 注释：(写入头部1byte的数据)（共8位）把参数大小（高位）和事件类型（低位），存储到缓冲区数组里（buf.arr）(每次记录的头部位置),并且偏移量下标加一
	var lenp *byte
	if narg == 3 {
		// Reserve the byte for length assuming that length < 128. // 注释：假设长度<128，则保留字节的长度。
		buf.varint(0)              // 注释：写入一个0到缓冲数组中
		lenp = &buf.arr[buf.pos-1] // 注释：记录写入前的地址(就是上面写入的0的位置的地址，方便后面对这块内存数据进行修改)
	}
	buf.varint(tickDiff)     // 注释：把本次消耗的时间记录到缓冲区数组里
	for _, a := range args { // 注释：循环扩展参数，把扩展参数记录到缓冲区数组里
		buf.varint(a) // 注释：把扩展参数记录到缓冲区数组里
	}
	if skip == 0 { // 注释：如果步间值是0时
		buf.varint(0) // 注释：记录0到缓冲区数组里
	} else if skip > 0 {
		buf.varint(traceStackID(mp, buf.stk[:], skip)) // 注释：(获取栈ID并且添加到缓冲区数组里)跳过skip的个数的栈ID（buf.stk数组里存储的是栈ID）
	}
	evSize := buf.pos - startPos
	if evSize > maxSize {
		throw("invalid length of trace event")
	}
	if lenp != nil {
		// Fill in actual length.
		*lenp = byte(evSize - 2)
	}
}

// 注释：跳过skip个数的的栈ID，返回跳过后的栈ID，buf是栈ID数组，skip是要跳过的栈ID的个数
func traceStackID(mp *m, buf []uintptr, skip int) uint64 {
	_g_ := getg() // 注释：从TLS里获取G地址对象
	gp := mp.curg // 注释：获取传入的M中的G
	var nstk int
	if gp == _g_ { // 注释：如果传入的M中的G等于当前G
		nstk = callers(skip+1, buf)
	} else if gp != nil {
		gp = mp.curg
		nstk = gcallers(gp, skip, buf)
	}
	if nstk > 0 {
		nstk-- // skip runtime.goexit
	}
	if nstk > 0 && gp.goid == 1 {
		nstk-- // skip runtime.main
	}
	id := trace.stackTab.put(buf[:nstk]) // 注释：
	return uint64(id)
}

// traceAcquireBuffer returns trace buffer to use and, if necessary, locks it.
// 注释：获取当前G的M；M对应P的ID；全局栈追踪的缓冲区地址的指针(对象)
func traceAcquireBuffer() (mp *m, pid int32, bufp *traceBufPtr) {
	mp = acquirem()                // 注释：获取当前G的M,并加锁禁止抢占
	if p := mp.p.ptr(); p != nil { // 注释：判断如果M的P存在，直接返回M和对应的P的ID，和P栈追踪缓冲区地址的指针(对象)
		return mp, p.id, &p.tracebuf
	}
	lock(&trace.bufLock)                 // 注释：栈追踪，锁定
	return mp, traceGlobProc, &trace.buf // 注释：返回M，当我们在没有真实P的情况下进行跟踪时使用的伪P的标识符；栈追踪缓冲区地址的指针(对象)
}

// traceReleaseBuffer releases a buffer previously acquired with traceAcquireBuffer.
// 注释：释放先前使用traceAcquireBuffer获取的缓冲区。
func traceReleaseBuffer(pid int32) {
	if pid == traceGlobProc { // 注释：如果没有对应P的ID直接解锁（没有P的时候会临时放一个站位的数组traceGlobProc）
		unlock(&trace.bufLock)
	}
	releasem(getg().m) // 注释：释放M（解锁）
}

// traceFlush puts buf onto stack of full buffers and returns an empty buffer. // 注释：traceFlush将buf放入已满缓冲区的堆栈中，并返回一个空缓冲区。
// 注释：把buf缓冲区放到已满的缓冲区队列中，返回空的缓冲区对象地址
func traceFlush(buf traceBufPtr, pid int32) traceBufPtr {
	owner := trace.lockOwner                         // 注释：主G地址
	dolock := owner == nil || owner != getg().m.curg // 注释：如果没有主G地址或者当前的G不是主G时为ture
	if dolock {                                      // 注释：如果没有主G地址或者当前的G不是主G时为ture
		lock(&trace.lock) // 注释：如果没有主G地址或者当前的G不是主G时，添加锁
	}
	if buf != 0 {
		traceFullQueue(buf) // 注释：把buf缓冲区放到已满的缓冲区队列中
	}
	if trace.empty != 0 { // 注释：如果空缓冲区有缓冲区数据时，用空缓冲区里的数据对象
		buf = trace.empty            // 注释：空缓冲区堆栈(和下面bufp.link.set(nil)一起组成出栈)
		trace.empty = buf.ptr().link // 注释：把链表下一个缓冲区对象放到空缓冲区里
	} else {
		buf = traceBufPtr(sysAlloc(unsafe.Sizeof(traceBuf{}), &memstats.other_sys)) // 注释：如果空缓冲区对象里没有数据时，申请内存空间
		if buf == 0 {
			throw("trace: out of memory")
		}
	}
	bufp := buf.ptr()  // 注释：取出缓冲区对象
	bufp.link.set(nil) // 注释：断开链表连接（相当于出栈）
	bufp.pos = 0       // 注释：清空下一个要写入的偏移量下标，（已经把偏移量的位置数据放到缓冲区已满的队列里了）

	// initialize the buffer for a new batch
	ticks := uint64(cputicks()) / traceTickDiv      // 注释：获取系统时钟时间
	bufp.lastTicks = ticks                          // 注释：记录最后一次处理的时间
	bufp.byte(traceEvBatch | 1<<traceArgCountShift) // 注释：把traceEvBatch | 1<<traceArgCountShift(固定的数据前缀)放到缓冲区arr数组中，并把下一个要插入的偏移量下标加一（bufp.pos）
	bufp.varint(uint64(pid))                        // 注释：把pid写入到缓冲去数组arr里
	bufp.varint(ticks)                              // 注释：把系统时钟时间写入到缓冲去数组arr里

	if dolock {
		unlock(&trace.lock) // 注释：如果没有主G地址或者当前的G不是主G时，上面加的锁，这里要解锁
	}
	return buf // 注释：返回缓冲区数据对象
}

// traceString adds a string to the trace.strings and returns the id.
func traceString(bufp *traceBufPtr, pid int32, s string) (uint64, *traceBufPtr) {
	if s == "" {
		return 0, bufp
	}

	lock(&trace.stringsLock)
	if raceenabled {
		// raceacquire is necessary because the map access
		// below is race annotated.
		raceacquire(unsafe.Pointer(&trace.stringsLock))
	}

	if id, ok := trace.strings[s]; ok {
		if raceenabled {
			racerelease(unsafe.Pointer(&trace.stringsLock))
		}
		unlock(&trace.stringsLock)

		return id, bufp
	}

	trace.stringSeq++
	id := trace.stringSeq
	trace.strings[s] = id

	if raceenabled {
		racerelease(unsafe.Pointer(&trace.stringsLock))
	}
	unlock(&trace.stringsLock)

	// memory allocation in above may trigger tracing and
	// cause *bufp changes. Following code now works with *bufp,
	// so there must be no memory allocation or any activities
	// that causes tracing after this point.

	buf := bufp.ptr()
	size := 1 + 2*traceBytesPerNumber + len(s)
	if buf == nil || len(buf.arr)-buf.pos < size {
		buf = traceFlush(traceBufPtrOf(buf), pid).ptr()
		bufp.set(buf)
	}
	buf.byte(traceEvString)
	buf.varint(id)

	// double-check the string and the length can fit.
	// Otherwise, truncate the string.
	slen := len(s)
	if room := len(buf.arr) - buf.pos; room < slen+traceBytesPerNumber {
		slen = room
	}

	buf.varint(uint64(slen))
	buf.pos += copy(buf.arr[buf.pos:], s[:slen])

	bufp.set(buf)
	return id, bufp
}

// traceAppend appends v to buf in little-endian-base-128 encoding.
func traceAppend(buf []byte, v uint64) []byte {
	for ; v >= 0x80; v >>= 7 {
		buf = append(buf, 0x80|byte(v))
	}
	buf = append(buf, byte(v))
	return buf
}

// varint appends v to buf in little-endian-base-128 encoding.
// 注释：variant在little-endian-base-128编码中将v附加到buf。
// 注释：把v写入到缓冲去数组arr里
func (buf *traceBuf) varint(v uint64) {
	pos := buf.pos // 注释：获取要写入的偏移量下标
	// 注释：循环把数据写入到缓冲区数组里(每8位循环一次（0X80=128）)，处理64为系统的指针数据放到byte(8位)里
	for ; v >= 0x80; v >>= 7 {
		buf.arr[pos] = 0x80 | byte(v)
		pos++
	}
	buf.arr[pos] = byte(v) // 注释：把最后不足8位的数据放到数组里
	pos++                  // 注释：偏移量下标加一
	buf.pos = pos          // 注释：设置下一个要插入的缓冲区偏移量下标
}

// byte appends v to buf. // 注释：byte将v附加到buf。
// 注释：把一个8位的byte类型加入到缓冲区数组里，并且偏移量下标加一
func (buf *traceBuf) byte(v byte) {
	buf.arr[buf.pos] = v
	buf.pos++
}

// traceStackTable maps stack traces (arrays of PC's) to unique uint32 ids.
// It is lock-free for reading.
type traceStackTable struct {
	lock mutex
	seq  uint32
	mem  traceAlloc
	tab  [1 << 13]traceStackPtr
}

// traceStack is a single stack in traceStackTable.
// 注释：traceStack是traceStackTable中的单个堆栈。
// 注释：追溯的堆栈记录
type traceStack struct {
	link traceStackPtr // 注释：追溯堆栈指针对象
	hash uintptr       // 注释：堆栈内存的哈希值
	id   uint32        // 注释：追溯的栈记录的自增ID，（每次访问堆栈对象时自增）
	n    int           // 注释：堆栈的长度（堆栈内存个数）
	stk  [0]uintptr    // 注释：堆栈ID(内存地址)数组(也是PC值)的首指针，用来存储堆栈的ID数组，数组的长度时n // real type [n]uintptr
}

type traceStackPtr uintptr

func (tp traceStackPtr) ptr() *traceStack { return (*traceStack)(unsafe.Pointer(tp)) }

// stack returns slice of PCs.
func (ts *traceStack) stack() []uintptr {
	return (*[traceStackSize]uintptr)(unsafe.Pointer(&ts.stk))[:ts.n]
}

// put returns a unique id for the stack trace pcs and caches it in the table,
// if it sees the trace for the first time.
// 注释：put为堆栈跟踪pc返回一个唯一的id，如果它第一次看到跟踪，则将其缓存在表中。
func (tab *traceStackTable) put(pcs []uintptr) uint32 {
	if len(pcs) == 0 {
		return 0
	}
	hash := memhash(unsafe.Pointer(&pcs[0]), 0, uintptr(len(pcs))*unsafe.Sizeof(pcs[0]))
	// First, search the hashtable w/o the mutex.
	if id := tab.find(pcs, hash); id != 0 {
		return id
	}
	// Now, double check under the mutex.
	lock(&tab.lock)
	if id := tab.find(pcs, hash); id != 0 {
		unlock(&tab.lock)
		return id
	}
	// Create new record. // 注释：创建新的记录
	tab.seq++                     // 注释：堆栈记录的ID
	stk := tab.newStack(len(pcs)) // 注释：申请堆栈记录的内存
	stk.hash = hash               // 注释：保存哈希
	stk.id = tab.seq              // 注释：保存记录的ID
	stk.n = len(pcs)              // 注释：保存记录的长度
	stkpc := stk.stack()          // 注释：记录的数据部分的指针数组，是有多个栈地址组成的数组，长度是n
	for i, pc := range pcs {      // 注释：把传入的栈id(内存地址)
		stkpc[i] = pc // 注释：把传入的栈id(内存地址)，写入数组中(记录的数据部分)
	}
	part := int(hash % uintptr(len(tab.tab)))
	stk.link = tab.tab[part]
	atomicstorep(unsafe.Pointer(&tab.tab[part]), unsafe.Pointer(stk))
	unlock(&tab.lock)
	return stk.id // 注释：返回记录的ID
}

// find checks if the stack trace pcs is already present in the table.
func (tab *traceStackTable) find(pcs []uintptr, hash uintptr) uint32 {
	part := int(hash % uintptr(len(tab.tab)))
Search:
	for stk := tab.tab[part].ptr(); stk != nil; stk = stk.link.ptr() {
		if stk.hash == hash && stk.n == len(pcs) {
			for i, stkpc := range stk.stack() {
				if stkpc != pcs[i] {
					continue Search
				}
			}
			return stk.id
		}
	}
	return 0
}

// newStack allocates a new stack of size n.
// 注释：申请堆栈记录的内存，内存的大小时结构体的大小加数据数组的大小
func (tab *traceStackTable) newStack(n int) *traceStack {
	return (*traceStack)(tab.mem.alloc(unsafe.Sizeof(traceStack{}) + uintptr(n)*sys.PtrSize))
}

// allFrames returns all of the Frames corresponding to pcs.
func allFrames(pcs []uintptr) []Frame {
	frames := make([]Frame, 0, len(pcs))
	ci := CallersFrames(pcs)
	for {
		f, more := ci.Next()
		frames = append(frames, f)
		if !more {
			return frames
		}
	}
}

// dump writes all previously cached stacks to trace buffers,
// releases all memory and resets state.
func (tab *traceStackTable) dump() {
	var tmp [(2 + 4*traceStackSize) * traceBytesPerNumber]byte
	bufp := traceFlush(0, 0)
	for _, stk := range tab.tab {
		stk := stk.ptr()
		for ; stk != nil; stk = stk.link.ptr() {
			tmpbuf := tmp[:0]
			tmpbuf = traceAppend(tmpbuf, uint64(stk.id))
			frames := allFrames(stk.stack())
			tmpbuf = traceAppend(tmpbuf, uint64(len(frames)))
			for _, f := range frames {
				var frame traceFrame
				frame, bufp = traceFrameForPC(bufp, 0, f)
				tmpbuf = traceAppend(tmpbuf, uint64(f.PC))
				tmpbuf = traceAppend(tmpbuf, uint64(frame.funcID))
				tmpbuf = traceAppend(tmpbuf, uint64(frame.fileID))
				tmpbuf = traceAppend(tmpbuf, uint64(frame.line))
			}
			// Now copy to the buffer.
			size := 1 + traceBytesPerNumber + len(tmpbuf)
			if buf := bufp.ptr(); len(buf.arr)-buf.pos < size {
				bufp = traceFlush(bufp, 0)
			}
			buf := bufp.ptr()
			buf.byte(traceEvStack | 3<<traceArgCountShift)
			buf.varint(uint64(len(tmpbuf)))
			buf.pos += copy(buf.arr[buf.pos:], tmpbuf)
		}
	}

	lock(&trace.lock)
	traceFullQueue(bufp)
	unlock(&trace.lock)

	tab.mem.drop()
	*tab = traceStackTable{}
	lockInit(&((*tab).lock), lockRankTraceStackTab)
}

type traceFrame struct {
	funcID uint64
	fileID uint64
	line   uint64
}

// traceFrameForPC records the frame information.
// It may allocate memory.
func traceFrameForPC(buf traceBufPtr, pid int32, f Frame) (traceFrame, traceBufPtr) {
	bufp := &buf
	var frame traceFrame

	fn := f.Function
	const maxLen = 1 << 10
	if len(fn) > maxLen {
		fn = fn[len(fn)-maxLen:]
	}
	frame.funcID, bufp = traceString(bufp, pid, fn)
	frame.line = uint64(f.Line)
	file := f.File
	if len(file) > maxLen {
		file = file[len(file)-maxLen:]
	}
	frame.fileID, bufp = traceString(bufp, pid, file)
	return frame, (*bufp)
}

// traceAlloc is a non-thread-safe region allocator.
// It holds a linked list of traceAllocBlock.
type traceAlloc struct {
	head traceAllocBlockPtr
	off  uintptr
}

// traceAllocBlock is a block in traceAlloc.
//
// traceAllocBlock is allocated from non-GC'd memory, so it must not
// contain heap pointers. Writes to pointers to traceAllocBlocks do
// not need write barriers.
//
//go:notinheap
type traceAllocBlock struct {
	next traceAllocBlockPtr
	data [64<<10 - sys.PtrSize]byte
}

// TODO: Since traceAllocBlock is now go:notinheap, this isn't necessary.
type traceAllocBlockPtr uintptr

func (p traceAllocBlockPtr) ptr() *traceAllocBlock   { return (*traceAllocBlock)(unsafe.Pointer(p)) }
func (p *traceAllocBlockPtr) set(x *traceAllocBlock) { *p = traceAllocBlockPtr(unsafe.Pointer(x)) }

// alloc allocates n-byte block.
func (a *traceAlloc) alloc(n uintptr) unsafe.Pointer {
	n = alignUp(n, sys.PtrSize)
	if a.head == 0 || a.off+n > uintptr(len(a.head.ptr().data)) {
		if n > uintptr(len(a.head.ptr().data)) {
			throw("trace: alloc too large")
		}
		block := (*traceAllocBlock)(sysAlloc(unsafe.Sizeof(traceAllocBlock{}), &memstats.other_sys))
		if block == nil {
			throw("trace: out of memory")
		}
		block.next.set(a.head.ptr())
		a.head.set(block)
		a.off = 0
	}
	p := &a.head.ptr().data[a.off]
	a.off += n
	return unsafe.Pointer(p)
}

// drop frees all previously allocated memory and resets the allocator.
func (a *traceAlloc) drop() {
	for a.head != 0 {
		block := a.head.ptr()
		a.head.set(block.next.ptr())
		sysFree(unsafe.Pointer(block), unsafe.Sizeof(traceAllocBlock{}), &memstats.other_sys)
	}
}

// The following functions write specific events to trace.

func traceGomaxprocs(procs int32) {
	traceEvent(traceEvGomaxprocs, 1, uint64(procs))
}

func traceProcStart() {
	traceEvent(traceEvProcStart, -1, uint64(getg().m.id))
}

func traceProcStop(pp *p) {
	// Sysmon and stopTheWorld can stop Ps blocked in syscalls,
	// to handle this we temporary employ the P.
	// 注释：Sysmon和stopTheWorld可以阻止系统调用中阻止的P，为了处理这个问题，我们临时使用了P。
	mp := acquirem()
	oldp := mp.p
	mp.p.set(pp)
	traceEvent(traceEvProcStop, -1)
	mp.p = oldp
	releasem(mp)
}

func traceGCStart() {
	traceEvent(traceEvGCStart, 3, trace.seqGC)
	trace.seqGC++
}

func traceGCDone() {
	traceEvent(traceEvGCDone, -1)
}

func traceGCSTWStart(kind int) {
	traceEvent(traceEvGCSTWStart, -1, uint64(kind))
}

func traceGCSTWDone() {
	traceEvent(traceEvGCSTWDone, -1)
}

// traceGCSweepStart prepares to trace a sweep loop. This does not
// emit any events until traceGCSweepSpan is called.
//
// traceGCSweepStart must be paired with traceGCSweepDone and there
// must be no preemption points between these two calls.
// 注释：标记GC清扫的链路最终开始
func traceGCSweepStart() {
	// Delay the actual GCSweepStart event until the first span
	// sweep. If we don't sweep anything, don't emit any events.
	_p_ := getg().m.p.ptr()
	if _p_.traceSweep {
		throw("double traceGCSweepStart")
	}
	_p_.traceSweep, _p_.traceSwept, _p_.traceReclaimed = true, 0, 0
}

// traceGCSweepSpan traces the sweep of a single page.
//
// This may be called outside a traceGCSweepStart/traceGCSweepDone
// pair; however, it will not emit any trace events in this case.
func traceGCSweepSpan(bytesSwept uintptr) {
	_p_ := getg().m.p.ptr()
	if _p_.traceSweep {
		if _p_.traceSwept == 0 {
			traceEvent(traceEvGCSweepStart, 1)
		}
		_p_.traceSwept += bytesSwept
	}
}

func traceGCSweepDone() {
	_p_ := getg().m.p.ptr()
	if !_p_.traceSweep {
		throw("missing traceGCSweepStart")
	}
	if _p_.traceSwept != 0 {
		traceEvent(traceEvGCSweepDone, -1, uint64(_p_.traceSwept), uint64(_p_.traceReclaimed))
	}
	_p_.traceSweep = false
}

func traceGCMarkAssistStart() {
	traceEvent(traceEvGCMarkAssistStart, 1)
}

func traceGCMarkAssistDone() {
	traceEvent(traceEvGCMarkAssistDone, -1)
}

func traceGoCreate(newg *g, pc uintptr) {
	newg.traceseq = 0
	newg.tracelastp = getg().m.p
	// +PCQuantum because traceFrameForPC expects return PCs and subtracts PCQuantum.
	id := trace.stackTab.put([]uintptr{pc + sys.PCQuantum})
	traceEvent(traceEvGoCreate, 2, uint64(newg.goid), uint64(id))
}

func traceGoStart() {
	_g_ := getg().m.curg
	_p_ := _g_.m.p
	_g_.traceseq++
	if _p_.ptr().gcMarkWorkerMode != gcMarkWorkerNotWorker {
		traceEvent(traceEvGoStartLabel, -1, uint64(_g_.goid), _g_.traceseq, trace.markWorkerLabels[_p_.ptr().gcMarkWorkerMode])
	} else if _g_.tracelastp == _p_ {
		traceEvent(traceEvGoStartLocal, -1, uint64(_g_.goid))
	} else {
		_g_.tracelastp = _p_
		traceEvent(traceEvGoStart, -1, uint64(_g_.goid), _g_.traceseq)
	}
}

func traceGoEnd() {
	traceEvent(traceEvGoEnd, -1)
}

func traceGoSched() {
	_g_ := getg()
	_g_.tracelastp = _g_.m.p
	traceEvent(traceEvGoSched, 1)
}

func traceGoPreempt() {
	_g_ := getg()
	_g_.tracelastp = _g_.m.p
	traceEvent(traceEvGoPreempt, 1)
}

func traceGoPark(traceEv byte, skip int) {
	if traceEv&traceFutileWakeup != 0 {
		traceEvent(traceEvFutileWakeup, -1)
	}
	traceEvent(traceEv & ^traceFutileWakeup, skip)
}

func traceGoUnpark(gp *g, skip int) {
	_p_ := getg().m.p
	gp.traceseq++
	if gp.tracelastp == _p_ {
		traceEvent(traceEvGoUnblockLocal, skip, uint64(gp.goid))
	} else {
		gp.tracelastp = _p_
		traceEvent(traceEvGoUnblock, skip, uint64(gp.goid), gp.traceseq)
	}
}

// 注释：系统栈追踪
func traceGoSysCall() {
	traceEvent(traceEvGoSysCall, 1) // 注释：执行栈事件，事件常量是系统栈追踪
}

func traceGoSysExit(ts int64) {
	if ts != 0 && ts < trace.ticksStart {
		// There is a race between the code that initializes sysexitticks
		// (in exitsyscall, which runs without a P, and therefore is not
		// stopped with the rest of the world) and the code that initializes
		// a new trace. The recorded sysexitticks must therefore be treated
		// as "best effort". If they are valid for this trace, then great,
		// use them for greater accuracy. But if they're not valid for this
		// trace, assume that the trace was started after the actual syscall
		// exit (but before we actually managed to start the goroutine,
		// aka right now), and assign a fresh time stamp to keep the log consistent.
		ts = 0
	}
	_g_ := getg().m.curg
	_g_.traceseq++
	_g_.tracelastp = _g_.m.p
	traceEvent(traceEvGoSysExit, -1, uint64(_g_.goid), _g_.traceseq, uint64(ts)/traceTickDiv)
}

// 注释：系统调用停止时的栈追踪
func traceGoSysBlock(pp *p) {
	// Sysmon and stopTheWorld can declare syscalls running on remote Ps as blocked,
	// to handle this we temporary employ the P.
	// 注释：Sysmon和stopTheWorld可以将在远程P上运行的系统调用声明为阻塞，为了处理此问题，我们临时使用P。
	mp := acquirem()                  // 注释：获取当前P。临时使用
	oldp := mp.p                      // 注释：把P存储起来
	mp.p.set(pp)                      // 注释：把传入的P替换当前的P，后面栈事件使用
	traceEvent(traceEvGoSysBlock, -1) // 注释：栈事件,系统调用停止时的栈追踪
	mp.p = oldp                       //注释：还原原来的P
	releasem(mp)                      //注释：释放临时的P
}

func traceHeapAlloc() {
	traceEvent(traceEvHeapAlloc, -1, memstats.heap_live)
}

func traceNextGC() {
	if nextGC := atomic.Load64(&memstats.next_gc); nextGC == ^uint64(0) {
		// Heap-based triggering is disabled.
		traceEvent(traceEvNextGC, -1, 0)
	} else {
		traceEvent(traceEvNextGC, -1, nextGC)
	}
}

// To access runtime functions from runtime/trace.
// See runtime/trace/annotation.go

//go:linkname trace_userTaskCreate runtime/trace.userTaskCreate
func trace_userTaskCreate(id, parentID uint64, taskType string) {
	if !trace.enabled {
		return
	}

	// Same as in traceEvent.
	mp, pid, bufp := traceAcquireBuffer()
	if !trace.enabled && !mp.startingtrace {
		traceReleaseBuffer(pid)
		return
	}

	typeStringID, bufp := traceString(bufp, pid, taskType)
	traceEventLocked(0, mp, pid, bufp, traceEvUserTaskCreate, 3, id, parentID, typeStringID)
	traceReleaseBuffer(pid)
}

//go:linkname trace_userTaskEnd runtime/trace.userTaskEnd
func trace_userTaskEnd(id uint64) {
	traceEvent(traceEvUserTaskEnd, 2, id)
}

//go:linkname trace_userRegion runtime/trace.userRegion
func trace_userRegion(id, mode uint64, name string) {
	if !trace.enabled {
		return
	}

	mp, pid, bufp := traceAcquireBuffer()
	if !trace.enabled && !mp.startingtrace {
		traceReleaseBuffer(pid)
		return
	}

	nameStringID, bufp := traceString(bufp, pid, name)
	traceEventLocked(0, mp, pid, bufp, traceEvUserRegion, 3, id, mode, nameStringID)
	traceReleaseBuffer(pid)
}

//go:linkname trace_userLog runtime/trace.userLog
func trace_userLog(id uint64, category, message string) {
	if !trace.enabled {
		return
	}

	mp, pid, bufp := traceAcquireBuffer()
	if !trace.enabled && !mp.startingtrace {
		traceReleaseBuffer(pid)
		return
	}

	categoryID, bufp := traceString(bufp, pid, category)

	extraSpace := traceBytesPerNumber + len(message) // extraSpace for the value string
	traceEventLocked(extraSpace, mp, pid, bufp, traceEvUserLog, 3, id, categoryID)
	// traceEventLocked reserved extra space for val and len(val)
	// in buf, so buf now has room for the following.
	buf := bufp.ptr()

	// double-check the message and its length can fit.
	// Otherwise, truncate the message.
	slen := len(message)
	if room := len(buf.arr) - buf.pos; room < slen+traceBytesPerNumber {
		slen = room
	}
	buf.varint(uint64(slen))
	buf.pos += copy(buf.arr[buf.pos:], message[:slen])

	traceReleaseBuffer(pid)
}
