# code-remarks-go1.16.14 
go1.16.14源码分析备注

### 已经备注的内容
- map
  - hmap核心结构体；bmap等相关结构体
  - makemap
  - 读取和写入
  - 哈希冲突
  

- 切片
  - 切片扩容规则
  
  
- 字符串


- 管道
  - hchan等相关结构体
  - makechan
  - chansend发送数据（写入数据）
  

- 启动流程
  - runtime.mstart 
  - runtime.mstart1
  - runtime.mstart0
  - runtime.schedule


- 调度器:runtime.schedule
  - runtime.schedule 调度器开始
  - runtime.globrunqget(每61次执行一下,到全局队列里拿一个)
  - runtime.runqget 本地队列拿
  - runtime.findrunnable 尝试从其他地方拿
  - runtime.globrunqget 全局队列里拿,min(全局G数量/CPU核数, 本地队列长度/2)
  - runtime.netpoll 从网络轮询里拿（优化方案）
  - runtime.runqsteal 从其他线程M的本地队列里窃取
