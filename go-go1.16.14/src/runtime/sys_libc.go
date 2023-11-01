// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build darwin openbsd,amd64 openbsd,arm64

package runtime

import "unsafe"

// Call fn with arg as its argument. Return what fn returns.  // 注释：使用arg作为参数调用fn。返回fn返回的内容。
// fn is the raw pc value of the entry point of the desired function.  // 注释：fn是所需函数的入口点的原始pc值。
// Switches to the system stack, if not already there.  // 注释：切换到系统堆栈（如果还没有）。
// Preserves the calling point as the location where a profiler traceback will begin. // 注释：将调用点保留为探查器回溯开始的位置。
//go:nosplit
func libcCall(fn, arg unsafe.Pointer) int32 {
	// Leave caller's PC/SP/G around for traceback.
	gp := getg()
	var mp *m
	if gp != nil {
		mp = gp.m
	}
	if mp != nil && mp.libcallsp == 0 {
		mp.libcallg.set(gp)
		mp.libcallpc = getcallerpc()
		// sp must be the last, because once async cpu profiler finds
		// all three values to be non-zero, it will use them
		mp.libcallsp = getcallersp()
	} else {
		// Make sure we don't reset libcallsp. This makes
		// libcCall reentrant; We remember the g/pc/sp for the
		// first call on an M, until that libcCall instance
		// returns.  Reentrance only matters for signals, as
		// libc never calls back into Go.  The tricky case is
		// where we call libcX from an M and record g/pc/sp.
		// Before that call returns, a signal arrives on the
		// same M and the signal handling code calls another
		// libc function.  We don't want that second libcCall
		// from within the handler to be recorded, and we
		// don't want that call's completion to zero
		// libcallsp.
		// We don't need to set libcall* while we're in a sighandler
		// (even if we're not currently in libc) because we block all
		// signals while we're handling a signal. That includes the
		// profile signal, which is the one that uses the libcall* info.
		mp = nil
	}
	res := asmcgocall(fn, arg)
	if mp != nil {
		mp.libcallsp = 0
	}
	return res
}
