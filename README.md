# annotation-go1.16.14 
go1.16.14中文注释


- 备注内容都是以"注释："为前缀的
- 代码中没有看明白的地方用"【纳闷】"标注了
- 代码中"【ing】"表示正在看的位置或下次要看的位置

### 已经备注的内容
- map
- slice
- channel
- 启动流程 - 调度循环
- runtime·rt0_go【汇编】程序入口
- runtime.newproc用协成执行fn
- Syscall【汇编】系统调用
- runtime·mcall【汇编】切换G0执行fn,永不返回
- atomic·Xadd64【汇编】交换相加后返回
- runtime·systemstack【汇编】切换G0系统栈执行fn
- runtime·gosave【汇编】保存现场
- runtime·gogo【汇编】执行G协成
- runtime·goexit【汇编】协成退出
- runtime.mallocgc 内存分配

### go语句对应执行的函数；语句 -> 函数（函数位置）
- defer -> deferproc (src/runtime/panic.go)
- go -> newproc (src/runtime/proc.go)
- panic -> gopanic (src/runtime/panic.go)
- recover -> gorecover (src/runtime/panic.go)
- make语句，包含三种情况，分别是slice、map、chan
  - make(slice) -> makeslice (src/runtime/slice.go)
  - make(map) -> makemap (src/runtime/map.go)
  - make(chan) -> makechan (src/runtime/chan.go)
- append语句，包含三总格式
  - append(y, make([]T, y)...) -> extendslice (src/cmd/compile/internal/gc/walk.go)
  - append(src，x...) -> appendslice (src/cmd/compile/internal/gc/walk.go)
  - append(src，x，y，z) -> walkappend (src/cmd/compile/internal/gc/walk.go)
  - 如果发生扩容时 -> growslice (src/runtime/slice.go)
- chan管道操作
  - 读取操作 -> chanrecv (src/runtime/chan.go)
  - 写入操作 -> chansend (src/runtime/chan.go)
- map操作
  - 读取操作:包含两种格式
    - v := m["xx"] -> mapaccess1 (src/runtime/map.go)
    - v,ok := m["xx"] -> mapaccess2 (src/runtime/map.go)
  - 写入操作 -> mapassign (src/runtime/map.go)

### go调度流程
- runtime·rt0_go【汇编】程序入口 (src/runtime/asm_amd64.s)
  - runtime·args(SB)      处理args
  - runtime·osinit(SB)    os初始化(os_linux.go)
  - runtime·schedinit(SB) 调度系统初始化(proc.go)(会进行p的初始化，也会把m0和某个p绑定)
  - runtime·newproc(SB)   将runtime.main函数放进新建的g里，然后插入m0绑定的p的本地队列里
  - runtime·mstart(SB)    启动线程m，进入启动调度系统（开始执行调度和业务代码），永不返回


