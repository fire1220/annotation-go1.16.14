// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "textflag.h"
#include "funcdata.h"

//
// System calls for AMD64, Linux
//

#define SYS_gettimeofday 96

// func Syscall(trap int64, a1, a2, a3 uintptr) (r1, r2, err uintptr);
// Trap # in AX, args in DI SI DX R10 R8 R9, return in AX DX
// Note that this differs from "standard" ABI convention, which
// would pass 4th arg in CX, not R10.
// 注释：Linux-amd64系统调用的规范:系统调用编号有寄存器AX传输，系统调用的前6个参数直接由寄存器DI、SI、DX、R10、R8和R9传输，结果由寄存器AX和DX返回
// 注释：macOS 等类 UINX 系统调用的参数传输大多数都采用类似的规则。

// 注释：系统调用：第一个参数会放到AX中表示"系统调用编号"；第二个到第四个会放到DI，SI，DX中表示调的参数；返回值有三个分别是系统调用返回的两个参数和一个err
TEXT ·Syscall(SB),NOSPLIT,$0-56         // 注释：函数名Syscall；NOSPLIT：函数不分裂；$0：自动计算函数栈空间大小；56：函数出入参数大小为56字节
    // 注释：执行系统调用的前置工作，实现位置：go-go1.16.14/src/runtime/proc.go
    // 注释：主要动作：标记栈抢占请求； 禁止栈拆分； 保存现场（PC、SP和G）； 栈追踪； 唤醒等待的M； 安全节点检查避免数据竞争；把当前P放到m.oldp里；解除M和P的绑定；
    // 注释：把P的状态设置成系统调用(_Psyscall)；判断是否开启GC，如果GC开启则把当前的P进入停止，如果当前的P是最后一个P时则运行GC，P的数量默认是系统核数
	CALL	runtime·entersyscall(SB)    // 注释：执行系统调用的前置工作
	MOVQ	a1+8(FP), DI                // 注释：函数入参的第2个参数放到DI寄存器里，DI是系统调用API的第一个参数
	MOVQ	a2+16(FP), SI               // 注释：函数入参的第3个参数放到SI寄存器里，SI是系统调用API的第二个参数
	MOVQ	a3+24(FP), DX               // 注释：函数入参的第4个参数放到DX寄存器里，DX是系统调用API的第三个参数
	MOVQ	trap+0(FP), AX	            // 注释：函数入参的第1个参数放到AX寄存器里，AX是系统调用API存储“系统调用编号”的寄存器 // syscall entry
	SYSCALL                             // 注释：执行系统调用
	CMPQ	AX, $0xfffffffffffff001     // 注释：系统调用编号边界值比较（如果）
	JLS	ok                              // 注释：系统调用编号边界值比较，如果小于边界值，则跳到ok标签处，否则继续向下执行
	MOVQ	$-1, r1+32(FP)              // 注释：把-1放到第一个返回值里。（此处以后表示系统调用失败的处理）
	MOVQ	$0, r2+40(FP)               // 注释：把0放到第二个返回值里
	NEGQ	AX                          // 注释：把AX寄存器的值取反（AX = -AX）
	MOVQ	AX, err+48(FP)              // 注释：把取反后的AX值放到第三个返回值里
	CALL	runtime·exitsyscall(SB)     // 注释：执行系统后的后置动作【ing】
	RET                                 // 注释：函数返回
ok:                                     // 注释：该标签表示系统调用成功
	MOVQ	AX, r1+32(FP)               // 注释：把AX(系统调用的第1个返回值)的值放到第一个返回值里
	MOVQ	DX, r2+40(FP)               // 注释：把DX(系统调用的第2个返回值)的值放到第二个返回值里
	MOVQ	$0, err+48(FP)              // 注释：把0放到第三个返回值里
	CALL	runtime·exitsyscall(SB)     // 注释：执行系统后的后置动作
	RET                                 // 注释：函数返回

// func Syscall6(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2, err uintptr)
TEXT ·Syscall6(SB),NOSPLIT,$0-80
	CALL	runtime·entersyscall(SB)
	MOVQ	a1+8(FP), DI
	MOVQ	a2+16(FP), SI
	MOVQ	a3+24(FP), DX
	MOVQ	a4+32(FP), R10
	MOVQ	a5+40(FP), R8
	MOVQ	a6+48(FP), R9
	MOVQ	trap+0(FP), AX	// syscall entry
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	ok6
	MOVQ	$-1, r1+56(FP)
	MOVQ	$0, r2+64(FP)
	NEGQ	AX
	MOVQ	AX, err+72(FP)
	CALL	runtime·exitsyscall(SB)
	RET
ok6:
	MOVQ	AX, r1+56(FP)
	MOVQ	DX, r2+64(FP)
	MOVQ	$0, err+72(FP)
	CALL	runtime·exitsyscall(SB)
	RET

// func RawSyscall(trap, a1, a2, a3 uintptr) (r1, r2, err uintptr)
TEXT ·RawSyscall(SB),NOSPLIT,$0-56
	MOVQ	a1+8(FP), DI
	MOVQ	a2+16(FP), SI
	MOVQ	a3+24(FP), DX
	MOVQ	trap+0(FP), AX	// syscall entry
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	ok1
	MOVQ	$-1, r1+32(FP)
	MOVQ	$0, r2+40(FP)
	NEGQ	AX
	MOVQ	AX, err+48(FP)
	RET
ok1:
	MOVQ	AX, r1+32(FP)
	MOVQ	DX, r2+40(FP)
	MOVQ	$0, err+48(FP)
	RET

// func RawSyscall6(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2, err uintptr)
TEXT ·RawSyscall6(SB),NOSPLIT,$0-80
	MOVQ	a1+8(FP), DI
	MOVQ	a2+16(FP), SI
	MOVQ	a3+24(FP), DX
	MOVQ	a4+32(FP), R10
	MOVQ	a5+40(FP), R8
	MOVQ	a6+48(FP), R9
	MOVQ	trap+0(FP), AX	// syscall entry
	SYSCALL
	CMPQ	AX, $0xfffffffffffff001
	JLS	ok2
	MOVQ	$-1, r1+56(FP)
	MOVQ	$0, r2+64(FP)
	NEGQ	AX
	MOVQ	AX, err+72(FP)
	RET
ok2:
	MOVQ	AX, r1+56(FP)
	MOVQ	DX, r2+64(FP)
	MOVQ	$0, err+72(FP)
	RET

// func rawVforkSyscall(trap, a1 uintptr) (r1, err uintptr)
TEXT ·rawVforkSyscall(SB),NOSPLIT,$0-32
	MOVQ	a1+8(FP), DI
	MOVQ	$0, SI
	MOVQ	$0, DX
	MOVQ	$0, R10
	MOVQ	$0, R8
	MOVQ	$0, R9
	MOVQ	trap+0(FP), AX	// syscall entry
	POPQ	R12 // preserve return address
	SYSCALL
	PUSHQ	R12
	CMPQ	AX, $0xfffffffffffff001
	JLS	ok2
	MOVQ	$-1, r1+16(FP)
	NEGQ	AX
	MOVQ	AX, err+24(FP)
	RET
ok2:
	MOVQ	AX, r1+16(FP)
	MOVQ	$0, err+24(FP)
	RET

// func rawSyscallNoError(trap, a1, a2, a3 uintptr) (r1, r2 uintptr)
TEXT ·rawSyscallNoError(SB),NOSPLIT,$0-48
	MOVQ	a1+8(FP), DI
	MOVQ	a2+16(FP), SI
	MOVQ	a3+24(FP), DX
	MOVQ	trap+0(FP), AX	// syscall entry
	SYSCALL
	MOVQ	AX, r1+32(FP)
	MOVQ	DX, r2+40(FP)
	RET

// func gettimeofday(tv *Timeval) (err uintptr)
TEXT ·gettimeofday(SB),NOSPLIT,$0-16
	MOVQ	tv+0(FP), DI
	MOVQ	$0, SI
	MOVQ	runtime·vdsoGettimeofdaySym(SB), AX
	TESTQ   AX, AX
	JZ fallback
	CALL	AX
ret:
	CMPQ	AX, $0xfffffffffffff001
	JLS	ok7
	NEGQ	AX
	MOVQ	AX, err+8(FP)
	RET
fallback:
	MOVL	$SYS_gettimeofday, AX
	SYSCALL
	JMP ret
ok7:
	MOVQ	$0, err+8(FP)
	RET
