// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// defined in package runtime

// Semacquire waits until *s > 0 and then atomically decrements it.
// It is intended as a simple sleep primitive for use by the synchronization
// library and should not be used directly.
func runtime_Semacquire(s *uint32) // 注释：别名 runtime.sync_runtime_Semacquire

// SemacquireMutex is like Semacquire, but for profiling contended Mutexes.
// If lifo is true, queue waiter at the head of wait queue.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_SemacquireMutex's caller.
func runtime_SemacquireMutex(s *uint32, lifo bool, skipframes int)

// Semrelease atomically increments *s and notifies a waiting goroutine
// if one is blocked in Semacquire.
// It is intended as a simple wakeup primitive for use by the synchronization
// library and should not be used directly.
// If handoff is true, pass count directly to the first waiter.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_Semrelease's caller.
func runtime_Semrelease(s *uint32, handoff bool, skipframes int) // 注释：别名 runtime.sync_runtime_Semrelease

// See runtime/sema.go for documentation.
func runtime_notifyListAdd(l *notifyList) uint32

// See runtime/sema.go for documentation.
func runtime_notifyListWait(l *notifyList, t uint32)

// See runtime/sema.go for documentation.
func runtime_notifyListNotifyAll(l *notifyList)

// See runtime/sema.go for documentation.
func runtime_notifyListNotifyOne(l *notifyList)

// Ensure that sync and runtime agree on size of notifyList.
func runtime_notifyListCheck(size uintptr)
func init() {
	var n notifyList
	runtime_notifyListCheck(unsafe.Sizeof(n))
}

// 注释：运行在多 CPU 的机器上；
// 注释：当前 Goroutine 为了获取该锁进入自旋的次数小于四次；
// 注释：当前机器上至少存在一个正在运行的处理器 P
// 注释：并且处理的运行队列为空；
// Active spinning runtime support.
// runtime_canSpin reports whether spinning makes sense at the moment.
func runtime_canSpin(i int) bool // 注释：sync.runtime_canSpin 是 runtime.sync_runtime_canSpin 的别名

// 注释：执行30次PAUSE系统指令，该指令只会占用 CPU 并消耗 CPU 时间
// runtime_doSpin does active spinning.
func runtime_doSpin() // 注释：sync.runtime_doSpin 是 runtime.sync_runtime_doSpin 的别名

func runtime_nanotime() int64
