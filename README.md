# vconn

[![Go Reference](https://pkg.go.dev/badge/github.com/456vv/vconn.svg)](https://pkg.go.dev/github.com/456vv/vconn)

golang vconn：给任意 `net.Conn` 加上**「连接断开提前通知」**能力。

- 对端断开（FIN/RST）可在**不读取、不消耗任何数据**的前提下被感知；
- 即使对端"先发数据再断开"，也能立即收到通知（不再被预读缓冲卡住）；
- 绝不改写调用方设置的 `ReadDeadline`，不会让 `Read` 意外永久阻塞；
- 每条连接至多 1 个常驻监控协程，不做 per-read goroutine，订阅关闭通知**几乎零开销**；
- 前台读串行化，多 goroutine 并发 `Read` 安全，且不会互相注入伪超时。

## 快速开始

```go
conn, err := net.Dial("tcp", "127.0.0.1:8080")
if err != nil {
    return err
}
vc := vconn.New(conn)
defer vc.Close()

// 方式一：订阅关闭通知（可长期使用，退出前请 Close，避免泄漏）
go func() {
    if err := <-vc.CloseNotify(); err != nil {
        log.Printf("连接已断开: %v", err)
    }
}()

// 方式二：需要退订时使用 WatchClose
ch, cancel := vc.WatchClose()
defer cancel()

// 方式三：同一连接反复订阅（每轮事件都要通知）—— 通知落定后重置再订阅
for {
    ch := vc.CloseNotify()
    <-ch
    if err := vc.ResetNotify(false); err != nil { // 本地已关闭/对端已断开时会被拒绝
        break
    }
}
```

## 接口

```go
type CloseNotifier interface {                          // 关闭通知接口
    CloseNotify() <-chan error                          // 关闭通知
}

type Conn struct{}                                      // 连接

// 构造
func New(c net.Conn) *Conn                              // 包装连接
func NewData(c net.Conn, b []byte) *Conn                // 包装连接并预置已到达的数据

// net.Conn 实现
func (T *Conn) Read(b []byte) (n int, err error)         // 读取
func (T *Conn) Write(b []byte) (n int, err error)        // 写入
func (T *Conn) Close() error                             // 关闭（幂等，返回首次错误）
func (T *Conn) LocalAddr() net.Addr                      // 本地地址（关闭后仍有效）
func (T *Conn) RemoteAddr() net.Addr                     // 远程地址（关闭后仍有效）
func (T *Conn) SetDeadline(t time.Time) error            // 读写超时（关闭后返回 net.ErrClosed）
func (T *Conn) SetReadDeadline(t time.Time) error        // 读超时
func (T *Conn) SetWriteDeadline(t time.Time) error       // 写超时
func (T *Conn) SetReadBuffer(bytes int) error            // 系统接收缓冲区
func (T *Conn) SetWriteBuffer(bytes int) error           // 系统发送缓冲区
func (T *Conn) SetKeepAlive(v bool) error                // TCP keepalive 开关
func (T *Conn) SetKeepAlivePeriod(d time.Duration) error // TCP keepalive 周期
func (T *Conn) SetLinger(sec int) error                  // 关闭 linger 行为
func (T *Conn) CloseWrite() error                        // 半关闭写方向
func (T *Conn) CloseRead() error                         // 半关闭读方向

// 关闭通知
func (T *Conn) CloseNotify() <-chan error                // 获取关闭通知（不可退订）
func (T *Conn) WatchClose() (<-chan error, func())       // 获取关闭通知 + 退订函数
func (T *Conn) CancelNotify(err error) bool              // 主动分发一次关闭通知（上锁，不可逆）
func (T *Conn) ResetNotify(force bool) error             // 清除已落定状态，重新武装（可反复订阅）
func (T *Conn) WithContext(ctx context.Context)          // 上下文取消时「通知」
func (T *Conn) WithContextClose(ctx context.Context)     // 上下文取消时「关闭连接」

// 状态
func (T *Conn) IsPeerClosed() bool                       // 是否探测到对端已关闭
func (T *Conn) Err() error                               // 关闭原因
func (T *Conn) Stats() Stats                             // 可观测计数快照
func (T *Conn) CheckAlive() error                        // 主动探活（非阻塞、不消耗数据）
func (T *Conn) CheckAliveBlocking() error             // 主动探活（内核探测失败时退化为阻塞预读）

// 配置
func (T *Conn) SetBackgroundReadBuffer(n int)            // 预读字节数：>0 预读、0 关闭、<0 丢弃
func (T *Conn) SetReadLimit(remain int64)                // 限制读取字节数，超出返回 io.EOF
func (T *Conn) SetMonitorInterval(d time.Duration)       // 空闲探测周期，默认 200ms

// 原始连接提取（各只能调用一次，重复调用 panic）
func (T *Conn) RawConn() net.Conn                        // 归还底层连接
func (T *Conn) RawConnFull() (net.Conn, []byte)          // 归还底层连接 + 未消费的预读字节
```

## 参数速查

| 方法 | 取值 | 语义 |
|---|---|---|
| `SetBackgroundReadBuffer` | `0`（默认） | 关闭后台预读，仅保留内核级关闭探测；`net.Pipe`/自定义连接需 `n > 0` 才能检测对端关闭 |
| | `n > 0` | 预读最多 n 字节（上限 1 MiB），供 `Read` / `RawConnFull` 使用 |
| | `n = 0` | 关闭预读，仅保留内核级关闭探测 |
| | `n < 0` | 丢弃模式：读到的数据直接丢弃，**会丢数据**；单次读取长度为 `max(\|n\|, 4 KiB)`（上限 1 MiB，且该缓冲随连接常驻） |
| `SetReadLimit` | `-1` | 不限制 |
| | `>= 0` | 剩余可读字节数，为 0 时 `Read` 返回 `io.EOF` |
| `SetMonitorInterval` | `<= 0` | 恢复默认 200ms |
| | `10ms ~` | 空闲连接的探测周期（灵敏度 / CPU 折中） |
| `Stats` | — | 返回 `ReadSeq/ProbeCount/PeekBytes/MonitorRounds/Notified/PeerDown/Closed` 快照 |

## CheckAliveBlocking 与安全阻塞探测

`CheckAliveBlocking` 是 `CheckAlive` 的增强版：内核探测（`SO_ERROR` + `poll`/`WSAPoll`）不可用时，**额外**尝试一次带短超时的阻塞预读探测。

它走两条路径：

| 路径 | 触发条件 | 行为 | 数据安全 |
|---|---|---|---|
| 内核探测 | 连接能解包到 `syscall.Conn`（`*net.TCPConn`、`*tls.Conn` 等） | 零字节消耗，直接返回结果 | ✅ 不碰数据 |
| 阻塞预读 | 拿不到 fd（`net.Pipe`、未知 wrapper 等） | 读 **1 字节**（带 2ms 超时），放入 peek 缓冲 | ✅ 前台 `Read` 优先消费 |

**探活不产生副作用**：`CheckAliveBlocking` 只对**本次调用**使用 4 KiB 的预读额度，
不会把 `peekSize` 写回连接，因此「探活一次」不会悄悄打开后台预读。
（旧实现会 `CAS(0 → 4096)`，导致一次探活就永久开启后台读。）

**阻塞预读只对「明确可安全读取」的连接开放**，避免误读 `*tls.Conn` 破坏握手/记录边界。判定规则（`isBlockProbeSafe`）：

| 连接形态 | 判定 | 理由 |
|---|---|---|
| 直接 `syscall.Conn`（`*net.TCPConn` 等） | ✅ 允许 | 用户自己的 fd 实现，读的是原始字节流 |
| 实现了 `SafeBlockProber` | 以 `SafeToBlockProbe()` 为准 | 显式表态优先（可 opt-in 也可 opt-out） |
| 包装型连接（声明了 `NetConn() net.Conn`） | ❌ 跳过 | 可能隐藏 TLS/多路复用等有副作用的 `Read` |
| 其它叶子连接（`net.Pipe`、内存管道） | ✅ 允许 | 无协议状态机，读到的就是应用字节 |

裸 `net.Pipe` 默认允许阻塞探测，这是 `SetBackgroundReadBuffer` 文档承诺的语义：
这类连接没有内核就绪事件，只能靠「带短超时的阻塞预读」发现对端关闭。
如果你有一个**会隐藏 TLS 但没实现 `NetConn()`** 的自定义连接，请显式 opt-out：

```go
type myHiddenTLS struct{ /* ... */ }

func (myHiddenTLS) SafeToBlockProbe() bool { return false } // 禁止阻塞探测
```

### 示例 1：TLS，推荐写法（零风险）

`*tls.Conn` 自带 `NetConn()`，`syscallConnOf` 会自动解包到 `*net.TCPConn`，走**内核探测路径**，不碰 TLS 层：

```go
tlsConn, err := tls.Dial("tcp", "server:443", cfg)
if err != nil {
    return err
}

c := vconn.New(tlsConn)
defer c.Close()

// 走内核探测：WSAPoll / poll(2)，零字节消耗
if err := c.CheckAliveBlocking(); err != nil {
    log.Printf("connection dead: %v", err)
    return
}
```

数据安全要点：

- 探测对象是**底层 TCP socket**，看到的是密文可读与否，**不会触发 TLS 握手**；
- `CheckAliveBlocking` 在 `probeConn` 返回 `probeIdle`/`probeReadable` 时直接返回 `nil`，**不会进入 `peekProbe`**，不读任何字节。

### 示例 2：TLS 外包了 wrapper（安全降级，不报错）

如果 wrapper 忘了实现 `NetConn()`，`CheckAliveBlocking` 会走 `probeUnsupported` 分支；由于连接不是 `syscall.Conn`、也不是 `SafeBlockProber`，**默认跳过阻塞预读**，返回 `nil`（fail-open）：

```go
type myWrapper struct {
    conn *tls.Conn
}
// 实现 net.Conn 全部方法，但**没有** NetConn()

w := &myWrapper{conn: tlsConn}
c := vconn.New(w)

// 安全：不会读 TLS 层，不会触发握手，不会丢数据
// 返回 nil 表示「无法探测，视为存活」
if err := c.CheckAliveBlocking(); err != nil {
    // 只有内核探测（若 wrapper 恰好能解包）才会返回错误
    log.Printf("dead: %v", err)
}
```

**推荐做法**：给 wrapper 加一行 `NetConn()`，让它能解包到 `*tls.Conn` → `*net.TCPConn`，升级为内核探测路径：

```go
func (w *myWrapper) NetConn() net.Conn { return w.conn }
```

### 示例 3：`net.Pipe`（开箱即可阻塞探测）

`net.Pipe` 是纯内存管道，没有 fd，`probeUnsupported` 恒成立。它属于「叶子连接」，
默认允许阻塞探测，无需额外包装：

```go
a, b := net.Pipe()
defer a.Close()
defer b.Close()

c := vconn.New(a)
defer c.Close()

// 直接可用：读 1 字节（带 2ms 超时），数据进 peek 缓冲
if err := c.CheckAliveBlocking(); err != nil {
    log.Printf("peer down: %v", err)
}
```

`WrapSafeBlockProbe` 仍然保留，用于把**包装型连接**（声明了 `NetConn()`、默认被跳过）
显式升级为可阻塞探测：

```go
type wrapper struct{ conn net.Conn }
func (w *wrapper) NetConn() net.Conn { return w.conn }
// ... 其余 net.Conn 方法

c := vconn.New(vconn.WrapSafeBlockProbe(&wrapper{conn: a}))
```

### 示例 4：自定义内存连接（实现 `SafeBlockProber`）

纯内存字节流、不维护协议状态机的连接，可以直接实现接口，无需包装：

```go
type memConn struct {
    // 实现 net.Conn 的全部方法...
}

func (m *memConn) SafeToBlockProbe() bool { return true } // 显式承诺可安全阻塞读

c := vconn.New(&memConn{})
defer c.Close()

if err := c.CheckAliveBlocking(); err != nil {
    log.Printf("peer down: %v", err)
}
```

### ⚠️ 反例：这些连接**不要**声明 `SafeBlockProber`

提前 `Read` 会破坏内部状态机的连接，**绝不能**实现 `SafeToBlockProbe() bool` 返回 `true`，也**不要**对它们使用 `WrapSafeBlockProbe`：

| 连接类型 | 后果 |
|---|---|
| `*tls.Conn` 及其 wrapper | `Read` 触发 `Handshake()`，2ms 超时导致握手失败且**永久不可用**；预读还会把 TLS 记录搬进 peek 缓冲 |
| HTTP/2 多路复用连接 | 提前读会吞掉帧头，整个帧解析失败 |
| yamux / smux 等多路复用 | 提前读会吞掉子流数据 |
| 任何「首次 `Read` 有副作用」的连接 | 副作用被提前触发 |

对这些连接，正确做法是：

- 能解包 fd → **什么也不用做**，自动走内核探测（`*tls.Conn` 就是这种情况）；
- 拿不到 fd → 接受 fail-open（返回 `nil`），用其它手段（业务心跳、自己读到的 EOF）保证连接健康。

### 行为速查

| 连接 | `direct` | `SafeBlockProber` | `CheckAliveBlocking` 行为 |
|---|---|---|---|
| `*net.TCPConn` | ✅ | — | 内核探测 |
| `*tls.Conn`（直接包装） | —（自动解包到 TCP） | — | 内核探测 |
| TLS wrapper（无 `NetConn`） | ❌ | ❌ | 阻塞探测（视为叶子连接）⚠️ 见下 |
| TLS wrapper（有 `NetConn`） | —（自动解包到 TCP） | — | 内核探测 |
| `net.Pipe`（裸） | ❌ | ❌ | 阻塞探测（叶子连接，默认允许） |
| `net.Pipe` + `WrapSafeBlockProbe` | ❌ | ✅ | 阻塞探测 |
| 自定义内存连接 + `SafeToBlockProbe` | ❌ | ✅ | 阻塞探测 |
| 任何连接 + `SafeToBlockProbe() false` | — | opt-out | 跳过，返回 `nil` |
| 自定义 `syscall.Conn` | ✅ | — | 阻塞探测（`SyscallConn` 失败时） |

⚠️ **TLS wrapper（无 `NetConn`）这一行是唯一的风险点**：因为它既拿不到 fd、
又没声明自己是包装，只能按「叶子连接」处理。如果你用这类 wrapper 包了 `*tls.Conn`，
请务必给它加 `NetConn()`（升级为内核探测），或实现 `SafeToBlockProbe() bool { return false }`（显式 opt-out）。

### 超时参数

| 常量 | 默认值 | 用于 |
|---|---|---|
| `peekProbeTimeout` | 2ms | `CheckAliveBlocking` 同步阻塞探测 |
| `peekReadTimeout` | 10ms | 监控协程后台预读 |

受系统 timer 精度限制，低于 1ms 无意义；同步路径建议 1~5ms。

## 反复订阅 / 重置（多轮通知）

默认语义是「一次性事件」：通知落定后 `notified` 被锁存，之后所有订阅都会立刻得到同一个结果。
需要在**同一条连接上多轮订阅**（心跳/请求级监控、把断开当作可重复消费的事件）时，用 `ResetNotify` 清除锁存：

```go
ch := vc.CloseNotify()          // 第 1 轮
<-ch
err := vc.ResetNotify(false)    // 清除 notified/closeErr/peerDown，重新武装
if err != nil { ... }           // 本地已关闭 → ErrResetLocallyClosed；对端已断开 → ErrResetPeerDown
ch = vc.CloseNotify()           // 第 2 轮：新一轮监控协程，旧 channel 不会被二次投递
```

`force` 参数只有一种用途：对端已断开（`ErrResetPeerDown`）时是否仍然重置。

| 调用 | 结果 |
|---|---|
| `ResetNotify(false)` 未落定时 | 幂等 `nil`，不打断正在工作的监控 |
| `ResetNotify(false)` 手动通知（`CancelNotify`）后 | `nil`，监控随下一次订阅重启 |
| `ResetNotify(false)` 对端已断开后 | `ErrResetPeerDown`（需要 `ResetNotify(true)`） |
| `ResetNotify(true)` 对端已断开后 | `nil`，清空 `peerDown`，监控立刻重新探测到断开并再次通知 |
| `ResetNotify(任意值)` 本地已 `Close`/`RawConn` | `ErrResetLocallyClosed`（`true` 也越不过） |
| `ResetNotify(任意值)` 上一轮监控卡在阻塞预读 | 状态已清空，返回 `ErrResetTimeout`，可直接重试 |

要点：

- 旧 channel 一旦投递就永久关闭，**重置不会二次投递**，必须重新订阅；
- 重置不清空预读缓冲区，也不影响 `Read`/`Write`/`SetReadLimit` 等数据面状态；
- 监控协程按「轮」管理（每轮独立 `quit/done`），重置后可重新启动，整条连接始终至多 1 个存活实例；
- 观测能力：不再提供查询「第几轮」或「当前订阅者数量」的接口，如需要请在调用方自行计数；
- **`CancelNotify` 是上锁（0→1），`ResetNotify` 是解锁（1→0）**，方向相反、不可互换：
  `CancelNotify(nil)` 会以 `ErrNotifyCanceled` 落定并唤醒所有等待者，不会清空状态。

## 检测原理与开销

| 平台 | 探测手段 | 是否有未读数据也能识别 FIN |
|---|---|---|
| Linux / Android | `poll(POLLIN/POLLRDHUP)` + `recv(MSG_PEEK)` | ✅ |
| macOS / iOS / *BSD | `poll(POLLIN)` + `recv(MSG_PEEK)` | ✅（BSD 收到 FIN 只置 `POLLIN`，故必须用 `MSG_PEEK` 复核） |
| Windows | `WSAPoll(POLLIN)` + `FIONREAD(WSAIoctl)` + 二次 poll 复核 | ✅（不用同步 `WSARecv`，避免并发读导致探针阻塞） |
| Solaris / illumos / AIX | `recv(MSG_PEEK)` | 缓冲区排空后可识别 |
| js / wasip1 / plan9 | 无内核探测，退化为「带自身超时的预读」 | 受预读缓冲限制 |

探测结果有四种，其中 `probeReadableHangup`（有未读数据 + 对端已 FIN）是关键区分：
它表示「可以立刻通知，但**不能**当成接收队列已空」，否则调用方可能提前关闭而丢掉未读数据。
本包对 `probeClosed` 与 `probeReadableHangup` 的通知语义完全一致：只分发通知，绝不关闭底层。

只在连接**空闲**（上一周期没有前台读取）时才探测，因此繁忙连接不产生任何额外系统调用；
空闲连接每 `SetMonitorInterval` 周期一次系统调用（默认 200ms 一次）。

## 与旧版本的行为差异（迁移须知）

旧版本存在若干缺陷（详见 [docs/audit-report-2026-11-15.md](docs/audit-report-2026-11-15.md)），修复后行为变化如下：

| 场景 | 旧行为 | 新行为 |
|---|---|---|
| 对端先发数据再断开 | **收不到通知** | 立即收到通知，数据仍可正常读出 |
| `Read` 同时拿到数据与 EOF | 返回 `(n>0, io.EOF)` | 返回 `(n, nil)`，EOF 留到下一次 |
| 关闭后 `LocalAddr/RemoteAddr` | 返回 `nil`（易 nil panic） | 返回地址快照 |
| 关闭后 `SetDeadline` 系列 | 返回 `nil` | 返回 `net.ErrClosed` |
| 重复 `Close` | 一律返回 `nil` | 返回首次关闭的错误 |
| `CancelNotify(nil)` | 不落定，`CloseNotify` 永久挂住 | 以 `ErrNotifyCanceled` 落定 |
| `CancelNotify` 传入不可比较 error | panic | 正常返回 |
| `CancelNotify(err)` 之后的 `IsPeerClosed()` | `true`（把手动通知当成对端断开） | `false`（只有真正探测到对端断开/网络异常才置位） |
| 通知落定后再次订阅 | 永远得到上一轮结果（不可重置） | 仍立即得到上一轮结果；`ResetNotify(false)` 后可进入下一轮 |
| `NewData(已包装连接, b)` | 静默丢弃 b | 追加到预读缓冲区 |
| 对端断开后底层 socket | 保持打开（可能泄漏 fd） | 保持打开（半关闭语义），由调用方 `Close()`；需要「通知即关闭」可 `go func(){ <-c.CloseNotify(); c.Close() }()` |
| `WithContext` | 每次泄漏 1 协程；取消不解除阻塞 | 不再泄漏；取消只通知（要断开请用 `WithContextClose`） |
| 预读/探测对 `ReadDeadline` 的影响 | 会抹掉调用方设置的超时 | 完全不修改，仅短暂使用自身超时并恢复 |
| `SetReadBuffer/SetWriteBuffer` | 非 TCP 静默返回 `nil` | 不支持时返回 `ErrUnsupported` |

分层加固之后的额外修正，详见 [docs/impl-report-2026-11-16-plan-b.md](docs/impl-report-2026-11-16-plan-b.md)：

| 场景 | 旧行为 | 新行为 |
|---|---|---|
| `SetReadLimit` + 预读同时生效 | 只扣 socket 字节，实际可读到 `N + len(peek)` | 按本次返回总字节扣减，严格不超过 `N` |
| macOS / *BSD 上对端 FIN（接收队列为空） | 只置 `POLLIN`，被当成「有数据」，**通知永不触发** | `MSG_PEEK` 复核为 0 字节 → 判为断开 |
| 对端「发数据后 FIN」 | 把 `POLLRDHUP` 直接当成已断开 | 用 `MSG_PEEK` 复核：仍有数据 → `probeReadableHangup`，通知但不丢数据 |
| 并发 `Close()` | 后到者可能拿到 `nil`（首个返回错误） | 等待首个 Close 完成，结果完全一致 |
| `ResetNotify` 因 `ErrResetTimeout` 重试 | 重试命中幂等分支，**监控不再启动** | 幂等分支会补回缺失的监控 |
| `RawConnFull` 与在途预读并发 | 已读出的字节写入被放弃的缓冲，**丢失** | 先停监控、唤醒前台 Read，再独占 `fg` 取走预读数据并完成所有权转移，零丢失 |
| `EINVAL` / 未知 errno | 判定为「对端已断开」，健康连接被误标记 | 不再判定；补齐 `ENETDOWN/ENETUNREACH/EHOSTUNREACH` |
| 解包不可比较的自定义 `net.Conn` | `map[net.Conn]` 作环检测 → panic | 去掉 map，仅保留跳数上限 |
| 监控协程自行退出 | `c.mon` 仍指向死实例，无法再开新一轮 | defer 里 `CompareAndSwap(m, nil)` |
| `stopMonitor` 未「催促」预读时 | 仍无条件重写 `ReadDeadline`（多余 syscall） | 仅在实际改过 deadline 时恢复 |
| `CheckAliveBlocking` | 一次探活永久打开 4 KiB 后台预读 | 只对本次探测生效，`peekSize` 不变 |
| `socketError` 读不到选项 | 当成「已断开」，`CheckAlive` 误报 | 返回 `ErrUnsupported`，交由 `probeConn` 兜底 |
| `Read` 返回 `(n>0, 非网络错误)` | 错误被静默丢弃 | 除超时外一律留存到下一次 |
| illumos | 无 `socketError`/`probeConn` 实现，**编译失败** | 已补齐构建标签 |
| `checkconn_*.go` | Windows 版用 `SO_KEEPALIVE` 冒充 `SO_ERROR`，且是死代码 | 已删除（无导出符号） |

### （2026-11-17）加固

| 场景 | 旧行为 | 新行为 |
|---|---|---|
| `RawConn` 与在途探测并发 | 探测协程可能在 `RawConn` 返回后仍操作同一 fd（TOCTOU） | `rawUsed` 的置位与所有关闭/探测路径的检查在同一把 `mu` 内；RawConn 取得所有权后不再碰 `rwc` |
| 并发 `Close()` | 可能重复 `rwc.Close()`，后到者返回 `use of closed network connection` | 共用 `closeDone/closeRet`，底层只关闭一次，所有并发 `Close` 返回同一结果 |
| `RawConnFull` 与阻塞中的前台 `Read` | 等 `fg` 锁可能永久卡住 | 先用过去时间的 `ReadDeadline` 唤醒前台 Read，取得 `fg` 后恢复调用方最新 deadline，再完成所有权转移 |
| Windows 探测的并发读竞态 | `WSAPoll` 后同步 `WSARecv(MSG_PEEK)` 可能阻塞，或把 EAGAIN 当 FIN | 改用 `FIONREAD + 二次 poll`，既不阻塞也不误判；Unix 侧把 `EAGAIN` 明确归为存活 |
| Solaris / illumos / AIX 非 socket fd | `ENOTSOCK/EOPNOTSUPP/EINVAL` 被判为 `probeClosed` | 返回 `probeUnsupported`（无法探测 ≠ 已断开） |
| `SetMonitorInterval` 修改周期 | 需等上一轮 timer 到期才生效 | 通过 wake 通道立即重置 timer |
| `WrapSafeBlockProbe(*net.TCPConn)` | 包装后丢失 `SyscallConn`，退化为阻塞预读 | 转发 `SyscallConn`，仍走内核探测 |
| 可观测性 | 无 | 新增 `Stats()`（读计数 / 探测次数 / 预读字节 / 监控轮次 / 状态） |
| 透传 API | 无 | 新增 `SetKeepAlive/SetKeepAlivePeriod/SetLinger/CloseWrite/CloseRead` |

## 性能

`4 KiB` 读取，内存连接（i7-4700MQ）：

| 场景 | ns/op | 相对开销 |
|---|---|---|
| 裸 `net.Conn` | ~220 | 1.00x |
| `vconn` 包装 | ~300 | ~1.35x |
| `vconn` 包装 + 订阅关闭通知 | ~300 | ~1.35x（通知不额外付费） |

旧版本订阅通知后约为包装开销的 7.5 倍（每读一次创建 1 个协程）。

## 平台支持

linux / android / windows / darwin / ios / freebsd / netbsd / openbsd / dragonfly / solaris / illumos / aix / js / wasip1 / plan9
（`go build` 全矩阵通过，其中 js/wasip1/plan9 自动降级为无内核探测模式）

> 2026-11-16 起 `js` / `wasip1` / `plan9` 的交叉编译已恢复（此前被遗留文件 `checkconn_unix.go` 的 `//go:build !windows` 挡住）。

## 测试

```bash
go test ./...            # 单元 + 回归
go test -race ./...      # 竞态检测
go test -bench . ./...   # 性能
```

回归用例分布：

| 文件 | 覆盖内容 |
|---|---|
| `conn_test.go` | 基础读写、预读、限流、探活、原始连接提取 |
| `conn_fix_test.go` | 历史缺陷回归（通知、解阻塞、deadline、并发读、协程增长） |
| `conn_reset_test.go` | 反复订阅 / `ResetNotify` 多轮语义 |
| `conn_harden_test.go` | 加固回归：限流含预读、对端 FIN 只通知不关底层、并发 Close 一致、监控补回、`RawConnFull` 零丢失、监控自清、探活无副作用、阻塞探测判定 |
| `conn_peek_test.go` | 预读/丢弃缓冲长度契约 |
| `conn_plan2_test.go` | 关闭状态机（`RawConn` 后不再关底层、并发 Close 只关一次）、`RawConnFull` 解阻塞、探测结果分类、选项透传 |
| `probe_helper_test.go` | 包装链解包、不可比较连接不 panic |
| `probe_poll_linux_test.go` | Linux `POLLRDHUP` 与 `POLLIN` 的区分（`probeReadableHangup`） |
| `probe_unix_test.go` | Solaris/illumos/AIX 非 socket fd 返回 `probeUnsupported` |
| `probe_windows_test.go` | Windows `WSAPoll + FIONREAD` 空闲/可读/FIN 探测 |
| `errno_*_test.go` | 错误码表（EINVAL 不误判、真断开仍识别） |

跨平台校验：

```bash
# 全矩阵 go build / go vet（含 illumos）
for t in linux/amd64 darwin/arm64 freebsd/amd64 openbsd/amd64 illumos/amd64 aix/ppc64 windows/amd64 js/wasm plan9/386; do
  GOOS=${t%/*} GOARCH=${t#*/} go vet ./...
done
```

> `ios/*` 的 `go vet` 需要 cgo 外部链接（测试二进制限制），但 `go build` 通过。
