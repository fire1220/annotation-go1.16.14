// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include "go_asm.h"
#include "textflag.h"

TEXT	·IndexByte(SB), NOSPLIT, $0-40
	MOVQ b_base+0(FP), SI
	MOVQ b_len+8(FP), BX
	MOVB c+24(FP), AL
	LEAQ ret+32(FP), R8
	JMP  indexbytebody<>(SB)

// 注释：返回比较字节字符在字符串中出现的位置(从0开始),没有找到返回-1，例如：x := IndexByteString("hello world", 'o')；这是x为4
TEXT	·IndexByteString(SB), NOSPLIT, $0-32
	MOVQ s_base+0(FP), SI           // 注释：参数一，go字符串的str，放到寄存器SI中
	MOVQ s_len+8(FP), BX            // 注释：参数一，go字符串的len，放到寄存器BX中
	MOVB c+16(FP), AL               // 注释：参数二，要查找的字节字符，放到寄存器AL中
	LEAQ ret+24(FP), R8             // 注释：返回值指针放到寄存器R8中
	JMP  indexbytebody<>(SB)        // 注释：执行函数，修改返回值寄存器R8中的指针（返回值赋值），并返回

// input:
//   SI: data
//   BX: data len
//   AL: byte sought
//   R8: address to put result
// 注释：在字符串中查找字符，入参都在寄存器里，寄存器： SI字符串数据，BX字符串长度，AL要查找的字节字符，R8返回数据的指针
TEXT	indexbytebody<>(SB), NOSPLIT, $0
	// Shuffle X0 around so that each byte contains
	// the character we're looking for.
	// 注释：译：打乱X0，使每个字节都包含我们要查找的字符。
	MOVD AX, X0
	PUNPCKLBW X0, X0
	PUNPCKLBW X0, X0
	PSHUFL $0, X0, X0

	CMPQ BX, $16
	JLT small               // 注释：小于16时处理

	MOVQ SI, DI

	CMPQ BX, $32
	JA avx2                 // 注释：小于等于32时处理
sse:
	LEAQ	-16(SI)(BX*1), AX	// AX = address of last 16 bytes
	JMP	sseloopentry

sseloop:
	// Move the next 16-byte chunk of the data into X1.
	MOVOU	(DI), X1
	// Compare bytes in X0 to X1.
	PCMPEQB	X0, X1
	// Take the top bit of each byte in X1 and put the result in DX.
	PMOVMSKB X1, DX
	// Find first set bit, if any.
	BSFL	DX, DX
	JNZ	ssesuccess
	// Advance to next block.
	ADDQ	$16, DI
sseloopentry:
	CMPQ	DI, AX
	JB	sseloop

	// Search the last 16-byte chunk. This chunk may overlap with the
	// chunks we've already searched, but that's ok.
	MOVQ	AX, DI
	MOVOU	(AX), X1
	PCMPEQB	X0, X1
	PMOVMSKB X1, DX
	BSFL	DX, DX
	JNZ	ssesuccess

failure:
	MOVQ $-1, (R8)
	RET

// We've found a chunk containing the byte.
// The chunk was loaded from DI.
// The index of the matching byte in the chunk is DX.
// The start of the data is SI.
ssesuccess:
	SUBQ SI, DI	// Compute offset of chunk within data.
	ADDQ DX, DI	// Add offset of byte within chunk.
	MOVQ DI, (R8)
	RET

// handle for lengths < 16
small:
	TESTQ	BX, BX // 注释：BX & BX ，值如果是0，会设置比较寄存器为1（true，表示相等）
	JEQ	failure // 注释：BX非空时返回-1(没有找到返回-1)

	// Check if we'll load across a page boundary.
	// 注释：译：检查我们是否要跨越页边界加载。
	LEAQ	16(SI), AX
	TESTW	$0xff0, AX
	JEQ	endofpage

	MOVOU	(SI), X1 // Load data                                   // 注释：译：加载数据
	PCMPEQB	X0, X1	// Compare target byte with each byte in data.  // 注释：译：将目标字节与数据中的每个字节进行比较
	PMOVMSKB X1, DX	// Move result bits to integer register.        // 注释：译：将结果位移动到整数寄存器
	BSFL	DX, DX	// Find first set bit.                          // 注释：查找第一个设置位，从右向左扫描DX(第二个)第一个含"1"的位，并把位号放到DX(第一个)里(位号从0开始)
	JZ	failure	// No set bit, failure.                             // 注释：译：无设定位，故障(没有找到返回-1)
	CMPL	DX, BX                                                  // 注释：比较查找后的位置下标和字符串长度，如果位置下标>=字符串长度则返回-1
	JAE	failure	// Match is past end of data.                       // 注释：译：匹配已过数据末尾(没有找到返回-1)
	MOVQ	DX, (R8)                                                // 注释：把查找后的下标（位号）放到返回值寄存器R8里
	RET

endofpage:
	MOVOU	-16(SI)(BX*1), X1	// Load data into the high end of X1.
	PCMPEQB	X0, X1	// Compare target byte with each byte in data.
	PMOVMSKB X1, DX	// Move result bits to integer register.
	MOVL	BX, CX
	SHLL	CX, DX
	SHRL	$16, DX	// Shift desired bits down to bottom of register.
	BSFL	DX, DX	// Find first set bit.
	JZ	failure	// No set bit, failure.
	MOVQ	DX, (R8)
	RET

avx2:
	CMPB   internal∕cpu·X86+const_offsetX86HasAVX2(SB), $1
	JNE sse
	MOVD AX, X0
	LEAQ -32(SI)(BX*1), R11
	VPBROADCASTB  X0, Y1
avx2_loop:
	VMOVDQU (DI), Y2
	VPCMPEQB Y1, Y2, Y3
	VPTEST Y3, Y3
	JNZ avx2success
	ADDQ $32, DI
	CMPQ DI, R11
	JLT avx2_loop
	MOVQ R11, DI
	VMOVDQU (DI), Y2
	VPCMPEQB Y1, Y2, Y3
	VPTEST Y3, Y3
	JNZ avx2success
	VZEROUPPER
	MOVQ $-1, (R8)
	RET

avx2success:
	VPMOVMSKB Y3, DX
	BSFL DX, DX
	SUBQ SI, DI
	ADDQ DI, DX
	MOVQ DX, (R8)
	VZEROUPPER
	RET
