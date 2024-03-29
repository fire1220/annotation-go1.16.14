// mksysnum_linux.pl /usr/include/asm/unistd_64.h
// Code generated by the command above; DO NOT EDIT.

// +build amd64,linux
// 注释：Linux系统调用编号

package syscall

const (
	SYS_READ                   = 0  // 读文件
	SYS_WRITE                  = 1  // 写文件
	SYS_OPEN                   = 2  // 打开文件
	SYS_CLOSE                  = 3  // 关闭文件描述字
	SYS_STAT                   = 4  // 取文件状态信息
	SYS_FSTAT                  = 5  // 参见stat
	SYS_LSTAT                  = 6  // 参见stat
	SYS_POLL                   = 7  // I/O多路转换
	SYS_LSEEK                  = 8  // 移动文件指针
	SYS_MMAP                   = 9  // 映射虚拟内存页
	SYS_MPROTECT               = 10 // 设置内存映像保护
	SYS_MUNMAP                 = 11 // 去除内存页映射
	SYS_BRK                    = 12 // 改变数据段空间的分配
	SYS_RT_SIGACTION           = 13
	SYS_RT_SIGPROCMASK         = 14
	SYS_RT_SIGRETURN           = 15
	SYS_IOCTL                  = 16 // I/O总控制函数
	SYS_PREAD64                = 17
	SYS_PWRITE64               = 18
	SYS_READV                  = 19 // 从文件读入数据到缓冲数组中
	SYS_WRITEV                 = 20 // 将缓冲数组里的数据写入文件
	SYS_ACCESS                 = 21 // 确定文件的可存取性
	SYS_PIPE                   = 22 // 创建管道
	SYS_SELECT                 = 23 // 对多路同步I/O进行轮询
	SYS_SCHED_YIELD            = 24 // 进程主动让出处理器,并将自己等候调度队列队尾
	SYS_MREMAP                 = 25 // 重新映射虚拟内存地址
	SYS_MSYNC                  = 26 // 将映射内存中的数据写回磁盘
	SYS_MINCORE                = 27
	SYS_MADVISE                = 28
	SYS_SHMGET                 = 29 // 获取共享内存
	SYS_SHMAT                  = 30 // 连接共享内存
	SYS_SHMCTL                 = 31 // 控制共享内存
	SYS_DUP                    = 32 // 复制已打开的文件描述字
	SYS_DUP2                   = 33 // 按指定条件复制文件描述字
	SYS_PAUSE                  = 34 // 挂起进程，等待信号
	SYS_NANOSLEEP              = 35 // 使进程睡眠指定的时间
	SYS_GETITIMER              = 36 // 获取计时器值
	SYS_ALARM                  = 37 // 设置进程的闹钟
	SYS_SETITIMER              = 38 // 设置计时器值
	SYS_GETPID                 = 39 // 获取进程标识号
	SYS_SENDFILE               = 40 // 在文件或端口间传输数据
	SYS_SOCKET                 = 41 // 建立socket
	SYS_CONNECT                = 42 // 连接远程主机
	SYS_ACCEPT                 = 43 // 响应socket连接请求
	SYS_SENDTO                 = 44 // 发送UDP信息
	SYS_RECVFROM               = 45 // 接收UDP信息
	SYS_SENDMSG                = 46 // 参见send
	SYS_RECVMSG                = 47 // 参见recv
	SYS_SHUTDOWN               = 48 // 关闭socket上的连接
	SYS_BIND                   = 49 // 绑定socket到端口
	SYS_LISTEN                 = 50 // 监听socket端口
	SYS_GETSOCKNAME            = 51 // 取得本地socket名字
	SYS_GETPEERNAME            = 52 // 获取通信对方的socket名字
	SYS_SOCKETPAIR             = 53 // 创建一对已联接的无名socket
	SYS_SETSOCKOPT             = 54 // 设置端口参数
	SYS_GETSOCKOPT             = 55 // 取端口设置
	SYS_CLONE                  = 56 // 按指定条件创建子进程
	SYS_FORK                   = 57 // 创建一个新进程
	SYS_VFORK                  = 58 // 创建一个子进程，以供执行新程序，常与execve等同时使用
	SYS_EXECVE                 = 59 // 运行可执行文件
	SYS_EXIT                   = 60 // 中止进程
	SYS_WAIT4                  = 61 // 参见waitpid
	SYS_KILL                   = 62 // 向进程或进程组发信号
	SYS_UNAME                  = 63 // 获取当前UNIX系统的名称、版本和主机等信息
	SYS_SEMGET                 = 64 // 获取一组信号量
	SYS_SEMOP                  = 65 // 信号量操作
	SYS_SEMCTL                 = 66 // 信号量控制
	SYS_SHMDT                  = 67 // 拆卸共享内存
	SYS_MSGGET                 = 68 // 获取消息队列
	SYS_MSGSND                 = 69 // 发消息
	SYS_MSGRCV                 = 70 // 取消息
	SYS_MSGCTL                 = 71 // 消息控制操作
	SYS_FCNTL                  = 72 // 文件控制
	SYS_FLOCK                  = 73 // 文件加/解锁
	SYS_FSYNC                  = 74 // 把文件在内存中的部分写回磁盘
	SYS_FDATASYNC              = 75
	SYS_TRUNCATE               = 76 // 截断文件
	SYS_FTRUNCATE              = 77 // 参见truncate
	SYS_GETDENTS               = 78 // 读取目录项
	SYS_GETCWD                 = 79
	SYS_CHDIR                  = 80  // 改变当前工作目录
	SYS_FCHDIR                 = 81  // 参见chdir
	SYS_RENAME                 = 82  // 文件改名
	SYS_MKDIR                  = 83  // 创建目录
	SYS_RMDIR                  = 84  // 删除目录
	SYS_CREAT                  = 85  // 创建新文件
	SYS_LINK                   = 86  // 创建链接
	SYS_UNLINK                 = 87  // 删除链接
	SYS_SYMLINK                = 88  // 创建符号链接
	SYS_READLINK               = 89  // 读符号链接的值
	SYS_CHMOD                  = 90  // 改变文件方式
	SYS_FCHMOD                 = 91  // 参见chmod
	SYS_CHOWN                  = 92  // 改变文件的属主或用户组
	SYS_FCHOWN                 = 93  // 参见chown
	SYS_LCHOWN                 = 94  // 参见chown
	SYS_UMASK                  = 95  // 设置文件权限掩码
	SYS_GETTIMEOFDAY           = 96  // 取时间和时区
	SYS_GETRLIMIT              = 97  // 获取系统资源上限
	SYS_GETRUSAGE              = 98  // 获取系统资源使用情况
	SYS_SYSINFO                = 99  // 取得系统信息
	SYS_TIMES                  = 100 // 取进程运行时间
	SYS_PTRACE                 = 101 // 进程跟踪
	SYS_GETUID                 = 102 // 获取用户标识号
	SYS_SYSLOG                 = 103
	SYS_GETGID                 = 104 // 获取组标识号
	SYS_SETUID                 = 105 // 设置用户标志号
	SYS_SETGID                 = 106 // 设置组标志号
	SYS_GETEUID                = 107 // 获取有效用户标识号
	SYS_GETEGID                = 108 // 获取有效组标识号
	SYS_SETPGID                = 109 // 设置指定进程组标志号
	SYS_GETPPID                = 110 // 获取父进程标识号
	SYS_GETPGRP                = 111 // 获取当前进程组标识号
	SYS_SETSID                 = 112 // 设置会晤标识号
	SYS_SETREUID               = 113 // 分别设置真实和有效的用户标识号
	SYS_SETREGID               = 114 // 分别设置真实和有效的的组标识号
	SYS_GETGROUPS              = 115 // 获取后补组标志清单
	SYS_SETGROUPS              = 116 // 设置后补组标志清单
	SYS_SETRESUID              = 117 // 分别设置真实的,有效的和保存过的用户标识号
	SYS_GETRESUID              = 118 // 分别获取真实的,有效的和保存过的用户标识号
	SYS_SETRESGID              = 119 // 分别设置真实的,有效的和保存过的组标识号
	SYS_GETRESGID              = 120 // 分别获取真实的,有效的和保存过的组标识号
	SYS_GETPGID                = 121 // 获取指定进程组标识号
	SYS_SETFSUID               = 122 // 设置文件系统检查时使用的用户标识号
	SYS_SETFSGID               = 123 // 设置文件系统检查时使用的组标识号
	SYS_GETSID                 = 124 // 获取会晤标识号
	SYS_CAPGET                 = 125 // 获取进程权限
	SYS_CAPSET                 = 126 // 设置进程权限
	SYS_RT_SIGPENDING          = 127
	SYS_RT_SIGTIMEDWAIT        = 128
	SYS_RT_SIGQUEUEINFO        = 129
	SYS_RT_SIGSUSPEND          = 130
	SYS_SIGALTSTACK            = 131
	SYS_UTIME                  = 132 // 改变文件的访问修改时间
	SYS_MKNOD                  = 133 // 创建索引节点
	SYS_USELIB                 = 134 // 选择要使用的二进制函数库
	SYS_PERSONALITY            = 135 // 设置进程运行域
	SYS_USTAT                  = 136 // 取文件系统信息
	SYS_STATFS                 = 137 // 取文件系统信息
	SYS_FSTATFS                = 138 // 参见statfs
	SYS_SYSFS                  = 139 // 取核心支持的文件系统类型
	SYS_GETPRIORITY            = 140 // 获取调度优先级
	SYS_SETPRIORITY            = 141 // 设置调度优先级
	SYS_SCHED_SETPARAM         = 142 // 设置进程的调度参数
	SYS_SCHED_GETPARAM         = 143 // 取得进程的调度参数
	SYS_SCHED_SETSCHEDULER     = 144 // 设置指定进程的调度策略和参数
	SYS_SCHED_GETSCHEDULER     = 145 // 取得指定进程的调度策略
	SYS_SCHED_GET_PRIORITY_MAX = 146 // 取得静态优先级的上限
	SYS_SCHED_GET_PRIORITY_MIN = 147 // 取得静态优先级的下限
	SYS_SCHED_RR_GET_INTERVAL  = 148 // 取得按RR算法调度的实时进程的时间片长度
	SYS_MLOCK                  = 149 // 内存页面加锁
	SYS_MUNLOCK                = 150 // 内存页面解锁
	SYS_MLOCKALL               = 151 // 调用进程所有内存页面加锁
	SYS_MUNLOCKALL             = 152 // 调用进程所有内存页面解锁
	SYS_VHANGUP                = 153 // 挂起当前终端
	SYS_MODIFY_LDT             = 154 // 读写进程的本地描述表
	SYS_PIVOT_ROOT             = 155
	SYS__SYSCTL                = 156 // 读/写系统参数
	SYS_PRCTL                  = 157 // 对进程进行特定操作
	SYS_ARCH_PRCTL             = 158
	SYS_ADJTIMEX               = 159 // 调整系统时钟
	SYS_SETRLIMIT              = 160 // 设置系统资源上限
	SYS_CHROOT                 = 161 // 改变根目录
	SYS_SYNC                   = 162 // 将内存缓冲区数据写回硬盘
	SYS_ACCT                   = 163 // 启用或禁止进程记账
	SYS_SETTIMEOFDAY           = 164 // 设置时间和时区
	SYS_MOUNT                  = 165 // 安装文件系统
	SYS_UMOUNT2                = 166
	SYS_SWAPON                 = 167 // 打开交换文件和设备
	SYS_SWAPOFF                = 168 // 关闭交换文件和设备
	SYS_REBOOT                 = 169 // 重新启动
	SYS_SETHOSTNAME            = 170 // 设置主机名称
	SYS_SETDOMAINNAME          = 171 // 设置域名
	SYS_IOPL                   = 172 // 改变进程I/O权限级别
	SYS_IOPERM                 = 173 // 设置端口I/O权限
	SYS_CREATE_MODULE          = 174 // 创建可装载的模块项
	SYS_INIT_MODULE            = 175 // 初始化模块
	SYS_DELETE_MODULE          = 176 // 删除可装载的模块项
	SYS_GET_KERNEL_SYMS        = 177
	SYS_QUERY_MODULE           = 178 // 查询模块信息
	SYS_QUOTACTL               = 179 // 控制磁盘配额
	SYS_NFSSERVCTL             = 180 // 对NFS守护进程进行控制
	SYS_GETPMSG                = 181
	SYS_PUTPMSG                = 182
	SYS_AFS_SYSCALL            = 183
	SYS_TUXCALL                = 184
	SYS_SECURITY               = 185
	SYS_GETTID                 = 186
	SYS_READAHEAD              = 187
	SYS_SETXATTR               = 188
	SYS_LSETXATTR              = 189
	SYS_FSETXATTR              = 190
	SYS_GETXATTR               = 191
	SYS_LGETXATTR              = 192
	SYS_FGETXATTR              = 193
	SYS_LISTXATTR              = 194
	SYS_LLISTXATTR             = 195
	SYS_FLISTXATTR             = 196
	SYS_REMOVEXATTR            = 197
	SYS_LREMOVEXATTR           = 198
	SYS_FREMOVEXATTR           = 199
	SYS_TKILL                  = 200
	SYS_TIME                   = 201 // 取得系统时间
	SYS_FUTEX                  = 202
	SYS_SCHED_SETAFFINITY      = 203
	SYS_SCHED_GETAFFINITY      = 204
	SYS_SET_THREAD_AREA        = 205
	SYS_IO_SETUP               = 206
	SYS_IO_DESTROY             = 207
	SYS_IO_GETEVENTS           = 208
	SYS_IO_SUBMIT              = 209
	SYS_IO_CANCEL              = 210
	SYS_GET_THREAD_AREA        = 211
	SYS_LOOKUP_DCOOKIE         = 212
	SYS_EPOLL_CREATE           = 213
	SYS_EPOLL_CTL_OLD          = 214
	SYS_EPOLL_WAIT_OLD         = 215
	SYS_REMAP_FILE_PAGES       = 216
	SYS_GETDENTS64             = 217
	SYS_SET_TID_ADDRESS        = 218
	SYS_RESTART_SYSCALL        = 219
	SYS_SEMTIMEDOP             = 220
	SYS_FADVISE64              = 221
	SYS_TIMER_CREATE           = 222
	SYS_TIMER_SETTIME          = 223
	SYS_TIMER_GETTIME          = 224
	SYS_TIMER_GETOVERRUN       = 225
	SYS_TIMER_DELETE           = 226
	SYS_CLOCK_SETTIME          = 227
	SYS_CLOCK_GETTIME          = 228
	SYS_CLOCK_GETRES           = 229
	SYS_CLOCK_NANOSLEEP        = 230
	SYS_EXIT_GROUP             = 231
	SYS_EPOLL_WAIT             = 232
	SYS_EPOLL_CTL              = 233
	SYS_TGKILL                 = 234
	SYS_UTIMES                 = 235 // 参见utime
	SYS_VSERVER                = 236
	SYS_MBIND                  = 237
	SYS_SET_MEMPOLICY          = 238
	SYS_GET_MEMPOLICY          = 239
	SYS_MQ_OPEN                = 240
	SYS_MQ_UNLINK              = 241
	SYS_MQ_TIMEDSEND           = 242
	SYS_MQ_TIMEDRECEIVE        = 243
	SYS_MQ_NOTIFY              = 244
	SYS_MQ_GETSETATTR          = 245
	SYS_KEXEC_LOAD             = 246
	SYS_WAITID                 = 247
	SYS_ADD_KEY                = 248
	SYS_REQUEST_KEY            = 249
	SYS_KEYCTL                 = 250
	SYS_IOPRIO_SET             = 251
	SYS_IOPRIO_GET             = 252
	SYS_INOTIFY_INIT           = 253
	SYS_INOTIFY_ADD_WATCH      = 254
	SYS_INOTIFY_RM_WATCH       = 255
	SYS_MIGRATE_PAGES          = 256
	SYS_OPENAT                 = 257
	SYS_MKDIRAT                = 258
	SYS_MKNODAT                = 259
	SYS_FCHOWNAT               = 260
	SYS_FUTIMESAT              = 261
	SYS_NEWFSTATAT             = 262
	SYS_UNLINKAT               = 263
	SYS_RENAMEAT               = 264
	SYS_LINKAT                 = 265
	SYS_SYMLINKAT              = 266
	SYS_READLINKAT             = 267
	SYS_FCHMODAT               = 268
	SYS_FACCESSAT              = 269
	SYS_PSELECT6               = 270
	SYS_PPOLL                  = 271
	SYS_UNSHARE                = 272
	SYS_SET_ROBUST_LIST        = 273
	SYS_GET_ROBUST_LIST        = 274
	SYS_SPLICE                 = 275
	SYS_TEE                    = 276
	SYS_SYNC_FILE_RANGE        = 277
	SYS_VMSPLICE               = 278
	SYS_MOVE_PAGES             = 279
	SYS_UTIMENSAT              = 280
	SYS_EPOLL_PWAIT            = 281
	SYS_SIGNALFD               = 282
	SYS_TIMERFD_CREATE         = 283
	SYS_EVENTFD                = 284
	SYS_FALLOCATE              = 285
	SYS_TIMERFD_SETTIME        = 286
	SYS_TIMERFD_GETTIME        = 287
	SYS_ACCEPT4                = 288
	SYS_SIGNALFD4              = 289
	SYS_EVENTFD2               = 290
	SYS_EPOLL_CREATE1          = 291
	SYS_DUP3                   = 292
	SYS_PIPE2                  = 293
	SYS_INOTIFY_INIT1          = 294
	SYS_PREADV                 = 295
	SYS_PWRITEV                = 296
	SYS_RT_TGSIGQUEUEINFO      = 297
	SYS_PERF_EVENT_OPEN        = 298
	SYS_RECVMMSG               = 299
	SYS_FANOTIFY_INIT          = 300
	SYS_FANOTIFY_MARK          = 301
	SYS_PRLIMIT64              = 302
)
