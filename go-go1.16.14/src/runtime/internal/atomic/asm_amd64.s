// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Note: some of these functions are semantically inlined
// by the compiler (in src/cmd/compile/internal/gc/ssa.go).

#include "textflag.h"

// bool Cas(int32 *val, int32 old, int32 new)
// Atomically:
//	if(*val == old){
//		*val = new;
//		return 1;
//	} else
//		return 0;
TEXT runtime∕internal∕atomic·Cas(SB),NOSPLIT,$0-17
	MOVQ	ptr+0(FP), BX
	MOVL	old+8(FP), AX
	MOVL	new+12(FP), CX
	LOCK
	CMPXCHGL	CX, 0(BX)
	SETEQ	ret+16(FP)
	RET

// bool	runtime∕internal∕atomic·Cas64(uint64 *val, uint64 old, uint64 new)
// Atomically:
//	if(*val == *old){
//		*val = new;
//		return 1;
//	} else {
//		return 0;
//	}
TEXT runtime∕internal∕atomic·Cas64(SB), NOSPLIT, $0-25
	MOVQ	ptr+0(FP), BX
	MOVQ	old+8(FP), AX
	MOVQ	new+16(FP), CX
	LOCK
	CMPXCHGQ	CX, 0(BX)
	SETEQ	ret+24(FP)
	RET

TEXT runtime∕internal∕atomic·Casuintptr(SB), NOSPLIT, $0-25
	JMP	runtime∕internal∕atomic·Cas64(SB)

TEXT runtime∕internal∕atomic·CasRel(SB), NOSPLIT, $0-17
	JMP	runtime∕internal∕atomic·Cas(SB)

TEXT runtime∕internal∕atomic·Loaduintptr(SB), NOSPLIT, $0-16
	JMP	runtime∕internal∕atomic·Load64(SB)

TEXT runtime∕internal∕atomic·Loaduint(SB), NOSPLIT, $0-16
	JMP	runtime∕internal∕atomic·Load64(SB)

TEXT runtime∕internal∕atomic·Storeuintptr(SB), NOSPLIT, $0-16
	JMP	runtime∕internal∕atomic·Store64(SB)

TEXT runtime∕internal∕atomic·Loadint64(SB), NOSPLIT, $0-16
	JMP	runtime∕internal∕atomic·Load64(SB)

TEXT runtime∕internal∕atomic·Xaddint64(SB), NOSPLIT, $0-24
	JMP	runtime∕internal∕atomic·Xadd64(SB)

// bool Casp1(void **val, void *old, void *new)
// Atomically:
//	if(*val == old){
//		*val = new;
//		return 1;
//	} else
//		return 0;
TEXT runtime∕internal∕atomic·Casp1(SB), NOSPLIT, $0-25
	MOVQ	ptr+0(FP), BX
	MOVQ	old+8(FP), AX
	MOVQ	new+16(FP), CX
	LOCK
	CMPXCHGQ	CX, 0(BX)
	SETEQ	ret+24(FP)
	RET

// uint32 Xadd(uint32 volatile *val, int32 delta)
// Atomically:
//	*val += delta;
//	return *val;
// 注释：(原子操作)交换相加后返回相加后的结果
TEXT runtime∕internal∕atomic·Xadd(SB), NOSPLIT, $0-20
	MOVQ	ptr+0(FP), BX   // 注释：参数1：(ptr)是个指针
	MOVL	delta+8(FP), AX // 注释：参数2：(delta)需要相加的数据
	MOVL	AX, CX          // 注释：(备份delta数据)把AX值复制到CX寄存器里（把参数2(delta)放到CX里）
	LOCK                    // 注释：同步锁，保证寄存器在CPU缓存中的数据是一致的
	XADDL	AX, 0(BX)       // 注释： 注释：(原子操作,两值调换相加),结果：0(BX) = AX + 0(BX)； AX = 0(BX),AX是ptr指针里的旧值
	ADDL	CX, AX          // 注释：【AX = delta旧值(CX) + ptr旧值(AX)】把0(BX)内存数据原子操作时的前数据拿出来和CX相加（CX就是函数的第二个参数），防止再次读取内存破坏原子性
	MOVL	AX, ret+16(FP)  // 注释：返回原子相加后的值（是针对内存的原子操作）
	RET                     // 注释：函数退出

// 注释：(原子操作)交换相加后返回相加后的结果
TEXT runtime∕internal∕atomic·Xadd64(SB), NOSPLIT, $0-24
	MOVQ	ptr+0(FP), BX           // 注释：参数1：(ptr)是个指针
	MOVQ	delta+8(FP), AX         // 注释：参数2：(delta)需要相加的数据
	MOVQ	AX, CX                  // 注释：(备份delta数据)把AX值复制到CX寄存器里（把参数2(delta)放到CX里）
	LOCK                            // 注释：（对总线和缓存上锁）强制所有lock信号之前的指令，都在此之前被执行，并同步相关缓存
	XADDQ	AX, 0(BX)               // 注释：(原子操作,两值调换相加),结果：0(BX) = AX + 0(BX)； AX = 0(BX),AX是ptr指针里的旧值
	ADDQ	CX, AX                  // 注释：【AX = delta旧值(CX) + ptr旧值(AX)】把0(BX)内存数据原子操作时的前数据拿出来和CX相加（CX就是函数的第二个参数），防止再次读取内存破坏原子性
	MOVQ	AX, ret+16(FP)          // 注释：注释：返回原子相加后的值（是针对内存的原子操作）
	RET

TEXT runtime∕internal∕atomic·Xadduintptr(SB), NOSPLIT, $0-24
	JMP	runtime∕internal∕atomic·Xadd64(SB)

// 注释：原子操作，交换位置并且返回，把ptr指针里的值和new的相互交换后返回ptr指针对应的值
TEXT runtime∕internal∕atomic·Xchg(SB), NOSPLIT, $0-20
	MOVQ	ptr+0(FP), BX   // 注释：接收第一个参数（ptr）放到BX寄存器里
	MOVL	new+8(FP), AX   // 注释：把第二个参数（new）放到寄存器AX里
	XCHGL	AX, 0(BX)       // 注释：原子操作，把0(BX)值和AX值交换
	MOVL	AX, ret+16(FP)  // 注释：把AX值放到返回值ret+16(FP)中（第三个参数（返回值参数））里
	RET

TEXT runtime∕internal∕atomic·Xchg64(SB), NOSPLIT, $0-24
	MOVQ	ptr+0(FP), BX
	MOVQ	new+8(FP), AX
	XCHGQ	AX, 0(BX)
	MOVQ	AX, ret+16(FP)
	RET

TEXT runtime∕internal∕atomic·Xchguintptr(SB), NOSPLIT, $0-24
	JMP	runtime∕internal∕atomic·Xchg64(SB)

TEXT runtime∕internal∕atomic·StorepNoWB(SB), NOSPLIT, $0-16
	MOVQ	ptr+0(FP), BX
	MOVQ	val+8(FP), AX
	XCHGQ	AX, 0(BX)
	RET

TEXT runtime∕internal∕atomic·Store(SB), NOSPLIT, $0-12
	MOVQ	ptr+0(FP), BX
	MOVL	val+8(FP), AX
	XCHGL	AX, 0(BX)
	RET

TEXT runtime∕internal∕atomic·StoreRel(SB), NOSPLIT, $0-12
	JMP	runtime∕internal∕atomic·Store(SB)

TEXT runtime∕internal∕atomic·StoreRel64(SB), NOSPLIT, $0-16
	JMP	runtime∕internal∕atomic·Store64(SB)

TEXT runtime∕internal∕atomic·StoreReluintptr(SB), NOSPLIT, $0-16
	JMP	runtime∕internal∕atomic·Store64(SB)

TEXT runtime∕internal∕atomic·Store8(SB), NOSPLIT, $0-9
	MOVQ	ptr+0(FP), BX
	MOVB	val+8(FP), AX
	XCHGB	AX, 0(BX)
	RET

TEXT runtime∕internal∕atomic·Store64(SB), NOSPLIT, $0-16
	MOVQ	ptr+0(FP), BX
	MOVQ	val+8(FP), AX
	XCHGQ	AX, 0(BX)
	RET

// void	runtime∕internal∕atomic·Or8(byte volatile*, byte);
TEXT runtime∕internal∕atomic·Or8(SB), NOSPLIT, $0-9
	MOVQ	ptr+0(FP), AX
	MOVB	val+8(FP), BX
	LOCK
	ORB	BX, (AX)
	RET

// void	runtime∕internal∕atomic·And8(byte volatile*, byte);
TEXT runtime∕internal∕atomic·And8(SB), NOSPLIT, $0-9
	MOVQ	ptr+0(FP), AX
	MOVB	val+8(FP), BX
	LOCK
	ANDB	BX, (AX)
	RET

// func Or(addr *uint32, v uint32)
TEXT runtime∕internal∕atomic·Or(SB), NOSPLIT, $0-12
	MOVQ	ptr+0(FP), AX
	MOVL	val+8(FP), BX
	LOCK
	ORL	BX, (AX)
	RET

// func And(addr *uint32, v uint32)
TEXT runtime∕internal∕atomic·And(SB), NOSPLIT, $0-12
	MOVQ	ptr+0(FP), AX
	MOVL	val+8(FP), BX
	LOCK
	ANDL	BX, (AX)
	RET
