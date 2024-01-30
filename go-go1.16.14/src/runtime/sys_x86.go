// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build amd64 386

package runtime

import (
	"runtime/internal/sys"
	"unsafe"
)

// adjust Gobuf as if it executed a call to fn with context ctxt
// and then did an immediate gosave.
// 注释：保存现场，保存:SP栈基地址、PC指令计数器、ctxt上下文（调用方函数指针，用来链路追踪）
func gostartcall(buf *gobuf, fn, ctxt unsafe.Pointer) {
	sp := buf.sp                   // 注释：还原SP栈地址（栈顶地址）
	if sys.RegSize > sys.PtrSize { // 注释：如果成立说明是32位系统
		sp -= sys.PtrSize                   // 注释：32系统则需要扩栈32位，保证sp占用64位
		*(*uintptr)(unsafe.Pointer(sp)) = 0 // 注释：清空SP
	}
	sp -= sys.PtrSize                        // 注释：SP向下（低地址）移动一个指针
	*(*uintptr)(unsafe.Pointer(sp)) = buf.pc // 注释：【ing】把buf.pc放到sp里（此时是向下移动一个指针之后的位置，其实就是return addr的位置，一般是LR寄存器存储的地址）
	buf.sp = sp
	buf.pc = uintptr(fn) // 注释：PC指令计数器(如果ctxt存在值则该值为ctxt)
	buf.ctxt = ctxt      // 注释：ctxt上下文（调用方函数指针，用来链路追踪）
}
