// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build 386 amd64 s390x arm arm64 ppc64 ppc64le mips mipsle mips64 mips64le riscv64 wasm

package bytealg

//go:noescape
func IndexByte(b []byte, c byte) int

//go:noescape
func IndexByteString(s string, c byte) int // 注释：返回比较字节字符在字符串中出现的位置(从0开始),没有找到返回-1，例如：x := IndexByteString("hello world", 'o')；这是x为4
