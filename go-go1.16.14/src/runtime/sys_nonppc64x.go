// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build !ppc64,!ppc64le

package runtime

// 注释：非PPC架构是执行,这是个空方法，就是什么都不做
func prepGoExitFrame(sp uintptr) {
}
