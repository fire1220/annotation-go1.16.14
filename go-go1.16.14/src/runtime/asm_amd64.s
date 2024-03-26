// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "go_asm.h"
#include "go_tls.h"
#include "funcdata.h"
#include "textflag.h"

// _rt0_amd64 is common startup code for most amd64 systems when using
// internal linking. This is the entry point for the program from the
// kernel for an ordinary -buildmode=exe program. The stack holds the
// number of arguments and the C-style argv.
// 注释：程序真正的入口
TEXT _rt0_amd64(SB),NOSPLIT,$-8
	MOVQ	0(SP), DI	// 注释：把命令行参数个数放到寄存器DI里 // argc
	LEAQ	8(SP), SI	// 注释：把命令行参数放到寄存器SI里    // argv
	JMP	runtime·rt0_go(SB) // 注释：调用runtime·rt0_go函数（汇编语言实现的）

// main is common startup code for most amd64 systems when using
// external linking. The C startup code will call the symbol "main"
// passing argc and argv in the usual C ABI registers DI and SI.
TEXT main(SB),NOSPLIT,$-8
	JMP	runtime·rt0_go(SB)

// _rt0_amd64_lib is common startup code for most amd64 systems when
// using -buildmode=c-archive or -buildmode=c-shared. The linker will
// arrange to invoke this function as a global constructor (for
// c-archive) or when the shared library is loaded (for c-shared).
// We expect argc and argv to be passed in the usual C ABI registers
// DI and SI.
TEXT _rt0_amd64_lib(SB),NOSPLIT,$0x50
	// Align stack per ELF ABI requirements.
	MOVQ	SP, AX
	ANDQ	$~15, SP
	// Save C ABI callee-saved registers, as caller may need them.
	MOVQ	BX, 0x10(SP)
	MOVQ	BP, 0x18(SP)
	MOVQ	R12, 0x20(SP)
	MOVQ	R13, 0x28(SP)
	MOVQ	R14, 0x30(SP)
	MOVQ	R15, 0x38(SP)
	MOVQ	AX, 0x40(SP)

	MOVQ	DI, _rt0_amd64_lib_argc<>(SB)
	MOVQ	SI, _rt0_amd64_lib_argv<>(SB)

	// Synchronous initialization.
	CALL	runtime·libpreinit(SB)

	// Create a new thread to finish Go runtime initialization.
	MOVQ	_cgo_sys_thread_create(SB), AX
	TESTQ	AX, AX
	JZ	nocgo
	MOVQ	$_rt0_amd64_lib_go(SB), DI
	MOVQ	$0, SI
	CALL	AX
	JMP	restore

nocgo:
	MOVQ	$0x800000, 0(SP)		// stacksize
	MOVQ	$_rt0_amd64_lib_go(SB), AX
	MOVQ	AX, 8(SP)			// fn
	CALL	runtime·newosproc0(SB)

restore:
	MOVQ	0x10(SP), BX
	MOVQ	0x18(SP), BP
	MOVQ	0x20(SP), R12
	MOVQ	0x28(SP), R13
	MOVQ	0x30(SP), R14
	MOVQ	0x38(SP), R15
	MOVQ	0x40(SP), SP
	RET

// _rt0_amd64_lib_go initializes the Go runtime.
// This is started in a separate thread by _rt0_amd64_lib.
TEXT _rt0_amd64_lib_go(SB),NOSPLIT,$0
	MOVQ	_rt0_amd64_lib_argc<>(SB), DI
	MOVQ	_rt0_amd64_lib_argv<>(SB), SI
	JMP	runtime·rt0_go(SB)

DATA _rt0_amd64_lib_argc<>(SB)/8, $0
GLOBL _rt0_amd64_lib_argc<>(SB),NOPTR, $8
DATA _rt0_amd64_lib_argv<>(SB)/8, $0
GLOBL _rt0_amd64_lib_argv<>(SB),NOPTR, $8

// Defined as ABIInternal since it does not use the stack-based Go ABI (and
// in addition there are no calls to this entry point from Go code).
// 注释：程序入口函数
TEXT runtime·rt0_go<ABIInternal>(SB),NOSPLIT,$0
	// copy arguments forward on an even stack
	// 注释：AMD64使用rdi和rsi传入参数，rid:argc, rsi:argv
	MOVQ	DI, AX		    // 注释：DI寄存器传入参数argc // argc
	MOVQ	SI, BX		    // 注释：SI寄存器传入参数argv // argv
	SUBQ	$(4*8+7), SP	// 注释：开辟参数的空间	// 2args 2auto
	ANDQ	$~15, SP        // 注释：参数空间内存对齐，(~15&SP）清空后4位，16位对齐
	MOVQ	AX, 16(SP)      // 注释：把参数argc放到栈上
	MOVQ	BX, 24(SP)      // 注释：把参数agrv放到栈上

	// create istack out of the given (operating system) stack.
	// _cgo_init may update stackguard.
	MOVQ	$runtime·g0(SB), DI         // 注释：把g0放到DI寄存器里
	LEAQ	(-64*1024+104)(SP), BX      // 注释：设置栈顶；开辟空间空间大小是（64*1024-104）,就是(64k-104字节)
	MOVQ	BX, g_stackguard0(DI)       // 注释：(设置爆栈警戒线)栈顶地址就是爆栈警戒的地址
	MOVQ	BX, g_stackguard1(DI)       // 注释：(设置C语言的爆栈警戒线)
	MOVQ	BX, (g_stack+stack_lo)(DI)  // 注释：设置栈顶位置（g.stack.lo）底地址
	MOVQ	SP, (g_stack+stack_hi)(DI)  // 注释：设置栈底位置（g.stack.hi）高地址

	// find out information about the processor we're on
	MOVL	$0, AX // 注释：清空寄存器AX
	CPUID          // 注释：获取CPU的ID，放到AX寄存器里
	MOVL	AX, SI // 注释：把CPU的ID放到SI寄存器里
	CMPL	AX, $0 // 注释：比较AX是否等于零
	JE	nocpuinfo  // 注释：如果AX（CPU的ID）如果等于0说明没有获取到CUP的ID，则跳转到nocpuinfo

	// Figure out how to serialize RDTSC.
	// On Intel processors LFENCE is enough. AMD requires MFENCE.
	// Don't know about the rest, so let's do MFENCE.
	CMPL	BX, $0x756E6547  // "Genu"
	JNE	notintel
	CMPL	DX, $0x49656E69  // "ineI"
	JNE	notintel
	CMPL	CX, $0x6C65746E  // "ntel"
	JNE	notintel
	MOVB	$1, runtime·isIntel(SB)
	MOVB	$1, runtime·lfenceBeforeRdtsc(SB)
notintel:

	// Load EAX=1 cpuid flags
	MOVL	$1, AX
	CPUID
	MOVL	AX, runtime·processorVersionInfo(SB)

nocpuinfo: // 注释：没有获取到CPU的ID是执行
	// if there is an _cgo_init, call it.
	MOVQ	_cgo_init(SB), AX
	TESTQ	AX, AX
	JZ	needtls
	// arg 1: g0, already in DI
	MOVQ	$setg_gcc<>(SB), SI // arg 2: setg_gcc
#ifdef GOOS_android
	MOVQ	$runtime·tls_g(SB), DX 	// arg 3: &tls_g
	// arg 4: TLS base, stored in slot 0 (Android's TLS_SLOT_SELF).
	// Compensate for tls_g (+16).
	MOVQ	-16(TLS), CX
#else
	MOVQ	$0, DX	// arg 3, 4: not used when using platform's TLS
	MOVQ	$0, CX
#endif
#ifdef GOOS_windows
	// Adjust for the Win64 calling convention.
	MOVQ	CX, R9 // arg 4
	MOVQ	DX, R8 // arg 3
	MOVQ	SI, DX // arg 2
	MOVQ	DI, CX // arg 1
#endif
	CALL	AX

	// update stackguard after _cgo_init
	MOVQ	$runtime·g0(SB), CX
	MOVQ	(g_stack+stack_lo)(CX), AX
	ADDQ	$const__StackGuard, AX
	MOVQ	AX, g_stackguard0(CX)
	MOVQ	AX, g_stackguard1(CX)

#ifndef GOOS_windows
	JMP ok
#endif
needtls:
#ifdef GOOS_plan9
	// skip TLS setup on Plan 9
	JMP ok
#endif
#ifdef GOOS_solaris
	// skip TLS setup on Solaris
	JMP ok
#endif
#ifdef GOOS_illumos
	// skip TLS setup on illumos
	JMP ok
#endif
#ifdef GOOS_darwin
	// skip TLS setup on Darwin
	JMP ok
#endif
#ifdef GOOS_openbsd
	// skip TLS setup on OpenBSD
	JMP ok
#endif

	LEAQ	runtime·m0+m_tls(SB), DI
	CALL	runtime·settls(SB)

	// store through it, to make sure it works
	get_tls(BX)
	MOVQ	$0x123, g(BX)
	MOVQ	runtime·m0+m_tls(SB), AX
	CMPQ	AX, $0x123
	JEQ 2(PC)
	CALL	runtime·abort(SB)
ok:
    // 注释：程序刚启动的时候必定有一个线程启动（主线程）
    // 注释：将当前的栈和资源保存在g0
    // 注释：将该线程保存在m0
    // 注释：tls: Thread Local Storage
	// set the per-goroutine and per-mach "registers"
	get_tls(BX) // 注释：获取TLS数据，里面存储的是G的指针(宏定义位置：src/runtime/go_tls.h)(宏定义内容：#define	get_tls(r)	MOVQ TLS, r)
	LEAQ	runtime·g0(SB), CX // 注释：把g0指针放到CX寄存器里
	MOVQ	CX, g(BX)          // 注释：（把g0放到当前运行的G里）把g0放到TLS（线程运行的G上面）里
	LEAQ	runtime·m0(SB), AX // 注释：把m0放到AX寄存器里

    // 注释：m0和g0互相绑定
	// save m->g0 = g0
	MOVQ	CX, m_g0(AX) // 注释：m0.g0 = g0（g0绑定到m0上）
	// save m0 to g0->m
	MOVQ	AX, g_m(CX)  // 注释：g0.m = m0（m0绑定到g0上）

	CLD				// convention is D is always left cleared
	CALL	runtime·check(SB)

	MOVL	16(SP), AX		      // 注释：(复制数据argc)这里是吧16SP位置数据（argc）放到AX寄存器上 // copy argc
	MOVL	AX, 0(SP)             // 注释：(复制数据argc)把argc数据放到0SP位置上
	MOVQ	24(SP), AX		      // 注释：(复制数据argv)把24SP数据（argv）放到AX寄存器上 // copy argv
	MOVQ	AX, 8(SP)             // 注释：(复制数据argv)把argv数据放到8SP位置上
	CALL	runtime·args(SB)      // 注释：处理args
	CALL	runtime·osinit(SB)    // 注释：os初始化(os_linux.go)
	CALL	runtime·schedinit(SB) // 注释：调度系统初始化(proc.go)(会进行p的初始化，也会把m0和某个p绑定)

    // 注释：创建一个goroutine，然后开启执行程序
	// create a new goroutine to start program
	MOVQ	$runtime·mainPC(SB), AX	 // 注释：(runtime.main函数指针放到AX里)访问全局变量runtime·mainPC，内容是runtime.main函数指针	// entry
	PUSHQ	AX                       // 注释：参数2入栈(值是&runtime.main)，把AX作为下个函数第2个参数，相当于subq $8,SP；movq AX,SP
	PUSHQ	$0                       // 注释：参数1入栈，把0作为下个函数第1个参数，相当于subq $8,SP；movq $0,SP		// arg size
	CALL	runtime·newproc(SB)      // 注释：(此时还没有执行只是添加队列)新建一个g，也叫main goroutine，它的任务函数是runtime.main函数，建好后插入m0绑定的p的本地队列
	POPQ	AX                       // 注释：出栈，去除上面的参数位置
	POPQ	AX                       // 注释：出栈，去除上面的参数位置

	// start this M
	CALL	runtime·mstart(SB)       // 注释：启动线程m，进入启动调度系统（开始执行调度和业务代码）

	CALL	runtime·abort(SB)	// mstart should never return
	RET

	// Prevent dead-code elimination of debugCallV1, which is
	// intended to be called by debuggers.
	MOVQ	$runtime·debugCallV1<ABIInternal>(SB), AX
	RET

// mainPC is a function value for runtime.main, to be passed to newproc.
// The reference to runtime.main is made via ABIInternal, since the
// actual function (not the ABI0 wrapper) is needed by newproc.
// 注释：译：mainPC是runtime.main的函数值，将传递给newproc。对runtime.main的引用是通过ABInternal进行的，因为newproc需要实际的函数（而不是ABI0包装器）。
// 注释：对来自汇编的闭包调用使用ABIInteral
DATA	runtime·mainPC+0(SB)/8,$runtime·main<ABIInternal>(SB) // 注释：runtime·mainPC是runtime.main的函数指针【mainPC=&fn{fn:runtime.main}】
GLOBL	runtime·mainPC(SB),RODATA,$8

TEXT runtime·breakpoint(SB),NOSPLIT,$0-0
	BYTE	$0xcc
	RET

TEXT runtime·asminit(SB),NOSPLIT,$0-0
	// No per-thread init.
	RET

/*
 *  go-routine
 */

// func gosave(buf *gobuf)
// save state in Gobuf; setjmp
// 注释：gosave保存现场，把寄存器里的数据保存的g.sched里，类型是gobuf
TEXT runtime·gosave(SB), NOSPLIT, $0-8
	MOVQ	buf+0(FP), AX		// 注释：获取入参*gobuf到寄存器AX里 // gobuf
	LEAQ	buf+0(FP), BX		// 注释：获取SP:存放入参的地址（就是当前函数的FP，调用放函数的SP）址放到BX寄存器里 // caller's SP
	MOVQ	BX, gobuf_sp(AX)    // 注释：(保存SP)把SP（FP的存放地址）的值放到gobuf.sp里
	MOVQ	0(SP), BX		    // 注释：获取PC:把SP（基站地址，程序开始位置，PC指令计数器存放下次要执行的指令）存放到临时寄存器BX里 // caller's PC
	MOVQ	BX, gobuf_pc(AX)    // 注释：(保存PC)把PC指令寄存器保存到gobuf.pc里
	MOVQ	$0, gobuf_ret(AX)   // 注释：初始化返回值，清空返回值位置gobuf.ret
	MOVQ	BP, gobuf_bp(AX)    // 注释：(保存BP)把BP保存到gobuf.bp里
	// Assert ctxt is zero. See func save.
	MOVQ	gobuf_ctxt(AX), BX
	TESTQ	BX, BX // 注释：比较；逻辑与（&）运算，TEMP等于0时设置ZF为1；不等于0时设置ZF为0（ZF是条件寄存器）
	JZ	2(PC)      // 注释：如果相等则跳转2(PC)执行指令
	CALL	runtime·badctxt(SB) // 注释：调用函数runtime·badctxt
	get_tls(CX)                 // 注释：获取TLS（*g）(宏定义位置：src/runtime/go_tls.h)(宏定义内容：#define	get_tls(r)	MOVQ TLS, r)
	MOVQ	g(CX), BX           // 注释：把G指针放到寄存器BX里
	MOVQ	BX, gobuf_g(AX)     // 注释：(保存G指针)把G指针存储到gobuf.g里
	RET

// func gogo(buf *gobuf)
// restore state from Gobuf; longjmp
// 注释：从Gobuf恢复状态并执行
// 注释：执行gogo函数指令，执行gobuf（之前保存的）中的数据(g.sched类型是gobuf)
// 注释：执行G协成
TEXT runtime·gogo(SB), NOSPLIT, $16-8
	MOVQ	buf+0(FP), BX		// 注释：获取入参的gobuf结构体放到BX寄存器中 // gobuf
	MOVQ	gobuf_g(BX), DX     // 注释：取出G放到DX寄存器中
	MOVQ	0(DX), CX		    // 注释：初始化CX寄存器（DX数据放到寄存器CX中）(保证G不为空) // make sure g != nil
	get_tls(CX) // 注释：取出TLS中的数据放到CX寄存器中，TLS存放了G的指针。(宏定义位置：src/runtime/go_tls.h)(宏定义内容：#define	get_tls(r)	MOVQ TLS, r)
	MOVQ	DX, g(CX)           // 注释：把G放到TLS的G指针中（getg函数请求的就是TLS中的G指针）
	MOVQ	gobuf_sp(BX), SP	// 注释：将runtime.goexit函数的PC恢复到SP中; 恢复g.sched.sp到SP寄存器中 // restore SP
	MOVQ	gobuf_ret(BX), AX   // 注释：恢复g.sched.ret返回值位置到寄存器AX中
	MOVQ	gobuf_ctxt(BX), DX  // 注释：恢复g.sched.ctxt到DX中
	MOVQ	gobuf_bp(BX), BP    // 注释：恢复g.sched.bp到寄存器BP中
	MOVQ	$0, gobuf_sp(BX)	// 注释：清空g.sched.sp数据 // clear to help garbage collector
	MOVQ	$0, gobuf_ret(BX)   // 注释：清空g.sched.ret数据
	MOVQ	$0, gobuf_ctxt(BX)  // 注释：清空g.sched.ctxt数据
	MOVQ	$0, gobuf_bp(BX)    // 注释：清空g.sched.bp数据
	MOVQ	gobuf_pc(BX), BX    // 注释：恢复g.sched.pc到BX中(指令计数器)
	JMP	BX                      // 注释：执行BX中位置指令

// func mcall(fn func(*g))
// Switch to m->g0's stack, call fn(g).
// Fn must never return. It should gogo(&g->sched)
// to keep running g.
// 注释：译：func mcall（fn func（*g））切换到m->g0的堆栈，调用fn（g）。Fn决不能再回来。它应该gogo（&g->sched）继续运行g。
//
// 注释：执行 runtime.mcall 函数的汇编函数
// 注释：每个M下都会保存一个g0，是自己独立的栈空间(系统线程栈空间)。
// 注释：步骤：
//      1.获取业务G（tls里的G）
//      2.保存现场
//      3.切换g0
//      4.把业务G压入栈中，作为fn的入参
//      5.执行fn，用g0执行fn，(业务G是入参),永不返回（需要执行下一次系统调度或休息）
TEXT runtime·mcall(SB), NOSPLIT, $0-8
	MOVQ	fn+0(FP), DI                    // 注释：(参数fn地址,闭包函数指令地址)第一个参数是闭包函数(存储的时候闭包函数指令的指针)

    // 注释：保存现场
	get_tls(CX)                             // 注释：获取TLS的G地址，放到CX寄存器里(里第一个地址就是G地址)
	MOVQ	g(CX), AX	                    // 注释：把业务G指针放到AX里 // save state in g->sched
	MOVQ	0(SP), BX	                    // 注释：(要执行的PC)硬件寄存器栈指针(栈顶,低地址)(存储当前PC值) // caller's PC
	MOVQ	BX, (g_sched+gobuf_pc)(AX)      // 注释：保存栈顶，低地址位置(硬件寄存器SP)
	LEAQ	fn+0(FP), BX	                // 注释：(参数fn地址)第1个参数(闭包函数) // caller's SP
	MOVQ	BX, (g_sched+gobuf_sp)(AX)      // 注释：保存第1个参数（闭包函数）
	MOVQ	AX, (g_sched+gobuf_g)(AX)       // 注释：保存G
	MOVQ	BP, (g_sched+gobuf_bp)(AX)      // 注释：保存BP基地址（栈低,高地址）

	// switch to m->g0 & its stack, call fn
	MOVQ	g(CX), BX                       // 注释：当前业务G
	MOVQ	g_m(BX), BX                     // 注释：当前业务G对应的M
	MOVQ	m_g0(BX), SI                    // 注释：G0
	CMPQ	SI, AX	                        // 注释：如果G0 == 当前业务G 则报错 // if g == m->g0 call badmcall
	JNE	3(PC)                               // 注释：如果不相等则跳转到下面第三行
	MOVQ	$runtime·badmcall(SB), AX       // 注释：如果相等则报错（把报错的指令函数地址放到AX里，后面执行）
	JMP	AX                                  // 注释：如果相等则报错
	MOVQ	SI, g(CX)	                    // 注释：切换到G0 // g = m->g0
	MOVQ	(g_sched+gobuf_sp)(SI), SP	    // 注释：把G0里的SP拿出来 // sp = m->g0->sched.sp
	PUSHQ	AX                              // 注释：(入栈)把业务G压入到栈里(AX是业务G指针)(这里把业务G指针作为fn的参数)
	MOVQ	DI, DX                          // 注释：把参数fn地址放到DX里
	MOVQ	0(DI), DI                       // 注释：(拿出fn函数指令)闭包函数指令
	CALL	DI                              // 注释：执行fn闭包函数(永不返回，如果返回了则报错了)
	POPQ	AX                              // 注释：(出栈)从栈中踢出业务G指针
	MOVQ	$runtime·badmcall2(SB), AX      // 注释：报错信息放到AX里(报错了)
	JMP	AX                                  // 注释：跳转到报错(报错了)
	RET                                     // 注释：程序退出

// systemstack_switch is a dummy routine that systemstack leaves at the bottom
// of the G stack. We need to distinguish the routine that
// lives at the bottom of the G stack from the one that lives
// at the top of the system stack because the one at the top of
// the system stack terminates the stack walk (see topofstack()).
TEXT runtime·systemstack_switch(SB), NOSPLIT, $0-0
	RET

// func systemstack(fn func())
// 注释：切换到系统堆栈执行闭包函数（系统堆栈指的就是g0，有独立的栈空间，就是线程栈空间，负责调度G）
// 注释：步骤：
//      1.获取M下的G0（每个M下都有自己的G0）
//      2.保存现场（把业务G的PC、SP、G、BP保存起来）
//      3.切换G0（把G0替换业务G（TLS））
//      4.执行fn闭包函数（此时是用G0执行的）
//      5.切换回业务G(把业务G放到TLS中)
//      6.恢复SP栈基指针（把业务的sp放回来）(业务G的参数赋值到栈的SP上)(SP存放函数参数访问头位置（基指针，栈低）)
//      7.清楚备份的SP
//      8.程序退出后继续执行后面逻辑时取出的g(get_tls)就是业务G，也就是说会继续执行业务G的内容
TEXT runtime·systemstack(SB), NOSPLIT, $0-8
	MOVQ	fn+0(FP), DI	                    // 注释：获取一个入参（闭包函数的地址） // DI = fn
	get_tls(CX)                                 // 注释：获取m的tls数据（线程的本地私有数据）(第一个元素就是存放G的地址)(程序当前运行的g会到这里找)
	MOVQ	g(CX), AX	                        // 注释：(G)获取g结构体 // AX = g
	MOVQ	g_m(AX), BX	                        // 注释：(M)获取g.m // BX = m

	CMPQ	AX, m_gsignal(BX)                   // 注释：比较g和g.m.gsignal
	JEQ	noswitch                                // 注释：如果相等跳转(执行)noswitch，不触发系统切换

	MOVQ	m_g0(BX), DX	                    // 注释：(g0)把g.m.g0 复制到DX里 // DX = g0
	CMPQ	AX, DX                              // 注释：比较g和g.m.g0
	JEQ	noswitch                                // 注释：如果相等跳转(执行)noswitch，不触发系统切换

	CMPQ	AX, m_curg(BX)                      // 注释：比较g和g.m.curg(当前的g)
	JNE	bad                                     // 注释：如果不相等跳转(执行)bad，异常

	// switch stacks
	// save our state in g->sched. Pretend to
	// be systemstack_switch if the G stack is scanned.
	// 注释：译：切换堆栈，将我们的状态保存在g->sched中。如果扫描G堆栈，则假装为systemstack_switch。
	MOVQ	$runtime·systemstack_switch(SB), SI // 注释：开始保存现场
	MOVQ	SI, (g_sched+gobuf_pc)(AX)          // 注释：备份数据到g.sched.pc(g.gobuf_pc)(指令计数器)
	MOVQ	SP, (g_sched+gobuf_sp)(AX)          // 注释：备份数据到g.sched.sp(g.gobuf_sp)(SP存放函数参数访问头位置(基指针，栈低))
	MOVQ	AX, (g_sched+gobuf_g)(AX)           // 注释：备份数据到g.sched.g(g.gobuf_g)(当前运行的g地址)
	MOVQ	BP, (g_sched+gobuf_bp)(AX)          // 注释：备份数据到g.sched.bp(g.gobuf_bp)

    // 注释：切换到G0
	// switch to g0                             // 注释：下面是把g0切换到g上(g0的备份的栈SP切换到当前运行的栈SP)
	MOVQ	DX, g(CX)                           // 注释：把g.m.g0赋值到TLS(FS寄存器)对应的指针里(上面已经把G保存到g.sched里了（保存现场）)
	MOVQ	(g_sched+gobuf_sp)(DX), BX          // 注释：复制g.m.g0.sched.sp的参数到BX（SP存放函数参数访问头位置(基指针，栈低)）
	// make it look like mstart called systemstack on g0, to stop traceback // 注释：下面是用汇编模拟函数调用
	SUBQ	$8, BX                              // 注释：g0的栈(SP)扩容(扩容还没有被执行);BX -= 8; 结果是SP-=8
	MOVQ	$runtime·mstart(SB), DX             // 注释：把运行的mstart指针赋值给寄存器DX【纳闷】【不知道为什么把mstart放到这里，不知道在哪里使用】
	MOVQ	DX, 0(BX)                           // 注释：把mstart放到0(SP)位置（扩容后0(SP)是空指针;8(SP)是g0.gobuf_sp的指针）(下面会启动线程，并且启动调度系统)
	MOVQ	BX, SP                              // 注释：（把g0的SP替换当前SP）g0.gobuf_sp切换到栈上，后面可以执行该栈(模拟函数调用)(下面启动线程，并且启动调度系统)

    // 注释：执行闭包函数
	// call target function                     // 注释：下面是调用闭包指令
	MOVQ	DI, DX                              // 注释：闭包函数地址(DI)赋值给DX寄存器
	MOVQ	0(DI), DI                           // 注释：闭包函数体指令PC(0(DI))赋值到DI上（就是把闭包体的指令提取出来）
	CALL	DI                                  // 注释：调用闭包体指令（调用PC指令(相当于调用闭包里的函数)）(执行完成后，回到g0的栈SP空间上)

    // 注释：切换回业务G
	// switch back to g                         // 注释：下面是把g切换回来
	get_tls(CX)                                 // 注释：(g.m.g0)获取TLS数据(FS寄存器)（里面存储着G的指针）到CX寄存器里(这里只是存放G的一个指针)
	MOVQ	g(CX), AX                           // 注释：(g0)把g0结构体赋值到AX里
	MOVQ	g_m(AX), BX                         // 注释：(M)取g.m.g0.m
	MOVQ	m_curg(BX), AX                      // 注释：(当前要执行的业务G)取m.curg(一开始g.m.g0.m.curg等同于g.m.curg就是业务的g)（上面有判断m.curg一定等于业务，否则bad）
	MOVQ	AX, g(CX)                           // 注释：把业务G切换到TLS(FS)对应的指针里
	MOVQ	(g_sched+gobuf_sp)(AX), SP          // 注释：恢复SP（把业务的sp放回来）(业务G的参数赋值到栈的SP上)(SP存放函数参数访问头位置（基指针，栈低）)
	MOVQ	$0, (g_sched+gobuf_sp)(AX)          // 注释：清除业务G备份的SP
	RET                                         // 注释：返回

noswitch:                              // 注释：不发生系统切换，直接执行闭包函数
	// already on m stack; tail call the function
	// Using a tail call here cleans up tracebacks since we won't stop
	// at an intermediate systemstack.
	MOVQ	DI, DX                     // 注释：把闭包函数指针赋值给寄存器DX
	MOVQ	0(DI), DI                  // 注释：闭包函数体的执行PC指令放到DI里，等待执行
	JMP	DI                             // 注释：执行闭包函数体的指令

bad:
	// Bad: g is not gsignal, not g0, not curg. What is it?
	MOVQ	$runtime·badsystemstack(SB), AX
	CALL	AX
	INT	$3


/*
 * support for morestack
 */

// Called during function prolog when more stack is needed.
//
// The traceback routines see morestack on a g0 as being
// the top of a stack (for example, morestack calling newstack
// calling the scheduler calling newm calling gc), so we must
// record an argument size. For that purpose, it has no arguments.
TEXT runtime·morestack(SB),NOSPLIT,$0-0
	// Cannot grow scheduler stack (m->g0).
	get_tls(CX)
	MOVQ	g(CX), BX
	MOVQ	g_m(BX), BX
	MOVQ	m_g0(BX), SI
	CMPQ	g(CX), SI
	JNE	3(PC)
	CALL	runtime·badmorestackg0(SB)
	CALL	runtime·abort(SB)

	// Cannot grow signal stack (m->gsignal).
	MOVQ	m_gsignal(BX), SI
	CMPQ	g(CX), SI
	JNE	3(PC)
	CALL	runtime·badmorestackgsignal(SB)
	CALL	runtime·abort(SB)

	// Called from f.
	// Set m->morebuf to f's caller.
	NOP	SP	// tell vet SP changed - stop checking offsets
	MOVQ	8(SP), AX	// f's caller's PC
	MOVQ	AX, (m_morebuf+gobuf_pc)(BX)
	LEAQ	16(SP), AX	// f's caller's SP
	MOVQ	AX, (m_morebuf+gobuf_sp)(BX)
	get_tls(CX)
	MOVQ	g(CX), SI
	MOVQ	SI, (m_morebuf+gobuf_g)(BX)

	// Set g->sched to context in f.
	MOVQ	0(SP), AX // f's PC
	MOVQ	AX, (g_sched+gobuf_pc)(SI)
	MOVQ	SI, (g_sched+gobuf_g)(SI)
	LEAQ	8(SP), AX // f's SP
	MOVQ	AX, (g_sched+gobuf_sp)(SI)
	MOVQ	BP, (g_sched+gobuf_bp)(SI)
	MOVQ	DX, (g_sched+gobuf_ctxt)(SI)

	// Call newstack on m->g0's stack.
	MOVQ	m_g0(BX), BX
	MOVQ	BX, g(CX)
	MOVQ	(g_sched+gobuf_sp)(BX), SP
	CALL	runtime·newstack(SB)
	CALL	runtime·abort(SB)	// crash if newstack returns
	RET

// morestack but not preserving ctxt.
TEXT runtime·morestack_noctxt(SB),NOSPLIT,$0
	MOVL	$0, DX
	JMP	runtime·morestack(SB)

// reflectcall: call a function with the given argument list
// func call(argtype *_type, f *FuncVal, arg *byte, argsize, retoffset uint32).
// 注释：reflectcall：使用给定的参数列表func调用（argtype*_type，f*FuncVal，arg*byte，argsize，retoffset uint32）调用函数。
// we don't have variable-sized frames, so we use a small number
// of constant-sized-frame functions to encode a few bits of size in the pc.
// 注释：我们没有可变大小的帧，所以我们使用少量恒定大小的帧函数来在pc中编码一些大小的比特。
// Caution: ugly multiline assembly macros in your future! // 注释：注意：将来会出现难看的多行汇编宏！

// 注释：宏，用来比较并且执行，CX寄存器 > $MAXSIZE则跳过，小于则执行NAME对应的函数地址
#define DISPATCH(NAME,MAXSIZE)		\
	CMPQ	CX, $MAXSIZE;		\           // 注释：比较CX和第二个入参
	JA	3(PC);			\                   // 注释：如果CX > 第二个入参，则根据当前PC地址向后跳转3个PC位置（这里的意思是向后执行下个DISPATCH）
	MOVQ	$NAME(SB), AX;		\           // 注释：这里把NAME的栈地址赋值给寄存器AX，用来跳转执行
	JMP	AX                                  // 注释：跳转执行
// Note: can't just "JMP NAME(SB)" - bad inlining results.

// 注释：这里会调用d.fn函数，就是refer里执行的函数
TEXT ·reflectcall<ABIInternal>(SB), NOSPLIT, $0-32
	MOVLQZX argsize+24(FP), CX
	DISPATCH(runtime·call16, 16) // 注释：DISPATCH是个宏(在上面有定义),16 > CX寄存器则执行下一个，否则执行runtime·call16函数,这个函数也被一个宏包裹(宏名称：CALLFN)
	DISPATCH(runtime·call32, 32) // 注释：32 > CX寄存器则继续向下执行比较，否则执行runtime·call32
	DISPATCH(runtime·call64, 64)
	DISPATCH(runtime·call128, 128)
	DISPATCH(runtime·call256, 256)
	DISPATCH(runtime·call512, 512)
	DISPATCH(runtime·call1024, 1024)
	DISPATCH(runtime·call2048, 2048)
	DISPATCH(runtime·call4096, 4096)
	DISPATCH(runtime·call8192, 8192)
	DISPATCH(runtime·call16384, 16384)
	DISPATCH(runtime·call32768, 32768)
	DISPATCH(runtime·call65536, 65536)
	DISPATCH(runtime·call131072, 131072)
	DISPATCH(runtime·call262144, 262144)
	DISPATCH(runtime·call524288, 524288)
	DISPATCH(runtime·call1048576, 1048576)
	DISPATCH(runtime·call2097152, 2097152)
	DISPATCH(runtime·call4194304, 4194304)
	DISPATCH(runtime·call8388608, 8388608)
	DISPATCH(runtime·call16777216, 16777216)
	DISPATCH(runtime·call33554432, 33554432)
	DISPATCH(runtime·call67108864, 67108864)
	DISPATCH(runtime·call134217728, 134217728)
	DISPATCH(runtime·call268435456, 268435456)
	DISPATCH(runtime·call536870912, 536870912)
	DISPATCH(runtime·call1073741824, 1073741824)
	MOVQ	$runtime·badreflectcall(SB), AX         // 注释：如果上面都没有比较通过，则panic
	JMP	AX

// 注释：定义宏
#define CALLFN(NAME,MAXSIZE)			\
TEXT NAME(SB), WRAPPER, $MAXSIZE-32;		\
	NO_LOCAL_POINTERS;			\
	/* copy arguments to stack */		\
	MOVQ	argptr+16(FP), SI;		\
	MOVLQZX argsize+24(FP), CX;		\
	MOVQ	SP, DI;				\
	REP;MOVSB;				\
	/* call function */			\
	MOVQ	f+8(FP), DX;			\
	PCDATA  $PCDATA_StackMapIndex, $0;	\
	MOVQ	(DX), AX;			\
	CALL	AX;				\
	/* copy return values back */		\
	MOVQ	argtype+0(FP), DX;		\
	MOVQ	argptr+16(FP), DI;		\
	MOVLQZX	argsize+24(FP), CX;		\
	MOVLQZX	retoffset+28(FP), BX;		\
	MOVQ	SP, SI;				\
	ADDQ	BX, DI;				\
	ADDQ	BX, SI;				\
	SUBQ	BX, CX;				\
	CALL	callRet<>(SB);			\
	RET

// callRet copies return values back at the end of call*. This is a
// separate function so it can allocate stack space for the arguments
// to reflectcallmove. It does not follow the Go ABI; it expects its
// arguments in registers.
TEXT callRet<>(SB), NOSPLIT, $32-0
	NO_LOCAL_POINTERS
	MOVQ	DX, 0(SP)
	MOVQ	DI, 8(SP)
	MOVQ	SI, 16(SP)
	MOVQ	CX, 24(SP)
	CALL	runtime·reflectcallmove(SB)
	RET

CALLFN(·call16, 16)     // 注释：CALLFN是一个宏，上面有定义，用宏来组装汇编函数(因为只有函数名和栈大小不同，所以才用宏的方式实现)
CALLFN(·call32, 32)
CALLFN(·call64, 64)
CALLFN(·call128, 128)
CALLFN(·call256, 256)
CALLFN(·call512, 512)
CALLFN(·call1024, 1024)
CALLFN(·call2048, 2048)
CALLFN(·call4096, 4096)
CALLFN(·call8192, 8192)
CALLFN(·call16384, 16384)
CALLFN(·call32768, 32768)
CALLFN(·call65536, 65536)
CALLFN(·call131072, 131072)
CALLFN(·call262144, 262144)
CALLFN(·call524288, 524288)
CALLFN(·call1048576, 1048576)
CALLFN(·call2097152, 2097152)
CALLFN(·call4194304, 4194304)
CALLFN(·call8388608, 8388608)
CALLFN(·call16777216, 16777216)
CALLFN(·call33554432, 33554432)
CALLFN(·call67108864, 67108864)
CALLFN(·call134217728, 134217728)
CALLFN(·call268435456, 268435456)
CALLFN(·call536870912, 536870912)
CALLFN(·call1073741824, 1073741824)

// 注释：循环执行PAUSE系统指令，循环次数是第一个参数，CPU暂停，此时不会被CPU切片抢占(该指令只会占用CPU并消耗CPU时间)
TEXT runtime·procyield(SB),NOSPLIT,$0-0
	MOVL	cycles+0(FP), AX
again:
	PAUSE           // 注释：CPU暂停，此时不会被CPU切片抢占，（占用CPU并消耗CPU的时间）
	SUBL	$1, AX  // 注释：AX = AX - 1
	JNZ	again       // 注释：不等于时跳转again标签
	RET


TEXT ·publicationBarrier(SB),NOSPLIT,$0-0
	// Stores are already ordered on x86, so this is just a
	// compile barrier.
	RET

// func jmpdefer(fv *funcval, argp uintptr)
// argp is a caller SP.
// called from deferreturn.
// 1. pop the caller
// 2. sub 5 bytes from the callers return
// 3. jmp to the argument
TEXT runtime·jmpdefer(SB), NOSPLIT, $0-16
	MOVQ	fv+0(FP), DX	// fn
	MOVQ	argp+8(FP), BX	// caller sp
	LEAQ	-8(BX), SP	// caller sp after CALL
	MOVQ	-8(SP), BP	// restore BP as if deferreturn returned (harmless if framepointers not in use)
	SUBQ	$5, (SP)	// return to CALL again
	MOVQ	0(DX), BX
	JMP	BX	// but first run the deferred function

// Save state of caller into g->sched. Smashes R8, R9.
TEXT gosave<>(SB),NOSPLIT,$0
	get_tls(R8)
	MOVQ	g(R8), R8
	MOVQ	0(SP), R9
	MOVQ	R9, (g_sched+gobuf_pc)(R8)
	LEAQ	8(SP), R9
	MOVQ	R9, (g_sched+gobuf_sp)(R8)
	MOVQ	$0, (g_sched+gobuf_ret)(R8)
	MOVQ	BP, (g_sched+gobuf_bp)(R8)
	// Assert ctxt is zero. See func save.
	MOVQ	(g_sched+gobuf_ctxt)(R8), R9
	TESTQ	R9, R9
	JZ	2(PC)
	CALL	runtime·badctxt(SB)
	RET

// func asmcgocall(fn, arg unsafe.Pointer) int32
// Call fn(arg) on the scheduler stack,
// aligned appropriately for the gcc ABI.
// See cgocall.go for more details.
TEXT ·asmcgocall(SB),NOSPLIT,$0-20
	MOVQ	fn+0(FP), AX
	MOVQ	arg+8(FP), BX

	MOVQ	SP, DX

	// Figure out if we need to switch to m->g0 stack.
	// We get called to create new OS threads too, and those
	// come in on the m->g0 stack already.
	get_tls(CX)
	MOVQ	g(CX), R8
	CMPQ	R8, $0
	JEQ	nosave
	MOVQ	g_m(R8), R8
	MOVQ	m_g0(R8), SI
	MOVQ	g(CX), DI
	CMPQ	SI, DI
	JEQ	nosave
	MOVQ	m_gsignal(R8), SI
	CMPQ	SI, DI
	JEQ	nosave

	// Switch to system stack.
	MOVQ	m_g0(R8), SI
	CALL	gosave<>(SB)
	MOVQ	SI, g(CX)
	MOVQ	(g_sched+gobuf_sp)(SI), SP

	// Now on a scheduling stack (a pthread-created stack).
	// Make sure we have enough room for 4 stack-backed fast-call
	// registers as per windows amd64 calling convention.
	SUBQ	$64, SP
	ANDQ	$~15, SP	// alignment for gcc ABI
	MOVQ	DI, 48(SP)	// save g
	MOVQ	(g_stack+stack_hi)(DI), DI
	SUBQ	DX, DI
	MOVQ	DI, 40(SP)	// save depth in stack (can't just save SP, as stack might be copied during a callback)
	MOVQ	BX, DI		// DI = first argument in AMD64 ABI
	MOVQ	BX, CX		// CX = first argument in Win64
	CALL	AX

	// Restore registers, g, stack pointer.
	get_tls(CX)
	MOVQ	48(SP), DI
	MOVQ	(g_stack+stack_hi)(DI), SI
	SUBQ	40(SP), SI
	MOVQ	DI, g(CX)
	MOVQ	SI, SP

	MOVL	AX, ret+16(FP)
	RET

nosave:
	// Running on a system stack, perhaps even without a g.
	// Having no g can happen during thread creation or thread teardown
	// (see needm/dropm on Solaris, for example).
	// This code is like the above sequence but without saving/restoring g
	// and without worrying about the stack moving out from under us
	// (because we're on a system stack, not a goroutine stack).
	// The above code could be used directly if already on a system stack,
	// but then the only path through this code would be a rare case on Solaris.
	// Using this code for all "already on system stack" calls exercises it more,
	// which should help keep it correct.
	SUBQ	$64, SP
	ANDQ	$~15, SP
	MOVQ	$0, 48(SP)		// where above code stores g, in case someone looks during debugging
	MOVQ	DX, 40(SP)	// save original stack pointer
	MOVQ	BX, DI		// DI = first argument in AMD64 ABI
	MOVQ	BX, CX		// CX = first argument in Win64
	CALL	AX
	MOVQ	40(SP), SI	// restore original stack pointer
	MOVQ	SI, SP
	MOVL	AX, ret+16(FP)
	RET

// func cgocallback(fn, frame unsafe.Pointer, ctxt uintptr)
// See cgocall.go for more details.
TEXT ·cgocallback(SB),NOSPLIT,$24-24
	NO_LOCAL_POINTERS

	// If g is nil, Go did not create the current thread.
	// Call needm to obtain one m for temporary use.
	// In this case, we're running on the thread stack, so there's
	// lots of space, but the linker doesn't know. Hide the call from
	// the linker analysis by using an indirect call through AX.
	get_tls(CX)
#ifdef GOOS_windows
	MOVL	$0, BX
	CMPQ	CX, $0
	JEQ	2(PC)
#endif
	MOVQ	g(CX), BX
	CMPQ	BX, $0
	JEQ	needm
	MOVQ	g_m(BX), BX
	MOVQ	BX, savedm-8(SP)	// saved copy of oldm
	JMP	havem
needm:
	MOVQ    $runtime·needm(SB), AX
	CALL	AX
	MOVQ	$0, savedm-8(SP) // dropm on return
	get_tls(CX)
	MOVQ	g(CX), BX
	MOVQ	g_m(BX), BX

	// Set m->sched.sp = SP, so that if a panic happens
	// during the function we are about to execute, it will
	// have a valid SP to run on the g0 stack.
	// The next few lines (after the havem label)
	// will save this SP onto the stack and then write
	// the same SP back to m->sched.sp. That seems redundant,
	// but if an unrecovered panic happens, unwindm will
	// restore the g->sched.sp from the stack location
	// and then systemstack will try to use it. If we don't set it here,
	// that restored SP will be uninitialized (typically 0) and
	// will not be usable.
	MOVQ	m_g0(BX), SI
	MOVQ	SP, (g_sched+gobuf_sp)(SI)

havem:
	// Now there's a valid m, and we're running on its m->g0.
	// Save current m->g0->sched.sp on stack and then set it to SP.
	// Save current sp in m->g0->sched.sp in preparation for
	// switch back to m->curg stack.
	// NOTE: unwindm knows that the saved g->sched.sp is at 0(SP).
	MOVQ	m_g0(BX), SI
	MOVQ	(g_sched+gobuf_sp)(SI), AX
	MOVQ	AX, 0(SP)
	MOVQ	SP, (g_sched+gobuf_sp)(SI)

	// Switch to m->curg stack and call runtime.cgocallbackg.
	// Because we are taking over the execution of m->curg
	// but *not* resuming what had been running, we need to
	// save that information (m->curg->sched) so we can restore it.
	// We can restore m->curg->sched.sp easily, because calling
	// runtime.cgocallbackg leaves SP unchanged upon return.
	// To save m->curg->sched.pc, we push it onto the curg stack and
	// open a frame the same size as cgocallback's g0 frame.
	// Once we switch to the curg stack, the pushed PC will appear
	// to be the return PC of cgocallback, so that the traceback
	// will seamlessly trace back into the earlier calls.
	MOVQ	m_curg(BX), SI
	MOVQ	SI, g(CX)
	MOVQ	(g_sched+gobuf_sp)(SI), DI  // prepare stack as DI
	MOVQ	(g_sched+gobuf_pc)(SI), BX
	MOVQ	BX, -8(DI)  // "push" return PC on the g stack
	// Gather our arguments into registers.
	MOVQ	fn+0(FP), BX
	MOVQ	frame+8(FP), CX
	MOVQ	ctxt+16(FP), DX
	// Compute the size of the frame, including return PC and, if
	// GOEXPERIMENT=framepointer, the saved base pointer
	LEAQ	fn+0(FP), AX
	SUBQ	SP, AX   // AX is our actual frame size
	SUBQ	AX, DI   // Allocate the same frame size on the g stack
	MOVQ	DI, SP

	MOVQ	BX, 0(SP)
	MOVQ	CX, 8(SP)
	MOVQ	DX, 16(SP)
	CALL	runtime·cgocallbackg(SB)

	// Compute the size of the frame again. FP and SP have
	// completely different values here than they did above,
	// but only their difference matters.
	LEAQ	fn+0(FP), AX
	SUBQ	SP, AX

	// Restore g->sched (== m->curg->sched) from saved values.
	get_tls(CX)
	MOVQ	g(CX), SI
	MOVQ	SP, DI
	ADDQ	AX, DI
	MOVQ	-8(DI), BX
	MOVQ	BX, (g_sched+gobuf_pc)(SI)
	MOVQ	DI, (g_sched+gobuf_sp)(SI)

	// Switch back to m->g0's stack and restore m->g0->sched.sp.
	// (Unlike m->curg, the g0 goroutine never uses sched.pc,
	// so we do not have to restore it.)
	MOVQ	g(CX), BX
	MOVQ	g_m(BX), BX
	MOVQ	m_g0(BX), SI
	MOVQ	SI, g(CX)
	MOVQ	(g_sched+gobuf_sp)(SI), SP
	MOVQ	0(SP), AX
	MOVQ	AX, (g_sched+gobuf_sp)(SI)

	// If the m on entry was nil, we called needm above to borrow an m
	// for the duration of the call. Since the call is over, return it with dropm.
	MOVQ	savedm-8(SP), BX
	CMPQ	BX, $0
	JNE 3(PC)
	MOVQ	$runtime·dropm(SB), AX
	CALL	AX

	// Done!
	RET

// func setg(gg *g)
// set g. for use by needm.
TEXT runtime·setg(SB), NOSPLIT, $0-8
	MOVQ	gg+0(FP), BX
#ifdef GOOS_windows
	CMPQ	BX, $0
	JNE	settls
	MOVQ	$0, 0x28(GS)
	RET
settls:
	MOVQ	g_m(BX), AX
	LEAQ	m_tls(AX), AX
	MOVQ	AX, 0x28(GS)
#endif
	get_tls(CX)
	MOVQ	BX, g(CX)
	RET

// void setg_gcc(G*); set g called from gcc.
TEXT setg_gcc<>(SB),NOSPLIT,$0
	get_tls(AX)
	MOVQ	DI, g(AX)
	RET

TEXT runtime·abort(SB),NOSPLIT,$0-0
	INT	$3
loop:
	JMP	loop

// check that SP is in range [g->stack.lo, g->stack.hi)
TEXT runtime·stackcheck(SB), NOSPLIT, $0-0
	get_tls(CX)
	MOVQ	g(CX), AX
	CMPQ	(g_stack+stack_hi)(AX), SP
	JHI	2(PC)
	CALL	runtime·abort(SB)
	CMPQ	SP, (g_stack+stack_lo)(AX)
	JHI	2(PC)
	CALL	runtime·abort(SB)
	RET

// func cputicks() int64
// 注释：CPU时钟周期计数器(cpu自从加电以来执行的周期数)，高32位存在DX中，低32位存在AX寄存器中
TEXT runtime·cputicks(SB),NOSPLIT,$0-0
	CMPB	runtime·lfenceBeforeRdtsc(SB), $1
	JNE	mfence
	LFENCE
	JMP	done
mfence:
	MFENCE
done:
	RDTSC                   // 获取cpu自从加电以来执行的周期数
	SHLQ	$32, DX         // 注释：SHLQ指令就是将DX中的高32位值左移32位
	ADDQ	DX, AX          // 注释：把高32位和低32位叠加起来放到AX中
	MOVQ	AX, ret+0(FP)   // 注释：把AX放到返回值中
	RET                     // 注释：汇编函数返回结束

// func memhash(p unsafe.Pointer, h, s uintptr) uintptr
// hash function using AES hardware instructions // 注释：使用AES硬件指令的散列函数
TEXT runtime·memhash(SB),NOSPLIT,$0-32
	CMPB	runtime·useAeshash(SB), $0      // 注释：判断runtime·useAeshash(SB)是否等于0
	JEQ	noaes                               // 注释：如果等于零跳转到noaes
	MOVQ	p+0(FP), AX	// ptr to data      // 注释：获取指针地址(第一个参数,AMD64系统指针是8字节64位)放到寄存器AX中
	MOVQ	s+16(FP), CX	// size         // 注释：获取大小(第三个个参数)放到CX寄存器里
	LEAQ	ret+24(FP), DX                  // 注释：把返回值的内存地址放到DX寄存器里
	JMP	aeshashbody<>(SB)                   // 注释：执行(跳转到)aeshashbody,跳转时可以访问上面的参数（第二个参数就是在调用函数时使用）
noaes:
	JMP	runtime·memhashFallback(SB)         // 注释：执行(跳转到)runtime·memhashFallback

// func strhash(p unsafe.Pointer, h uintptr) uintptr
TEXT runtime·strhash(SB),NOSPLIT,$0-24
	CMPB	runtime·useAeshash(SB), $0
	JEQ	noaes
	MOVQ	p+0(FP), AX	// ptr to string struct
	MOVQ	8(AX), CX	// length of string
	MOVQ	(AX), AX	// string data
	LEAQ	ret+16(FP), DX
	JMP	aeshashbody<>(SB)
noaes:
	JMP	runtime·strhashFallback(SB)

// AX: data
// CX: length
// DX: address to put return value
// 注释：在执行前内存寄存器里AX表示数据（内存指针），CX是尺寸大小，DX是存放返回的数据指针
// 注释：执行使用AES计算哈希值
TEXT aeshashbody<>(SB),NOSPLIT,$0-0
	// Fill an SSE register with our seeds. // 注释： 用我们的种子填写SSE登记册。
	MOVQ	h+8(FP), X0			// 64 bits of per-table hash seed   // 注释：(就是上面的第二个参数)每个表的64位散列种子
	PINSRW	$4, CX, X0			// 16 bits of length                // 注释：长度为16位,将CX中的低位插入到X0中的第4个位置
	PSHUFHW $0, X0, X0			// repeat length 4 times total      // 注释：重复长度总计4次，（根据X0（第三个参数）中编码对X0（第二个参数）高位进行打乱处理，结果放到0中（丢掉结果））
	MOVO	X0, X1				// save unscrambled seed            // 注释：保存未处理的种子，到X1中
	PXOR	runtime·aeskeysched(SB), X0	// xor in per-process seed  // 注释：每个进程种子中的xor
	AESENC	X0, X0				// scramble seed                    // 注释：打乱种子(执行一轮AES加密流程)

	CMPQ	CX, $16             // 注释：判断CX寄存器里的值和16相比较
	JB	aes0to15                // 注释：如果小于则跳转到aes0to15标记
	JE	aes16                   // 注释：如果等于则跳转aes16标记
	CMPQ	CX, $32             // 注释：比较CX和32
	JBE	aes17to32               // 注释：小于等于是跳转到aes17to32标记
	CMPQ	CX, $64             // 注释：比较CX寄存器和64
	JBE	aes33to64               // 注释：小于等于时跳转到aes33to64标记
	CMPQ	CX, $128            // 注释：比较CX寄存器和128
	JBE	aes65to128              // 注释：小于等于时跳转到aes65to128标记
	JMP	aes129plus              // 注释：无条件跳转到aes129plus标记

aes0to15:
	TESTQ	CX, CX
	JE	aes0

	ADDQ	$16, AX
	TESTW	$0xff0, AX
	JE	endofpage

	// 16 bytes loaded at this address won't cross
	// a page boundary, so we can load it directly.
	MOVOU	-16(AX), X1
	ADDQ	CX, CX
	MOVQ	$masks<>(SB), AX
	PAND	(AX)(CX*8), X1
final1:
	PXOR	X0, X1	// xor data with seed
	AESENC	X1, X1	// scramble combo 3 times
	AESENC	X1, X1
	AESENC	X1, X1
	MOVQ	X1, (DX)
	RET

endofpage:
	// address ends in 1111xxxx. Might be up against
	// a page boundary, so load ending at last byte.
	// Then shift bytes down using pshufb.
	MOVOU	-32(AX)(CX*1), X1
	ADDQ	CX, CX
	MOVQ	$shifts<>(SB), AX
	PSHUFB	(AX)(CX*8), X1
	JMP	final1

aes0:
	// Return scrambled input seed
	AESENC	X0, X0
	MOVQ	X0, (DX)
	RET

aes16:
	MOVOU	(AX), X1
	JMP	final1

aes17to32:
	// make second starting seed
	PXOR	runtime·aeskeysched+16(SB), X1
	AESENC	X1, X1

	// load data to be hashed
	MOVOU	(AX), X2
	MOVOU	-16(AX)(CX*1), X3

	// xor with seed
	PXOR	X0, X2
	PXOR	X1, X3

	// scramble 3 times
	AESENC	X2, X2
	AESENC	X3, X3
	AESENC	X2, X2
	AESENC	X3, X3
	AESENC	X2, X2
	AESENC	X3, X3

	// combine results
	PXOR	X3, X2
	MOVQ	X2, (DX)
	RET

aes33to64:
	// make 3 more starting seeds
	MOVO	X1, X2
	MOVO	X1, X3
	PXOR	runtime·aeskeysched+16(SB), X1
	PXOR	runtime·aeskeysched+32(SB), X2
	PXOR	runtime·aeskeysched+48(SB), X3
	AESENC	X1, X1
	AESENC	X2, X2
	AESENC	X3, X3

	MOVOU	(AX), X4
	MOVOU	16(AX), X5
	MOVOU	-32(AX)(CX*1), X6
	MOVOU	-16(AX)(CX*1), X7

	PXOR	X0, X4
	PXOR	X1, X5
	PXOR	X2, X6
	PXOR	X3, X7

	AESENC	X4, X4
	AESENC	X5, X5
	AESENC	X6, X6
	AESENC	X7, X7

	AESENC	X4, X4
	AESENC	X5, X5
	AESENC	X6, X6
	AESENC	X7, X7

	AESENC	X4, X4
	AESENC	X5, X5
	AESENC	X6, X6
	AESENC	X7, X7

	PXOR	X6, X4
	PXOR	X7, X5
	PXOR	X5, X4
	MOVQ	X4, (DX)
	RET

aes65to128:
	// make 7 more starting seeds
	MOVO	X1, X2
	MOVO	X1, X3
	MOVO	X1, X4
	MOVO	X1, X5
	MOVO	X1, X6
	MOVO	X1, X7
	PXOR	runtime·aeskeysched+16(SB), X1
	PXOR	runtime·aeskeysched+32(SB), X2
	PXOR	runtime·aeskeysched+48(SB), X3
	PXOR	runtime·aeskeysched+64(SB), X4
	PXOR	runtime·aeskeysched+80(SB), X5
	PXOR	runtime·aeskeysched+96(SB), X6
	PXOR	runtime·aeskeysched+112(SB), X7
	AESENC	X1, X1
	AESENC	X2, X2
	AESENC	X3, X3
	AESENC	X4, X4
	AESENC	X5, X5
	AESENC	X6, X6
	AESENC	X7, X7

	// load data
	MOVOU	(AX), X8
	MOVOU	16(AX), X9
	MOVOU	32(AX), X10
	MOVOU	48(AX), X11
	MOVOU	-64(AX)(CX*1), X12
	MOVOU	-48(AX)(CX*1), X13
	MOVOU	-32(AX)(CX*1), X14
	MOVOU	-16(AX)(CX*1), X15

	// xor with seed
	PXOR	X0, X8
	PXOR	X1, X9
	PXOR	X2, X10
	PXOR	X3, X11
	PXOR	X4, X12
	PXOR	X5, X13
	PXOR	X6, X14
	PXOR	X7, X15

	// scramble 3 times
	AESENC	X8, X8
	AESENC	X9, X9
	AESENC	X10, X10
	AESENC	X11, X11
	AESENC	X12, X12
	AESENC	X13, X13
	AESENC	X14, X14
	AESENC	X15, X15

	AESENC	X8, X8
	AESENC	X9, X9
	AESENC	X10, X10
	AESENC	X11, X11
	AESENC	X12, X12
	AESENC	X13, X13
	AESENC	X14, X14
	AESENC	X15, X15

	AESENC	X8, X8
	AESENC	X9, X9
	AESENC	X10, X10
	AESENC	X11, X11
	AESENC	X12, X12
	AESENC	X13, X13
	AESENC	X14, X14
	AESENC	X15, X15

	// combine results
	PXOR	X12, X8
	PXOR	X13, X9
	PXOR	X14, X10
	PXOR	X15, X11
	PXOR	X10, X8
	PXOR	X11, X9
	PXOR	X9, X8
	MOVQ	X8, (DX)
	RET

aes129plus:
	// make 7 more starting seeds
	MOVO	X1, X2
	MOVO	X1, X3
	MOVO	X1, X4
	MOVO	X1, X5
	MOVO	X1, X6
	MOVO	X1, X7
	PXOR	runtime·aeskeysched+16(SB), X1
	PXOR	runtime·aeskeysched+32(SB), X2
	PXOR	runtime·aeskeysched+48(SB), X3
	PXOR	runtime·aeskeysched+64(SB), X4
	PXOR	runtime·aeskeysched+80(SB), X5
	PXOR	runtime·aeskeysched+96(SB), X6
	PXOR	runtime·aeskeysched+112(SB), X7
	AESENC	X1, X1
	AESENC	X2, X2
	AESENC	X3, X3
	AESENC	X4, X4
	AESENC	X5, X5
	AESENC	X6, X6
	AESENC	X7, X7

	// start with last (possibly overlapping) block
	MOVOU	-128(AX)(CX*1), X8
	MOVOU	-112(AX)(CX*1), X9
	MOVOU	-96(AX)(CX*1), X10
	MOVOU	-80(AX)(CX*1), X11
	MOVOU	-64(AX)(CX*1), X12
	MOVOU	-48(AX)(CX*1), X13
	MOVOU	-32(AX)(CX*1), X14
	MOVOU	-16(AX)(CX*1), X15

	// xor in seed
	PXOR	X0, X8
	PXOR	X1, X9
	PXOR	X2, X10
	PXOR	X3, X11
	PXOR	X4, X12
	PXOR	X5, X13
	PXOR	X6, X14
	PXOR	X7, X15

	// compute number of remaining 128-byte blocks
	DECQ	CX
	SHRQ	$7, CX

aesloop:
	// scramble state
	AESENC	X8, X8
	AESENC	X9, X9
	AESENC	X10, X10
	AESENC	X11, X11
	AESENC	X12, X12
	AESENC	X13, X13
	AESENC	X14, X14
	AESENC	X15, X15

	// scramble state, xor in a block
	MOVOU	(AX), X0
	MOVOU	16(AX), X1
	MOVOU	32(AX), X2
	MOVOU	48(AX), X3
	AESENC	X0, X8
	AESENC	X1, X9
	AESENC	X2, X10
	AESENC	X3, X11
	MOVOU	64(AX), X4
	MOVOU	80(AX), X5
	MOVOU	96(AX), X6
	MOVOU	112(AX), X7
	AESENC	X4, X12
	AESENC	X5, X13
	AESENC	X6, X14
	AESENC	X7, X15

	ADDQ	$128, AX
	DECQ	CX
	JNE	aesloop

	// 3 more scrambles to finish
	AESENC	X8, X8
	AESENC	X9, X9
	AESENC	X10, X10
	AESENC	X11, X11
	AESENC	X12, X12
	AESENC	X13, X13
	AESENC	X14, X14
	AESENC	X15, X15
	AESENC	X8, X8
	AESENC	X9, X9
	AESENC	X10, X10
	AESENC	X11, X11
	AESENC	X12, X12
	AESENC	X13, X13
	AESENC	X14, X14
	AESENC	X15, X15
	AESENC	X8, X8
	AESENC	X9, X9
	AESENC	X10, X10
	AESENC	X11, X11
	AESENC	X12, X12
	AESENC	X13, X13
	AESENC	X14, X14
	AESENC	X15, X15

	PXOR	X12, X8
	PXOR	X13, X9
	PXOR	X14, X10
	PXOR	X15, X11
	PXOR	X10, X8
	PXOR	X11, X9
	PXOR	X9, X8
	MOVQ	X8, (DX)
	RET

// func memhash32(p unsafe.Pointer, h uintptr) uintptr
TEXT runtime·memhash32(SB),NOSPLIT,$0-24
	CMPB	runtime·useAeshash(SB), $0
	JEQ	noaes
	MOVQ	p+0(FP), AX	// ptr to data
	MOVQ	h+8(FP), X0	// seed
	PINSRD	$2, (AX), X0	// data
	AESENC	runtime·aeskeysched+0(SB), X0
	AESENC	runtime·aeskeysched+16(SB), X0
	AESENC	runtime·aeskeysched+32(SB), X0
	MOVQ	X0, ret+16(FP)
	RET
noaes:
	JMP	runtime·memhash32Fallback(SB)

// func memhash64(p unsafe.Pointer, h uintptr) uintptr
TEXT runtime·memhash64(SB),NOSPLIT,$0-24
	CMPB	runtime·useAeshash(SB), $0
	JEQ	noaes
	MOVQ	p+0(FP), AX	// ptr to data
	MOVQ	h+8(FP), X0	// seed
	PINSRQ	$1, (AX), X0	// data
	AESENC	runtime·aeskeysched+0(SB), X0
	AESENC	runtime·aeskeysched+16(SB), X0
	AESENC	runtime·aeskeysched+32(SB), X0
	MOVQ	X0, ret+16(FP)
	RET
noaes:
	JMP	runtime·memhash64Fallback(SB)

// simple mask to get rid of data in the high part of the register.
DATA masks<>+0x00(SB)/8, $0x0000000000000000
DATA masks<>+0x08(SB)/8, $0x0000000000000000
DATA masks<>+0x10(SB)/8, $0x00000000000000ff
DATA masks<>+0x18(SB)/8, $0x0000000000000000
DATA masks<>+0x20(SB)/8, $0x000000000000ffff
DATA masks<>+0x28(SB)/8, $0x0000000000000000
DATA masks<>+0x30(SB)/8, $0x0000000000ffffff
DATA masks<>+0x38(SB)/8, $0x0000000000000000
DATA masks<>+0x40(SB)/8, $0x00000000ffffffff
DATA masks<>+0x48(SB)/8, $0x0000000000000000
DATA masks<>+0x50(SB)/8, $0x000000ffffffffff
DATA masks<>+0x58(SB)/8, $0x0000000000000000
DATA masks<>+0x60(SB)/8, $0x0000ffffffffffff
DATA masks<>+0x68(SB)/8, $0x0000000000000000
DATA masks<>+0x70(SB)/8, $0x00ffffffffffffff
DATA masks<>+0x78(SB)/8, $0x0000000000000000
DATA masks<>+0x80(SB)/8, $0xffffffffffffffff
DATA masks<>+0x88(SB)/8, $0x0000000000000000
DATA masks<>+0x90(SB)/8, $0xffffffffffffffff
DATA masks<>+0x98(SB)/8, $0x00000000000000ff
DATA masks<>+0xa0(SB)/8, $0xffffffffffffffff
DATA masks<>+0xa8(SB)/8, $0x000000000000ffff
DATA masks<>+0xb0(SB)/8, $0xffffffffffffffff
DATA masks<>+0xb8(SB)/8, $0x0000000000ffffff
DATA masks<>+0xc0(SB)/8, $0xffffffffffffffff
DATA masks<>+0xc8(SB)/8, $0x00000000ffffffff
DATA masks<>+0xd0(SB)/8, $0xffffffffffffffff
DATA masks<>+0xd8(SB)/8, $0x000000ffffffffff
DATA masks<>+0xe0(SB)/8, $0xffffffffffffffff
DATA masks<>+0xe8(SB)/8, $0x0000ffffffffffff
DATA masks<>+0xf0(SB)/8, $0xffffffffffffffff
DATA masks<>+0xf8(SB)/8, $0x00ffffffffffffff
GLOBL masks<>(SB),RODATA,$256

// func checkASM() bool
TEXT ·checkASM(SB),NOSPLIT,$0-1
	// check that masks<>(SB) and shifts<>(SB) are aligned to 16-byte
	MOVQ	$masks<>(SB), AX
	MOVQ	$shifts<>(SB), BX
	ORQ	BX, AX
	TESTQ	$15, AX
	SETEQ	ret+0(FP)
	RET

// these are arguments to pshufb. They move data down from
// the high bytes of the register to the low bytes of the register.
// index is how many bytes to move.
DATA shifts<>+0x00(SB)/8, $0x0000000000000000
DATA shifts<>+0x08(SB)/8, $0x0000000000000000
DATA shifts<>+0x10(SB)/8, $0xffffffffffffff0f
DATA shifts<>+0x18(SB)/8, $0xffffffffffffffff
DATA shifts<>+0x20(SB)/8, $0xffffffffffff0f0e
DATA shifts<>+0x28(SB)/8, $0xffffffffffffffff
DATA shifts<>+0x30(SB)/8, $0xffffffffff0f0e0d
DATA shifts<>+0x38(SB)/8, $0xffffffffffffffff
DATA shifts<>+0x40(SB)/8, $0xffffffff0f0e0d0c
DATA shifts<>+0x48(SB)/8, $0xffffffffffffffff
DATA shifts<>+0x50(SB)/8, $0xffffff0f0e0d0c0b
DATA shifts<>+0x58(SB)/8, $0xffffffffffffffff
DATA shifts<>+0x60(SB)/8, $0xffff0f0e0d0c0b0a
DATA shifts<>+0x68(SB)/8, $0xffffffffffffffff
DATA shifts<>+0x70(SB)/8, $0xff0f0e0d0c0b0a09
DATA shifts<>+0x78(SB)/8, $0xffffffffffffffff
DATA shifts<>+0x80(SB)/8, $0x0f0e0d0c0b0a0908
DATA shifts<>+0x88(SB)/8, $0xffffffffffffffff
DATA shifts<>+0x90(SB)/8, $0x0e0d0c0b0a090807
DATA shifts<>+0x98(SB)/8, $0xffffffffffffff0f
DATA shifts<>+0xa0(SB)/8, $0x0d0c0b0a09080706
DATA shifts<>+0xa8(SB)/8, $0xffffffffffff0f0e
DATA shifts<>+0xb0(SB)/8, $0x0c0b0a0908070605
DATA shifts<>+0xb8(SB)/8, $0xffffffffff0f0e0d
DATA shifts<>+0xc0(SB)/8, $0x0b0a090807060504
DATA shifts<>+0xc8(SB)/8, $0xffffffff0f0e0d0c
DATA shifts<>+0xd0(SB)/8, $0x0a09080706050403
DATA shifts<>+0xd8(SB)/8, $0xffffff0f0e0d0c0b
DATA shifts<>+0xe0(SB)/8, $0x0908070605040302
DATA shifts<>+0xe8(SB)/8, $0xffff0f0e0d0c0b0a
DATA shifts<>+0xf0(SB)/8, $0x0807060504030201
DATA shifts<>+0xf8(SB)/8, $0xff0f0e0d0c0b0a09
GLOBL shifts<>(SB),RODATA,$256

TEXT runtime·return0(SB), NOSPLIT, $0
	MOVL	$0, AX
	RET


// Called from cgo wrappers, this function returns g->m->curg.stack.hi.
// Must obey the gcc calling convention.
TEXT _cgo_topofstack(SB),NOSPLIT,$0
	get_tls(CX)
	MOVQ	g(CX), AX
	MOVQ	g_m(AX), AX
	MOVQ	m_curg(AX), AX
	MOVQ	(g_stack+stack_hi)(AX), AX
	RET

// The top-most function running on a goroutine
// returns to goexit+PCQuantum. Defined as ABIInternal
// so as to make it identifiable to traceback (this
// function it used as a sentinel; traceback wants to
// see the func PC, not a wrapper PC).
// 注释：译：goroutine上运行的最上面的函数返回goexit+PCQuantum。定义为ABIInteral，以便使其可被回溯识别（该函数用作哨兵；回溯希望看到函数PC，而不是包装PC）。
// 注释：G调用完成后调用的退出函数
// 注释：协成退出
TEXT runtime·goexit<ABIInternal>(SB),NOSPLIT,$0-0
	BYTE	$0x90	// NOP
	CALL	runtime·goexit1(SB)	// 注释：永远不返回，因为执行了下一次调度 // does not return
	// traceback from goexit1 must hit code range of goexit
	BYTE	$0x90	// NOP

// This is called from .init_array and follows the platform, not Go, ABI.
TEXT runtime·addmoduledata(SB),NOSPLIT,$0-0
	PUSHQ	R15 // The access to global variables below implicitly uses R15, which is callee-save
	MOVQ	runtime·lastmoduledatap(SB), AX
	MOVQ	DI, moduledata_next(AX)
	MOVQ	DI, runtime·lastmoduledatap(SB)
	POPQ	R15
	RET

// gcWriteBarrier performs a heap pointer write and informs the GC.
//
// gcWriteBarrier does NOT follow the Go ABI. It takes two arguments:
// - DI is the destination of the write
// - AX is the value being written at DI
// It clobbers FLAGS. It does not clobber any general-purpose registers,
// but may clobber others (e.g., SSE registers).
// Defined as ABIInternal since it does not use the stack-based Go ABI.
TEXT runtime·gcWriteBarrier<ABIInternal>(SB),NOSPLIT,$120
	// Save the registers clobbered by the fast path. This is slightly
	// faster than having the caller spill these.
	MOVQ	R14, 104(SP)
	MOVQ	R13, 112(SP)
	// TODO: Consider passing g.m.p in as an argument so they can be shared
	// across a sequence of write barriers.
	get_tls(R13)
	MOVQ	g(R13), R13
	MOVQ	g_m(R13), R13
	MOVQ	m_p(R13), R13
	MOVQ	(p_wbBuf+wbBuf_next)(R13), R14
	// Increment wbBuf.next position.
	LEAQ	16(R14), R14
	MOVQ	R14, (p_wbBuf+wbBuf_next)(R13)
	CMPQ	R14, (p_wbBuf+wbBuf_end)(R13)
	// Record the write.
	MOVQ	AX, -16(R14)	// Record value
	// Note: This turns bad pointer writes into bad
	// pointer reads, which could be confusing. We could avoid
	// reading from obviously bad pointers, which would
	// take care of the vast majority of these. We could
	// patch this up in the signal handler, or use XCHG to
	// combine the read and the write.
	MOVQ	(DI), R13
	MOVQ	R13, -8(R14)	// Record *slot
	// Is the buffer full? (flags set in CMPQ above)
	JEQ	flush
ret:
	MOVQ	104(SP), R14
	MOVQ	112(SP), R13
	// Do the write.
	MOVQ	AX, (DI)
	RET

flush:
	// Save all general purpose registers since these could be
	// clobbered by wbBufFlush and were not saved by the caller.
	// It is possible for wbBufFlush to clobber other registers
	// (e.g., SSE registers), but the compiler takes care of saving
	// those in the caller if necessary. This strikes a balance
	// with registers that are likely to be used.
	//
	// We don't have type information for these, but all code under
	// here is NOSPLIT, so nothing will observe these.
	//
	// TODO: We could strike a different balance; e.g., saving X0
	// and not saving GP registers that are less likely to be used.
	MOVQ	DI, 0(SP)	// Also first argument to wbBufFlush
	MOVQ	AX, 8(SP)	// Also second argument to wbBufFlush
	MOVQ	BX, 16(SP)
	MOVQ	CX, 24(SP)
	MOVQ	DX, 32(SP)
	// DI already saved
	MOVQ	SI, 40(SP)
	MOVQ	BP, 48(SP)
	MOVQ	R8, 56(SP)
	MOVQ	R9, 64(SP)
	MOVQ	R10, 72(SP)
	MOVQ	R11, 80(SP)
	MOVQ	R12, 88(SP)
	// R13 already saved
	// R14 already saved
	MOVQ	R15, 96(SP)

	// This takes arguments DI and AX
	CALL	runtime·wbBufFlush(SB)

	MOVQ	0(SP), DI
	MOVQ	8(SP), AX
	MOVQ	16(SP), BX
	MOVQ	24(SP), CX
	MOVQ	32(SP), DX
	MOVQ	40(SP), SI
	MOVQ	48(SP), BP
	MOVQ	56(SP), R8
	MOVQ	64(SP), R9
	MOVQ	72(SP), R10
	MOVQ	80(SP), R11
	MOVQ	88(SP), R12
	MOVQ	96(SP), R15
	JMP	ret

// gcWriteBarrierCX is gcWriteBarrier, but with args in DI and CX.
// Defined as ABIInternal since it does not use the stable Go ABI.
TEXT runtime·gcWriteBarrierCX<ABIInternal>(SB),NOSPLIT,$0
	XCHGQ CX, AX
	CALL runtime·gcWriteBarrier<ABIInternal>(SB)
	XCHGQ CX, AX
	RET

// gcWriteBarrierDX is gcWriteBarrier, but with args in DI and DX.
// Defined as ABIInternal since it does not use the stable Go ABI.
TEXT runtime·gcWriteBarrierDX<ABIInternal>(SB),NOSPLIT,$0
	XCHGQ DX, AX
	CALL runtime·gcWriteBarrier<ABIInternal>(SB)
	XCHGQ DX, AX
	RET

// gcWriteBarrierBX is gcWriteBarrier, but with args in DI and BX.
// Defined as ABIInternal since it does not use the stable Go ABI.
TEXT runtime·gcWriteBarrierBX<ABIInternal>(SB),NOSPLIT,$0
	XCHGQ BX, AX
	CALL runtime·gcWriteBarrier<ABIInternal>(SB)
	XCHGQ BX, AX
	RET

// gcWriteBarrierBP is gcWriteBarrier, but with args in DI and BP.
// Defined as ABIInternal since it does not use the stable Go ABI.
TEXT runtime·gcWriteBarrierBP<ABIInternal>(SB),NOSPLIT,$0
	XCHGQ BP, AX
	CALL runtime·gcWriteBarrier<ABIInternal>(SB)
	XCHGQ BP, AX
	RET

// gcWriteBarrierSI is gcWriteBarrier, but with args in DI and SI.
// Defined as ABIInternal since it does not use the stable Go ABI.
TEXT runtime·gcWriteBarrierSI<ABIInternal>(SB),NOSPLIT,$0
	XCHGQ SI, AX
	CALL runtime·gcWriteBarrier<ABIInternal>(SB)
	XCHGQ SI, AX
	RET

// gcWriteBarrierR8 is gcWriteBarrier, but with args in DI and R8.
// Defined as ABIInternal since it does not use the stable Go ABI.
TEXT runtime·gcWriteBarrierR8<ABIInternal>(SB),NOSPLIT,$0
	XCHGQ R8, AX
	CALL runtime·gcWriteBarrier<ABIInternal>(SB)
	XCHGQ R8, AX
	RET

// gcWriteBarrierR9 is gcWriteBarrier, but with args in DI and R9.
// Defined as ABIInternal since it does not use the stable Go ABI.
TEXT runtime·gcWriteBarrierR9<ABIInternal>(SB),NOSPLIT,$0
	XCHGQ R9, AX
	CALL runtime·gcWriteBarrier<ABIInternal>(SB)
	XCHGQ R9, AX
	RET

DATA	debugCallFrameTooLarge<>+0x00(SB)/20, $"call frame too large"
GLOBL	debugCallFrameTooLarge<>(SB), RODATA, $20	// Size duplicated below

// debugCallV1 is the entry point for debugger-injected function
// calls on running goroutines. It informs the runtime that a
// debug call has been injected and creates a call frame for the
// debugger to fill in.
//
// To inject a function call, a debugger should:
// 1. Check that the goroutine is in state _Grunning and that
//    there are at least 256 bytes free on the stack.
// 2. Push the current PC on the stack (updating SP).
// 3. Write the desired argument frame size at SP-16 (using the SP
//    after step 2).
// 4. Save all machine registers (including flags and XMM reigsters)
//    so they can be restored later by the debugger.
// 5. Set the PC to debugCallV1 and resume execution.
//
// If the goroutine is in state _Grunnable, then it's not generally
// safe to inject a call because it may return out via other runtime
// operations. Instead, the debugger should unwind the stack to find
// the return to non-runtime code, add a temporary breakpoint there,
// and inject the call once that breakpoint is hit.
//
// If the goroutine is in any other state, it's not safe to inject a call.
//
// This function communicates back to the debugger by setting RAX and
// invoking INT3 to raise a breakpoint signal. See the comments in the
// implementation for the protocol the debugger is expected to
// follow. InjectDebugCall in the runtime tests demonstrates this protocol.
//
// The debugger must ensure that any pointers passed to the function
// obey escape analysis requirements. Specifically, it must not pass
// a stack pointer to an escaping argument. debugCallV1 cannot check
// this invariant.
//
// This is ABIInternal because Go code injects its PC directly into new
// goroutine stacks.
TEXT runtime·debugCallV1<ABIInternal>(SB),NOSPLIT,$152-0
	// Save all registers that may contain pointers so they can be
	// conservatively scanned.
	//
	// We can't do anything that might clobber any of these
	// registers before this.
	MOVQ	R15, r15-(14*8+8)(SP)
	MOVQ	R14, r14-(13*8+8)(SP)
	MOVQ	R13, r13-(12*8+8)(SP)
	MOVQ	R12, r12-(11*8+8)(SP)
	MOVQ	R11, r11-(10*8+8)(SP)
	MOVQ	R10, r10-(9*8+8)(SP)
	MOVQ	R9, r9-(8*8+8)(SP)
	MOVQ	R8, r8-(7*8+8)(SP)
	MOVQ	DI, di-(6*8+8)(SP)
	MOVQ	SI, si-(5*8+8)(SP)
	MOVQ	BP, bp-(4*8+8)(SP)
	MOVQ	BX, bx-(3*8+8)(SP)
	MOVQ	DX, dx-(2*8+8)(SP)
	// Save the frame size before we clobber it. Either of the last
	// saves could clobber this depending on whether there's a saved BP.
	MOVQ	frameSize-24(FP), DX	// aka -16(RSP) before prologue
	MOVQ	CX, cx-(1*8+8)(SP)
	MOVQ	AX, ax-(0*8+8)(SP)

	// Save the argument frame size.
	MOVQ	DX, frameSize-128(SP)

	// Perform a safe-point check.
	MOVQ	retpc-8(FP), AX	// Caller's PC
	MOVQ	AX, 0(SP)
	CALL	runtime·debugCallCheck(SB)
	MOVQ	8(SP), AX
	TESTQ	AX, AX
	JZ	good
	// The safety check failed. Put the reason string at the top
	// of the stack.
	MOVQ	AX, 0(SP)
	MOVQ	16(SP), AX
	MOVQ	AX, 8(SP)
	// Set AX to 8 and invoke INT3. The debugger should get the
	// reason a call can't be injected from the top of the stack
	// and resume execution.
	MOVQ	$8, AX
	BYTE	$0xcc
	JMP	restore

good:
	// Registers are saved and it's safe to make a call.
	// Open up a call frame, moving the stack if necessary.
	//
	// Once the frame is allocated, this will set AX to 0 and
	// invoke INT3. The debugger should write the argument
	// frame for the call at SP, push the trapping PC on the
	// stack, set the PC to the function to call, set RCX to point
	// to the closure (if a closure call), and resume execution.
	//
	// If the function returns, this will set AX to 1 and invoke
	// INT3. The debugger can then inspect any return value saved
	// on the stack at SP and resume execution again.
	//
	// If the function panics, this will set AX to 2 and invoke INT3.
	// The interface{} value of the panic will be at SP. The debugger
	// can inspect the panic value and resume execution again.
#define DEBUG_CALL_DISPATCH(NAME,MAXSIZE)	\
	CMPQ	AX, $MAXSIZE;			\
	JA	5(PC);				\
	MOVQ	$NAME(SB), AX;			\
	MOVQ	AX, 0(SP);			\
	CALL	runtime·debugCallWrap(SB);	\
	JMP	restore

	MOVQ	frameSize-128(SP), AX
	DEBUG_CALL_DISPATCH(debugCall32<>, 32)
	DEBUG_CALL_DISPATCH(debugCall64<>, 64)
	DEBUG_CALL_DISPATCH(debugCall128<>, 128)
	DEBUG_CALL_DISPATCH(debugCall256<>, 256)
	DEBUG_CALL_DISPATCH(debugCall512<>, 512)
	DEBUG_CALL_DISPATCH(debugCall1024<>, 1024)
	DEBUG_CALL_DISPATCH(debugCall2048<>, 2048)
	DEBUG_CALL_DISPATCH(debugCall4096<>, 4096)
	DEBUG_CALL_DISPATCH(debugCall8192<>, 8192)
	DEBUG_CALL_DISPATCH(debugCall16384<>, 16384)
	DEBUG_CALL_DISPATCH(debugCall32768<>, 32768)
	DEBUG_CALL_DISPATCH(debugCall65536<>, 65536)
	// The frame size is too large. Report the error.
	MOVQ	$debugCallFrameTooLarge<>(SB), AX
	MOVQ	AX, 0(SP)
	MOVQ	$20, 8(SP) // length of debugCallFrameTooLarge string
	MOVQ	$8, AX
	BYTE	$0xcc
	JMP	restore

restore:
	// Calls and failures resume here.
	//
	// Set AX to 16 and invoke INT3. The debugger should restore
	// all registers except RIP and RSP and resume execution.
	MOVQ	$16, AX
	BYTE	$0xcc
	// We must not modify flags after this point.

	// Restore pointer-containing registers, which may have been
	// modified from the debugger's copy by stack copying.
	MOVQ	ax-(0*8+8)(SP), AX
	MOVQ	cx-(1*8+8)(SP), CX
	MOVQ	dx-(2*8+8)(SP), DX
	MOVQ	bx-(3*8+8)(SP), BX
	MOVQ	bp-(4*8+8)(SP), BP
	MOVQ	si-(5*8+8)(SP), SI
	MOVQ	di-(6*8+8)(SP), DI
	MOVQ	r8-(7*8+8)(SP), R8
	MOVQ	r9-(8*8+8)(SP), R9
	MOVQ	r10-(9*8+8)(SP), R10
	MOVQ	r11-(10*8+8)(SP), R11
	MOVQ	r12-(11*8+8)(SP), R12
	MOVQ	r13-(12*8+8)(SP), R13
	MOVQ	r14-(13*8+8)(SP), R14
	MOVQ	r15-(14*8+8)(SP), R15

	RET

// runtime.debugCallCheck assumes that functions defined with the
// DEBUG_CALL_FN macro are safe points to inject calls.
#define DEBUG_CALL_FN(NAME,MAXSIZE)		\
TEXT NAME(SB),WRAPPER,$MAXSIZE-0;		\
	NO_LOCAL_POINTERS;			\
	MOVQ	$0, AX;				\
	BYTE	$0xcc;				\
	MOVQ	$1, AX;				\
	BYTE	$0xcc;				\
	RET
DEBUG_CALL_FN(debugCall32<>, 32)
DEBUG_CALL_FN(debugCall64<>, 64)
DEBUG_CALL_FN(debugCall128<>, 128)
DEBUG_CALL_FN(debugCall256<>, 256)
DEBUG_CALL_FN(debugCall512<>, 512)
DEBUG_CALL_FN(debugCall1024<>, 1024)
DEBUG_CALL_FN(debugCall2048<>, 2048)
DEBUG_CALL_FN(debugCall4096<>, 4096)
DEBUG_CALL_FN(debugCall8192<>, 8192)
DEBUG_CALL_FN(debugCall16384<>, 16384)
DEBUG_CALL_FN(debugCall32768<>, 32768)
DEBUG_CALL_FN(debugCall65536<>, 65536)

// func debugCallPanicked(val interface{})
TEXT runtime·debugCallPanicked(SB),NOSPLIT,$16-16
	// Copy the panic value to the top of stack.
	MOVQ	val_type+0(FP), AX
	MOVQ	AX, 0(SP)
	MOVQ	val_data+8(FP), AX
	MOVQ	AX, 8(SP)
	MOVQ	$2, AX
	BYTE	$0xcc
	RET

// Note: these functions use a special calling convention to save generated code space.
// Arguments are passed in registers, but the space for those arguments are allocated
// in the caller's stack frame. These stubs write the args into that stack space and
// then tail call to the corresponding runtime handler.
// The tail call makes these stubs disappear in backtraces.
// Defined as ABIInternal since they do not use the stack-based Go ABI.
TEXT runtime·panicIndex<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	AX, x+0(FP)
	MOVQ	CX, y+8(FP)
	JMP	runtime·goPanicIndex(SB)
TEXT runtime·panicIndexU<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	AX, x+0(FP)
	MOVQ	CX, y+8(FP)
	JMP	runtime·goPanicIndexU(SB)
TEXT runtime·panicSliceAlen<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	CX, x+0(FP)
	MOVQ	DX, y+8(FP)
	JMP	runtime·goPanicSliceAlen(SB)
TEXT runtime·panicSliceAlenU<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	CX, x+0(FP)
	MOVQ	DX, y+8(FP)
	JMP	runtime·goPanicSliceAlenU(SB)
TEXT runtime·panicSliceAcap<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	CX, x+0(FP)
	MOVQ	DX, y+8(FP)
	JMP	runtime·goPanicSliceAcap(SB)
TEXT runtime·panicSliceAcapU<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	CX, x+0(FP)
	MOVQ	DX, y+8(FP)
	JMP	runtime·goPanicSliceAcapU(SB)
TEXT runtime·panicSliceB<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	AX, x+0(FP)
	MOVQ	CX, y+8(FP)
	JMP	runtime·goPanicSliceB(SB)
TEXT runtime·panicSliceBU<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	AX, x+0(FP)
	MOVQ	CX, y+8(FP)
	JMP	runtime·goPanicSliceBU(SB)
TEXT runtime·panicSlice3Alen<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	DX, x+0(FP)
	MOVQ	BX, y+8(FP)
	JMP	runtime·goPanicSlice3Alen(SB)
TEXT runtime·panicSlice3AlenU<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	DX, x+0(FP)
	MOVQ	BX, y+8(FP)
	JMP	runtime·goPanicSlice3AlenU(SB)
TEXT runtime·panicSlice3Acap<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	DX, x+0(FP)
	MOVQ	BX, y+8(FP)
	JMP	runtime·goPanicSlice3Acap(SB)
TEXT runtime·panicSlice3AcapU<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	DX, x+0(FP)
	MOVQ	BX, y+8(FP)
	JMP	runtime·goPanicSlice3AcapU(SB)
TEXT runtime·panicSlice3B<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	CX, x+0(FP)
	MOVQ	DX, y+8(FP)
	JMP	runtime·goPanicSlice3B(SB)
TEXT runtime·panicSlice3BU<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	CX, x+0(FP)
	MOVQ	DX, y+8(FP)
	JMP	runtime·goPanicSlice3BU(SB)
TEXT runtime·panicSlice3C<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	AX, x+0(FP)
	MOVQ	CX, y+8(FP)
	JMP	runtime·goPanicSlice3C(SB)
TEXT runtime·panicSlice3CU<ABIInternal>(SB),NOSPLIT,$0-16
	MOVQ	AX, x+0(FP)
	MOVQ	CX, y+8(FP)
	JMP	runtime·goPanicSlice3CU(SB)

#ifdef GOOS_android
// Use the free TLS_SLOT_APP slot #2 on Android Q.
// Earlier androids are set up in gcc_android.c.
DATA runtime·tls_g+0(SB)/8, $16
GLOBL runtime·tls_g+0(SB), NOPTR, $8
#endif

// The compiler and assembler's -spectre=ret mode rewrites
// all indirect CALL AX / JMP AX instructions to be
// CALL retpolineAX / JMP retpolineAX.
// See https://support.google.com/faqs/answer/7625886.
#define RETPOLINE(reg) \
	/*   CALL setup */     BYTE $0xE8; BYTE $(2+2); BYTE $0; BYTE $0; BYTE $0;	\
	/* nospec: */									\
	/*   PAUSE */           BYTE $0xF3; BYTE $0x90;					\
	/*   JMP nospec */      BYTE $0xEB; BYTE $-(2+2);				\
	/* setup: */									\
	/*   MOVQ AX, 0(SP) */  BYTE $0x48|((reg&8)>>1); BYTE $0x89;			\
	                        BYTE $0x04|((reg&7)<<3); BYTE $0x24;			\
	/*   RET */             BYTE $0xC3

TEXT runtime·retpolineAX(SB),NOSPLIT,$0; RETPOLINE(0)
TEXT runtime·retpolineCX(SB),NOSPLIT,$0; RETPOLINE(1)
TEXT runtime·retpolineDX(SB),NOSPLIT,$0; RETPOLINE(2)
TEXT runtime·retpolineBX(SB),NOSPLIT,$0; RETPOLINE(3)
/* SP is 4, can't happen / magic encodings */
TEXT runtime·retpolineBP(SB),NOSPLIT,$0; RETPOLINE(5)
TEXT runtime·retpolineSI(SB),NOSPLIT,$0; RETPOLINE(6)
TEXT runtime·retpolineDI(SB),NOSPLIT,$0; RETPOLINE(7)
TEXT runtime·retpolineR8(SB),NOSPLIT,$0; RETPOLINE(8)
TEXT runtime·retpolineR9(SB),NOSPLIT,$0; RETPOLINE(9)
TEXT runtime·retpolineR10(SB),NOSPLIT,$0; RETPOLINE(10)
TEXT runtime·retpolineR11(SB),NOSPLIT,$0; RETPOLINE(11)
TEXT runtime·retpolineR12(SB),NOSPLIT,$0; RETPOLINE(12)
TEXT runtime·retpolineR13(SB),NOSPLIT,$0; RETPOLINE(13)
TEXT runtime·retpolineR14(SB),NOSPLIT,$0; RETPOLINE(14)
TEXT runtime·retpolineR15(SB),NOSPLIT,$0; RETPOLINE(15)
