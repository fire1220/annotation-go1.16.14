// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import "unsafe"

// Should be a built-in for unsafe.Pointer?
//
//go:nosplit
func add(p unsafe.Pointer, x uintptr) unsafe.Pointer {
	return unsafe.Pointer(uintptr(p) + x)
}

// getg returns the pointer to the current g.
// The compiler rewrites calls to this function into instructions
// that fetch the g directly (from TLS or from the dedicated register).
// 注释：从TLS（Thread-local Storage线程本地存储）或从专用寄存器里拿数
// 注释：获取当前运行的g；执行位置是case ssa.OpAMD64LoweredGetG:
// 注释：getg返回指向当前g的指针。编译器将对此函数的调用重写为直接获取g的指令（来自TLS或来自专用寄存器）。
// 注释：要获取当前用户堆栈的g，可以使用getg().m.curg。
// 注释：getg()返回当前g，但是当在系统或信号堆栈上执行时，这将分别返回当前m的g0或gsignal。
// 注释：要确定g是在用户堆栈还是系统堆栈上运行，可以使用getg() == getg().m.curg，相等表示在用户态堆栈，不相等表示在系统堆栈。
func getg() *g

// mcall switches from the g to the g0 stack and invokes fn(g),
// where g is the goroutine that made the call.
// 注释：译：mcall从g切换到g0堆栈并调用fn（g），其中g是进行调用的goroutine。
// mcall saves g's current PC/SP in g->sched so that it can be restored later.
// It is up to fn to arrange for that later execution, typically by recording
// g in a data structure, causing something to call ready(g) later.
// 注释：译：mcall将g当前的PC/SP保存在g->sched中，以便以后可以恢复。这取决于fn来安排稍后的执行，通常是通过在数据结构中记录g，使某些东西稍后调用ready（g）。
// mcall returns to the original goroutine g later, when g has been rescheduled.
// fn must not return at all; typically it ends by calling schedule, to let the m
// run other goroutines.
// 注释：译：稍后，当g被重新安排时，mcall返回到原来的goroutine g。fn不得返回；通常它以调用schedule结束，让m运行其他goroutine。
//
// mcall can only be called from g stacks (not g0, not gsignal).
// 注释：译：只能从g堆栈调用mcall（不是g0，也不是gsignal）。
//
// This must NOT be go:noescape: if fn is a stack-allocated closure,
// fn puts g on a run queue, and g executes before fn returns, the
// closure will be invalidated while it is still executing.
// 注释：译：这一定不能去：noescape：如果fn是一个堆栈分配的闭包，fn将g放在运行队列中，并且g在fn返回之前执行，则闭包在执行时将无效。
//
// 注释：保存现场; 汇编函数是：TEXT runtime·mcall(SB), NOSPLIT, $0-8
func mcall(fn func(*g))

// systemstack runs fn on a system stack.
// If systemstack is called from the per-OS-thread (g0) stack, or
// if systemstack is called from the signal handling (gsignal) stack,
// systemstack calls fn directly and returns.
// Otherwise, systemstack is being called from the limited stack
// of an ordinary goroutine. In this case, systemstack switches
// to the per-OS-thread stack, calls fn, and switches back.
// It is common to use a func literal as the argument, in order
// to share inputs and outputs with the code around the call
// to system stack:
//
//	... set up y ...
//	systemstack(func() {
//		x = bigcall(y)
//	})
//	... use x ...
//
// 注释：切换到系统堆栈（系统堆栈指的就是g0，有独立的8M栈空间，负责调度G），汇编位置：TEXT runtime·systemstack(SB), NOSPLIT, $0-8
//
//go:noescape
func systemstack(fn func()) // 注释：切换系统栈执行

var badsystemstackMsg = "fatal: systemstack called from unexpected goroutine"

//go:nosplit
//go:nowritebarrierrec
func badsystemstack() {
	sp := stringStructOf(&badsystemstackMsg)
	write(2, sp.str, int32(sp.len))
}

// memclrNoHeapPointers clears n bytes starting at ptr.
//
// Usually you should use typedmemclr. memclrNoHeapPointers should be
// used only when the caller knows that *ptr contains no heap pointers
// because either:
//
// *ptr is initialized memory and its type is pointer-free, or
//
// *ptr is uninitialized memory (e.g., memory that's being reused
// for a new allocation) and hence contains only "junk".
//
// memclrNoHeapPointers ensures that if ptr is pointer-aligned, and n
// is a multiple of the pointer size, then any pointer-aligned,
// pointer-sized portion is cleared atomically. Despite the function
// name, this is necessary because this function is the underlying
// implementation of typedmemclr and memclrHasPointers. See the doc of
// memmove for more details.
//
// The (CPU-specific) implementations of this function are in memclr_*.s.
//
//go:noescape
func memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr) // 注释：0填充ptr指针向后n个字节，初始化内存（清空内存，用于申请后的0填充动作，汇编实现）

//go:linkname reflect_memclrNoHeapPointers reflect.memclrNoHeapPointers
func reflect_memclrNoHeapPointers(ptr unsafe.Pointer, n uintptr) {
	memclrNoHeapPointers(ptr, n)
}

// memmove copies n bytes from "from" to "to".
//
// memmove ensures that any pointer in "from" is written to "to" with
// an indivisible write, so that racy reads cannot observe a
// half-written pointer. This is necessary to prevent the garbage
// collector from observing invalid pointers, and differs from memmove
// in unmanaged languages. However, memmove is only required to do
// this if "from" and "to" may contain pointers, which can only be the
// case if "from", "to", and "n" are all be word-aligned.
//
// Implementations are in memmove_*.s.
//
// 注释：将n个字节从“from”复制到“to”
//
//go:noescape
func memmove(to, from unsafe.Pointer, n uintptr)

//go:linkname reflect_memmove reflect.memmove
func reflect_memmove(to, from unsafe.Pointer, n uintptr) {
	memmove(to, from, n)
}

// exported value for testing
var hashLoad = float32(loadFactorNum) / float32(loadFactorDen)

// 注释：快速返回随机数
//
//go:nosplit
func fastrand() uint32 {
	mp := getg().m
	// Implement xorshift64+: 2 32-bit xorshift sequences added together.
	// Shift triplet [17,7,16] was calculated as indicated in Marsaglia's
	// Xorshift paper: https://www.jstatsoft.org/article/view/v008i14/xorshift.pdf
	// This generator passes the SmallCrush suite, part of TestU01 framework:
	// http://simul.iro.umontreal.ca/testu01/tu01.html
	s1, s0 := mp.fastrand[0], mp.fastrand[1] // 注释：获取两个值
	s1 ^= s1 << 17                           // 注释：向右移动17位之后亦或(扩大，利用亦或加密)
	s1 = s1 ^ s0 ^ s1>>7 ^ s0>>16            // 注释：多次亦或处理（缩小，利用亦或加密）
	mp.fastrand[0], mp.fastrand[1] = s0, s1  // 注释：重新定义这两个值
	return s0 + s1                           // 注释：返回两个数的和
}

//go:nosplit
func fastrandn(n uint32) uint32 {
	// This is similar to fastrand() % n, but faster.
	// See https://lemire.me/blog/2016/06/27/a-fast-alternative-to-the-modulo-reduction/
	return uint32(uint64(fastrand()) * uint64(n) >> 32)
}

//go:linkname sync_fastrand sync.fastrand
func sync_fastrand() uint32 { return fastrand() }

//go:linkname net_fastrand net.fastrand
func net_fastrand() uint32 { return fastrand() }

//go:linkname os_fastrand os.fastrand
func os_fastrand() uint32 { return fastrand() }

// in internal/bytealg/equal_*.s
//
//go:noescape
func memequal(a, b unsafe.Pointer, size uintptr) bool

// noescape hides a pointer from escape analysis.  noescape is
// the identity function but escape analysis doesn't think the
// output depends on the input.  noescape is inlined and currently
// compiles down to zero instructions.
// 注释：翻译：noescape对escape分析隐藏了一个指针。noescape是身份函数，但escape分析不认为输出取决于输入。noescape是内联的，目前编译为零指令。
// USE CAREFULLY! // 注释：小心使用！
//
//go:nosplit
func noescape(p unsafe.Pointer) unsafe.Pointer {
	x := uintptr(p)
	return unsafe.Pointer(x ^ 0)
}

// Not all cgocallback frames are actually cgocallback,
// so not all have these arguments. Mark them uintptr so that the GC
// does not misinterpret memory when the arguments are not present.
// cgocallback is not called from Go, only from crosscall2.
// This in turn calls cgocallbackg, which is where we'll find
// pointer-declared arguments.
func cgocallback(fn, frame, ctxt uintptr)

// 注释：调用汇编函数：TEXT runtime·gogo(SB), NOSPLIT, $16-8
func gogo(buf *gobuf) // 注释：从gobuf恢复状态并执行
func gosave(buf *gobuf)

//go:noescape
func jmpdefer(fv *funcval, argp uintptr)
func asminit()
func setg(gg *g)
func breakpoint()

// reflectcall calls fn with a copy of the n argument bytes pointed at by arg. // 注释：reflectcall使用arg指向的n个参数字节的副本调用fn。
// After fn returns, reflectcall copies n-retoffset result bytes
// back into arg+retoffset before returning. If copying result bytes back,
// the caller should pass the argument frame type as argtype, so that
// call can execute appropriate write barriers during the copy.
// 注释：在fn返回后，reflectcall在返回之前将n-retoffset结果字节复制回arg+retoffset。如果复制结果字节，调用方应将参数帧类型作为argtype传递，以便调用可以在复制期间执行适当的写屏障。
//
// Package reflect always passes a frame type. In package runtime,
// Windows callbacks are the only use of this that copies results
// back, and those cannot have pointers in their results, so runtime
// passes nil for the frame type.
// 注释：包反射总是通过一个帧类型。在包运行时中，Windows回调是将结果复制回的唯一用途，并且这些回调的结果中不能有指针，因此运行时为帧类型传递nil。
//
// Package reflect accesses this symbol through a linkname. // 注释：包反射通过链接名访问此符号。
// 注释：这里会调用d.fn函数，就是refer里执行的函数
func reflectcall(argtype *_type, fn, arg unsafe.Pointer, argsize uint32, retoffset uint32)

func procyield(cycles uint32)

type neverCallThisFunction struct{}

// goexit is the return stub at the top of every goroutine call stack.
// Each goroutine stack is constructed as if goexit called the
// goroutine's entry point function, so that when the entry point
// function returns, it will return to goexit, which will call goexit1
// to perform the actual exit.
//
// This function must never be called directly. Call goexit1 instead.
// gentraceback assumes that goexit terminates the stack. A direct
// call on the stack will cause gentraceback to stop walking the stack
// prematurely and if there is leftover state it may panic.
func goexit(neverCallThisFunction)

// publicationBarrier performs a store/store barrier (a "publication"
// or "export" barrier). Some form of synchronization is required
// between initializing an object and making that object accessible to
// another processor. Without synchronization, the initialization
// writes and the "publication" write may be reordered, allowing the
// other processor to follow the pointer and observe an uninitialized
// object. In general, higher-level synchronization should be used,
// such as locking or an atomic pointer write. publicationBarrier is
// for when those aren't an option, such as in the implementation of
// the memory manager.
//
// There's no corresponding barrier for the read side because the read
// side naturally has a data dependency order. All architectures that
// Go supports or seems likely to ever support automatically enforce
// data dependency ordering.
func publicationBarrier()

// getcallerpc returns the program counter (PC) of its caller's caller.
// 注释：getcallerrpc返回其调用者的程序计数器（PC）。
// getcallersp returns the stack pointer (SP) of its caller's caller.
// 注释：getcallersp返回其调用者的调用者的堆栈指针（SP）。
// The implementation may be a compiler intrinsic; there is not
// necessarily code implementing this on every platform.
// 注释：该实现可以是编译器内部的；不一定有代码在每个平台上实现这一点。
//
// For example:
//
//	func f(arg1, arg2, arg3 int) {
//		pc := getcallerpc()
//		sp := getcallersp()
//	}
//
// These two lines find the PC and SP immediately following
// the call to f (where f will return).
// 注释：这两行在调用f（f将返回）之后立即找到PC和SP。
//
// The call to getcallerpc and getcallersp must be done in the
// frame being asked about.
// 注释：对getcallerrpc和getcallersp的调用必须在被询问的帧中完成。
//
// The result of getcallersp is correct at the time of the return,
// but it may be invalidated by any subsequent call to a function
// that might relocate the stack in order to grow or shrink it.
// 注释：getcallersp的结果在返回时是正确的，但它可能会因随后对函数的任何调用而无效，该函数可能会重新定位堆栈以扩大或缩小堆栈。
// A general rule is that the result of getcallersp should be used
// immediately and can only be passed to nosplit functions.
// 注释：一般规则是，getcallersp的结果应该立即使用，并且只能传递给nosplit函数

// 注释：返回对所在函数的调用，上面案例中有解释
// 注释：获取 caller(呼叫者，上游的函数)的PC（伪）指令寄存器，对应硬件IP寄存器
//
//go:noescape
func getcallerpc() uintptr

// 注释：返回对所在函数的返回，上面案例中有解释
// 注释：获取 caller(呼叫者，上游的函数)的SP（伪）寄存器，对应硬件BP寄存器（函数栈帧底部）
//
//go:noescape
func getcallersp() uintptr // implemented as an intrinsic on all platforms

// getclosureptr returns the pointer to the current closure.
// getclosureptr can only be used in an assignment statement
// at the entry of a function. Moreover, go:nosplit directive
// must be specified at the declaration of caller function,
// so that the function prolog does not clobber the closure register.
// for example:
//
//	//go:nosplit
//	func f(arg1, arg2, arg3 int) {
//		dx := getclosureptr()
//	}
//
// The compiler rewrites calls to this function into instructions that fetch the
// pointer from a well-known register (DX on x86 architecture, etc.) directly.
func getclosureptr() uintptr

//go:noescape
func asmcgocall(fn, arg unsafe.Pointer) int32

func morestack()
func morestack_noctxt()
func rt0_go()

// return0 is a stub used to return 0 from deferproc.
// It is called at the very end of deferproc to signal
// the calling Go function that it should not jump
// to deferreturn.
// in asm_*.s
func return0()

// in asm_*.s
// not called directly; definitions here supply type information for traceback.
func call16(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call32(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call64(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call128(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call256(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call512(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1024(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call2048(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call4096(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call8192(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call16384(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call32768(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call65536(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call131072(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call262144(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call524288(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1048576(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call2097152(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call4194304(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call8388608(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call16777216(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call33554432(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call67108864(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call134217728(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call268435456(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call536870912(typ, fn, arg unsafe.Pointer, n, retoffset uint32)
func call1073741824(typ, fn, arg unsafe.Pointer, n, retoffset uint32)

func systemstack_switch()

// alignUp rounds n up to a multiple of a. a must be a power of 2.
// 注释：alignUp将n向上取整到a的倍数。a必须是2的幂。
// 注释：(常用于内存对齐【(n + 7)&^ 7)】向上取整a倍数
func alignUp(n, a uintptr) uintptr {
	return (n + a - 1) &^ (a - 1)
}

// alignDown rounds n down to a multiple of a. a must be a power of 2.
// 注释：向下取a整
func alignDown(n, a uintptr) uintptr {
	return n &^ (a - 1)
}

// divRoundUp returns ceil(n / a).
// 注释：向上取a整
func divRoundUp(n, a uintptr) uintptr {
	// a is generally a power of two. This will get inlined and
	// the compiler will optimize the division.
	return (n + a - 1) / a
}

// checkASM reports whether assembly runtime checks have passed.
func checkASM() bool

func memequal_varlen(a, b unsafe.Pointer) bool

// bool2int returns 0 if x is false or 1 if x is true.
func bool2int(x bool) int {
	// Avoid branches. In the SSA compiler, this compiles to
	// exactly what you would want it to.
	return int(uint8(*(*uint8)(unsafe.Pointer(&x))))
}

// abort crashes the runtime in situations where even throw might not
// work. In general it should do something a debugger will recognize
// (e.g., an INT3 on x86). A crash in abort is recognized by the
// signal handler, which will attempt to tear down the runtime
// immediately.
func abort()

// Called from compiled code; declared for vet; do NOT call from Go.
func gcWriteBarrier()
func duffzero()
func duffcopy()

// Called from linker-generated .initarray; declared for go vet; do NOT call from Go.
func addmoduledata()
