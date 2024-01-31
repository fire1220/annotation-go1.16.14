# annotation-go1.16.14 
go1.16.14中文注释


- 备注内容都是以"注释："为前缀的
- 代码中没有看明白的地方用"【纳闷】"标注了
- 代码中"【ing】"表示正在看的位置或下次要看的位置

### 已经备注的内容
- map
- 切片
- 管道
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

