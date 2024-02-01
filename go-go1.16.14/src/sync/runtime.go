// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import "unsafe"

// defined in package runtime

// Semacquire waits until *s > 0 and then atomically decrements it.
// It is intended as a simple sleep primitive for use by the synchronization
// library and should not be used directly.
// 注释：译：Semaquire等待*s>0，然后原子递减。它是一个简单的睡眠原语，供同步库使用，不应直接使用。
func runtime_Semacquire(s *uint32) // 注释：获取信号量，别名 runtime.sync_runtime_Semacquire

// SemacquireMutex is like Semacquire, but for profiling contended Mutexes.
// If lifo is true, queue waiter at the head of wait queue.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_SemacquireMutex's caller.
// 注释：译：SemaquireMuex与Semaquire类似，但用于分析争用的Mutex。如果lifo是真的，请将服务员排在等候队伍的最前面。
//		skipframes是在跟踪过程中要忽略的帧数，从runtime_SemaquireMutex的调用方开始计数。
func runtime_SemacquireMutex(s *uint32, lifo bool, skipframes int)

// Semrelease atomically increments *s and notifies a waiting goroutine
// if one is blocked in Semacquire.
// It is intended as a simple wakeup primitive for use by the synchronization
// library and should not be used directly.
// If handoff is true, pass count directly to the first waiter.
// skipframes is the number of frames to omit during tracing, counting from
// runtime_Semrelease's caller.
// 注释：译：Semrelease自动递增*s，并在Semaquire中阻塞时通知等待的goroutine。它旨在作为同步库使用的简单唤醒原语，不应直接使用。
//		如果切换为真，则将计数直接传递给第一个服务员。skipframes是在跟踪过程中要忽略的帧数，从runtime_Semrelase的调用方开始计数。
func runtime_Semrelease(s *uint32, handoff bool, skipframes int) // 注释：信号量释放，别名 runtime.sync_runtime_Semrelease

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
