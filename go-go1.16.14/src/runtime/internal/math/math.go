// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package math

import "runtime/internal/sys"

const MaxUintptr = ^uintptr(0)

// MulUintptr returns a * b and whether the multiplication overflowed.
// On supported platforms this is an intrinsic lowered by the compiler.
// 注释：指针相乘，返回相乘的结果和是否溢出
func MulUintptr(a, b uintptr) (uintptr, bool) {
	// 注释：判断是否溢出，a和b的最大长度要小于2的32次方（1<<16），或2的64次方（1<<32）
	if a|b < 1<<(4*sys.PtrSize) || a == 0 {
		return a * b, false
	}
	// 注释：如果b>(指针的最大值/a)说明存在溢出
	overflow := b > MaxUintptr/a
	return a * b, overflow
}
