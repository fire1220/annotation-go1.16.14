// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#ifdef GOARCH_arm
#define LR R14
#endif

// 注释：(AMD64)定义get_tls为获取TLS寄存器数据
#ifdef GOARCH_amd64
#define	get_tls(r)	MOVQ TLS, r
#define	g(r)	0(r)(TLS*1)
#endif

// 注释：(AMD32)定义get_tls为获取TLS寄存器数据
#ifdef GOARCH_386
#define	get_tls(r)	MOVL TLS, r
#define	g(r)	0(r)(TLS*1)
#endif
