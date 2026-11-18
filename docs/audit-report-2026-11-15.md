# vconn 全量审计报告（Bug / 功能盲区 / 并发安全 / 性能 / 跨平台）

- 审计日期：2026-11-15
- 代码基线：`github.com/456vv/vconn`（conn.go 7.8K、conn_reader.go 6.9K、error_common.go 2.4K、checkconn_unix.go、checkconn_windows.go、conn_test.go）
- 工具链：`go1.27.3 windows/amd64`，`go vet`、`go test`、`go test -race`、12 个 GOOS 交叉编译、自建 15 项探针 + 4 组基准
- 结论概要：**6 项 P0（核心功能失效 / 挂死 / 数据丢失）、9 项 P1（并发与契约）、9 项 P2（跨平台 / 健壮性 / 文档 / 测试）**

> **实施状态（2026-11-15 更新）**：已按选定方案 ③（A + B + C1/C2/C8，严格 API 兼容）完成修复，
> 变更清单与验证结果见 **§11 修复实施记录**。
> 下文 §2–§10 保留**审计时的原始状态**（含原始行号），用于缺陷追溯与回归对照。

---

## 0. 一句话总结

> 库的**核心卖点（提前探测断线）在"对端先发数据后断开"这一最常见场景下完全失效**；同时后台读机制会**篡改用户的 ReadDeadline**（导致读永久阻塞）、**Close() 可永久阻塞并泄漏 fd**、**健康连接上的并发读会被注入伪超时**；开启关闭通知后**单次 Read 慢 7.5 倍**；`js/wasm`、`wasip1`、`plan9` **三个平台无法编译**。

---

## 1. 严重性定义

| 等级 | 含义 | 处置 |
|---|---|---|
| **P0** | 功能失效 / 永久阻塞 / 数据丢失 / 进程 panic，生产必踩 | 立即修，出 patch 版本 |
| **P1** | 并发不安全、违反 `net.Conn` 契约、可复现异常行为 | 本迭代修 |
| **P2** | 跨平台、健壮性、可观测性、文档、工程化 | 排期修 |

---

## 2. P0 缺陷（6 项，均已复现）

### B01　对端"先发数据再断开"，`CloseNotify` 永不触发 ★最严重

- **位置**：`conn_reader.go:73`、`conn_reader.go:112-134`、`conn.go:176-182`
- **根因链**：
  1. `startBackgroundRead()` 在 `r.peeked.Load()==true` 或 `inRead.Load()==true` 时直接 return（`conn_reader.go:73`）；
  2. 后台预读循环在 `lpb >= bl` 时 `break`（缓冲区满，默认 `bl=1`），函数正常返回、协程销毁（`conn_reader.go:120-122`）；
  3. 此后**没有任何监控在跑**，除非用户再成功 `Read` 一次（`conn.go:181` 才重新挂起）。
- **复现（probe 6）**：
  ```
  对端写入 "h" 后关闭
  → 600ms 内 CloseNotify 无任何事件
  → 前台 Read 排空 1 字节后，通知才补发 EOF
  ```
- **影响**：HTTP/1.1 keep-alive、Redis、MQTT、SSE、任意"请求-响应后对端 FIN"的协议，全部检测不到断线。库等于只在"对端一句话不说就挂断"时有效（现有单测 `TestConn_Basic`、`TestConn_CloseNotifyRemoteClose` 恰好都是这种场景，因此测试全绿但线上失效）。
- **修复方案**（三选一，可叠加）：
  - **B01-A（最小改动，~0.5 天）**：peek 缓冲满后不退出，转入"旁路观察"——继续阻塞读但把多余字节丢弃（或写入环形缓冲上限 N KB），保持 FD 上的读挂起以捕获 FIN。风险：会吞掉数据（默认 1 字节 → 建议默认值改为 0=纯观察模式或 4KB）。
  - **B01-B（中等，~1 天）**：引入"监控已停"状态，`CloseNotify()` 时若 `peeked` 为真则改为**定时探测**（每 200ms 用 `SyscallConn` + `SO_ERROR`/`Poll` 检查可读/错误），不再依赖一次性预读。
  - **B01-C（推荐，~2 天）**：用 `poll/epoll/kqueue` 可读性探测替代"读字节"（见 §7 架构草案），零字节消耗、天然检测 FIN/RST、无 goroutine churn。

### B02　`Close()` 可永久阻塞，且底层 fd 不被释放（违反 `net.Conn` 契约）

- **位置**：`conn.go:203-212`（先 `notifyClose` 再 `rwc.Close()`）、`conn_reader.go:157-184`
- **根因**：`Close()` → `notifyClose()` → `abortBackgroundRead()`，后者只能靠 `rwc.SetReadDeadline(time.Unix(0,1))` 把阻塞读踢出来，再 `cond.Wait()` **无限等待**。若底层 `SetReadDeadline` 返回错误（自定义 `net.Conn`、部分隧道/串口/加密封装很常见），就永远等不到；而真正能解阻塞的 `rwc.Close()` 被排在最后，永远执行不到。
- **复现（probe 2）**：把 `net.Pipe` 包一层"deadline 不支持"的 `net.Conn` → `Close()` 800ms 后仍未返回，fd 与协程全部泄漏。
- **影响**：`defer conn.Close()`、`context` 取消、超时清理路径全部可能挂死；fuzz/压测下表现为 goroutine 数单调增长。TCP 上虽能被 deadline 踢出，但 `Close()` 仍被推迟一个完整的中断往返。
- **修复**：
  - **B02-A（推荐）**：`Close()` 改为 **先 `rwc.Close()`（立刻解阻塞）→ 再 `markClosed/notify`**，并把 `abortBackgroundRead` 的 `cond.Wait` 改为 `select { case <-done: case <-time.After(kickTimeout): }` 兜底。
  - **B02-B**：`abortBackgroundRead` 不再阻塞等待，只发"kick"信号，由监控协程自行退出。

### B03　后台读清除用户 `ReadDeadline` 且不恢复 → `Read` 永久阻塞

- **位置**：`conn_reader.go:97`（`SetReadDeadline(time.Time{})`），恢复只发生在 `abortBackgroundRead` 的 `178-180` 行
- **根因**：后台读为了"阻塞等数据"会清掉用户设置超时；但只要它因"缓冲满"正常退出（`conn_reader.go:120`），就走不到 abort 恢复路径，**用户超时被永久抹掉**。
- **复现（probe 1）**：
  ```
  SetReadDeadline(now+150ms) → SetBackgroundReadBuffer(2) → CloseNotify()
  → 1.2s 后 Read 仍未返回
  ```
- **影响**：超时机制形同虚设 → 连接池/爬虫/长连接出现"永久挂住"的读，只能靠外部 kill。
- **修复**：
  - **B03-A（推荐）**：后台读**永不修改**用户 deadline，改用"监控协程自己的 deadline/或 poll 超时"，退出时按快照恢复。
  - **B03-B**：所有退出路径（正常/异常）统一 `defer` 恢复 `readDeadline` 快照。

### B04　关闭状态没有一次性标志：`CancelNotify(nil)` 破坏语义；`CancelNotify` 对不可比较 error 直接 panic

- **位置**：`conn.go:136-139`
  ```go
  func (c *Conn) CancelNotify(err error) bool {
      c.notifyClose(err)          // closeErr==nil 时不会落定
      return c.closeErr == err    // ① 无锁读 ② 接口比较
  }
  ```
- **问题 1**：用 `closeErr != nil` 兼作"已关闭"标志。传 `nil` 时永不落定 → 之后 `CloseNotify()` 永久挂住（probe 3: `CancelNotify(nil)` 连续两次都返回 `true`，随后 `CloseNotify` 阻塞）。
- **问题 2**：`c.closeErr == err` 是**未加锁读**（Go 内存模型层面的数据竞争；`-race` 因写只发生一次而未被点亮）。
- **问题 3**：接口比较在**同类型且不可比较**时 panic。Probe 13：`panic: runtime error: comparing uncomparable type vconn.uncomparableErr`。
- **修复**：拆成 `closed bool` + `closeErr error`（同锁保护）；`CancelNotify` 返回"是否本次生效"；比较用 `errors.Is` 或直接删除比较语义。

### B05　远端断开后不关闭 socket，`IsClosed()` 仍为 `false`（fd 泄漏 + 状态错乱）

- **位置**：`conn.go:216-242`（`notifyClose` 只发通知，不 `close` 也不置 `closed`）
- **复现（probe 6b）**：收到 EOF 通知后 `IsClosed()==false`，fd 仍打开，直到用户显式 `Close()`。
- **影响**：忘记 `Close` 的代码（常见于 `defer` 在另一层）→ fd 耗尽；用 `IsClosed()` 判断"该退出循环了"的代码永远退不出。
- **修复**：
  - **B05-A（推荐）**：`notifyClose` 内 `closed.Store(true)` + 视配置关闭底层；新增 `SetCloseOnRemoteDrop(bool)`（默认 true）。
  - **B05-B**：保持现状但把 `IsClosed()` 语义改为"已失效"（含远端断开），并明确文档。

### B06　`(n>0, err!=nil)` 同时返回，naive 循环丢最后一段数据

- **位置**：`conn_reader.go:236-262`
- **复现（probe 15）**：`Read -> n=3 err=EOF data="abc"` —— 用户写 `if err != nil { break }` 就丢了 `"abc"`。
- **修复**：`n>0` 时返回 `nil`，把 `EOF` 推迟到下一次调用（`io.Reader` 惯例）。

---

## 3. P1 缺陷（并发安全与契约，9 项）

### B07　并发 `Read` 互相注入伪超时（同一个 `inRead` 标志 + "过去时间" deadline 打断）

- **位置**：`conn_reader.go:205-207`（`for r.inRead.Load() { abortBackgroundRead() }`）+ `conn_reader.go:242-247`（前台读也置 `inRead`）
- **复现（probe 12）**：**健康且空闲**的连接上，第 2 个 `Read` 让第 1 个 `Read` 立刻返回
  `read tcp 127.0.0.1:64961->127.0.0.1:64960: i/o timeout`
- **影响**：文档声明"所有公开方法均为并发安全"，实际并发读会出现**假断线/假超时**，用户据此重连 → 抖动。
- **修复**：前台读用独立 `fgMu sync.Mutex` 串行化；`inRead` 拆成 `bgRunning` 与 `fgReading`；**不允许用 deadline 打断别人的读**。

### B08　`inRead` 前后台共用且 get/set 非配对 → 双前台读可同时读 socket

- **位置**：`conn_reader.go:242`（置 true）/`250-252`（置 false），两者之间没有所有权校验
- **影响**：两个 goroutine 同时 `rwc.Read` → 字节被随机瓜分（协议粘包/解密流损坏）；且 A 清标志时 B 仍在读，abort 逻辑全部错判。
- **修复**：同 B07，改为独占锁 + 明确所有权。

### B09　每次成功 `Read` 都 spawn 一个后台协程 + 2 次 `SetReadDeadline`（性能塌陷）

- **位置**：`conn.go:176-182`、`conn_reader.go:63-79`、`conn_reader.go:97`
- **实测（in-memory，4KB/次，无系统调用噪声）**：

  | 场景 | ns/op | 吞吐 | 相对开销 |
  |---|---|---|---|
  | 裸 conn | 737 | 5552 MB/s | 1.00x |
  | vconn 包装（不开通知） | 901 | 4545 MB/s | **1.22x** |
  | vconn 包装 + `CloseNotify` | 5506 | 744 MB/s | **7.47x** |

- **影响**：每读一次 = 1 次 goroutine 创建 + 1 次抢占式中断 + 2 次系统调用；100k QPS 代理 ≈ 每秒 10 万个 goroutine 生死，GC 压力巨大。
- **修复**：常驻单监控协程 + park/unpark（`chan struct{}` 唤醒），或 B01-C 的 poll 方案；实测目标 ≤1.2x。

### B10　`CloseNotify` 注册的 channel 无法注销 → 内存泄漏；`WithContext` 每次泄漏 1 goroutine，且取消不解除阻塞

- **位置**：`conn.go:112-132`、`conn.go:142-154`
- **复现（probe 5）**：调用 500 次 `CloseNotify` → `closeSignals` 长度 500（永不释放）；50 次 `WithContext` → goroutine 2→53。
- **附带问题**：`WithContext` 在 `ctx.Done()` 时**只发通知、不关闭连接**，阻塞在 `Read` 的协程根本醒不过来——用户最期待的"取消即解阻塞"没实现。
- **修复**：`CloseNotify` 返回 `(<-chan error, func())` 或在 `Conn` 上加 `Unwatch`；`WithContext` 改为 `ctx` 取消时执行 `Close()`（可配置）；对 `ctx==nil` 直接 return。

### B11　`RawConnFull` 存在 peek 丢失竞态窗口

- **位置**：`conn.go:91-96`
  ```go
  if c.r.hasPeekedByte() { return c.RawConn(), c.r.peekedByte() }
  return c.RawConn(), nil     // ← hasPeekedByte 为 false 与 RawConn 之间，后台可能已读到字节
  ```
- **影响**：返回 `nil` 却把字节永远留在 `peekByte` 里 → 提取后的原始连接**丢前缀数据**（解密流/协议头损坏）。
- **修复**：在锁内一次性快照（先 abort，再取 `peekByte` 并清空）。

### B12　`RawConn` 不终止正在阻塞的前台读 → 两个 owner 抢同一 socket

- **位置**：`conn.go:69-87`
- **影响**：提取后用户新 goroutine 用 raw conn 读，而旧的阻塞 `Read` 仍在 fd 上 → 数据被"偷"。
- **修复**：`RawConn` 内部等同 `abortForegroundRead()`（带超时兜底），并在文档注明"提取后旧 goroutine 的 Read 返回 `ErrRawConnAlreadyUsed`"。

### B13　关闭后 `net.Conn` 契约破坏（4 处）

| 行为 | 现状 | 标准库语义 | 风险 |
|---|---|---|---|
| `LocalAddr()/RemoteAddr()` | 返回 `nil`（`conn.go:247-260`） | 保持有效地址 | 用户 `defer log(conn.RemoteAddr().String())` **nil panic** |
| `SetDeadline/SetReadDeadline/SetWriteDeadline` | 返回 `nil` | `net.ErrClosed` | 用户以为设置成功，进入死等 |
| `SetReadBuffer/SetWriteBuffer` | 非 `*net.TCPConn` 时静默 `nil` | 应报 unsupported | 调优静默失效 |
| 二次 `Close()` | 返回 `nil` | 应返回首次错误（或 `net.ErrClosed`） | 首次关闭错误被吞 |

- **修复**：地址在 `New` 时快照；deadline 系列关闭后返回 `net.ErrClosed`；`Close` 记忆首次错误。

### B14　关闭后 `Read` 的返回错误不确定

- **位置**：`conn.go:164` 返回 `net.ErrClosed`；`conn_reader.go:236-239` 返回 `io.EOF`
- **影响**：调用方难以区分"对端正常结束"与"本地已关闭"，重试策略会误判。
- **修复**：统一 `net.ErrClosed`（跨 goroutine 竞争时也保持同一语义）。

### B15　`SetReadLimit` 的 load-then-add 非原子

- **位置**：`conn_reader.go:194`（load）→ `226/259`（add）
- **影响**：并发读时可超发配额（limit 语义是"最多读 N 字节"，实际可能多读）。
- **修复**：用 `atomic.CompareAndSwap` 循环，或把配额扣减挪进 `r.mu` 临界区。

---

## 4. P2 缺陷（跨平台 / 健壮性 / 文档 / 测试，9 项）

### B16　3 个平台无法编译（`!windows` 构建标签过宽）

- **实测矩阵**（`go build ./...`）：

  | 平台 | 结果 |
  |---|---|
  | linux/386、linux/arm64、linux/loong64 | ✅ |
  | windows/386、amd64、arm64 | ✅ |
  | darwin/arm64、ios/arm64 | ✅ |
  | freebsd、netbsd、openbsd、dragonfly、solaris、illumos、aix | ✅ |
  | **js/wasm** | ❌ `undefined: syscall.SOL_SOCKET`（checkconn_unix.go:19） |
  | **wasip1/wasm** | ❌ 同上 |
  | **plan9/amd64** | ❌ `syscall.GetsockoptInt/SOL_SOCKET/SO_ERROR` + `error_common.go:52-57` 的 `ECONNRESET/ECONNABORTED/EPIPE/ENETRESET` |

- **修复**：`checkconn_*.go` 用精确标签（`//go:build linux || darwin || ...`）+ `golang.org/x/sys/unix`；`js/wasip1/plan9` 提供 `ErrNotSupported` 桩；`error_common.go` 的 errno 常量拆到 `errno_unix.go` / `errno_windows.go` / `errno_stub.go`。

### B17　Windows `checkConnection` 用错 socket 选项（且整个函数是死代码）

- **位置**：`checkconn_windows.go:20` —— 读的是 `SO_KEEPALIVE`（keepalive 是否开启，返回值 0/1），**不是** `SO_ERROR`（待处理错误）。因此该函数永远返回 `nil`。
- **附带**：`checkConnection` 在包内**没有任何调用点**（grep 仅两处定义），即"连接健康检查"这个能力根本没暴露出来。
- **修复**：Windows 改用 `windows.GetsockoptInt(h, windows.SOL_SOCKET, windows.SO_ERROR)`；导出 `CheckAlive() error` / `IsAlive() bool`（TCP 可选叠加 `TCP_INFO` 的 RTT/重传）。

### B18　`isCommonNetError` 误判与漏判

- **位置**：`error_common.go:18-77`
- **误判**：`syscall.EINVAL` 列为断线（EINVAL 可来自非法 iovec/deadline，会误报断线）。
- **漏判**：`ECONNREFUSED`、`ENETUNREACH`、`EHOSTUNREACH`、`ENETDOWN`、`EPROTO`、`ENOTCONN`、`EBADF`（Unix）；`WSAENOTCONN(10057)`、`WSAEHOSTUNREACH(10065)`、`WSAENETUNREACH(10051)`（Windows）；`context.Canceled/DeadlineExceeded`。
- **不一致**：`net.OpError.Timeout()` 优先返回 false，但裸 `ETIMEDOUT`/`WSAETIMEDOUT` 又判为断线（同一现象两种结论）。
- **修复**：改为"错误码白名单 + 明确排除超时"的单一入口；超时统一走 `net.Error.Timeout()`；补充 `errors.Is(err, os.ErrDeadlineExceeded) → false`。

### B19　`SetBackgroundReadBuffer` 无上限校验，负数即"静默吞数据"

- **位置**：`conn_reader.go:102-111` `make([]byte, max(-bl, 4096))`；`conn.go:98-100` 无校验
- **风险**：`SetBackgroundReadBuffer(-1<<30)` → 立刻 1GB 分配（OOM/DoS）；负值语义=**静默丢弃所有数据**，误用即数据丢失且无任何日志。
- **修复**：限幅（如 `[-64KB, 64KB]`）；丢弃模式拆成显式 `SetDiscardMode(true)`；越界返回 error。

### B20　`RawConnFull` 返回的切片与内部缓冲共享底层数组

- `peekedByte()` 直接返回 `r.peekByte`（`conn_reader.go:52-59`），调用方修改会污染内部状态（此处已置 nil，风险低但无文档）。
- **修复**：文档注明"调用方拥有该切片"，或在必要时 copy。

### B21　`NewData` 对已包装连接静默丢弃种子数据

- **位置**：`conn.go:43-50`：`if vc, ok := c.(*Conn); ok { return vc }` —— 此时 `b` 被忽略。
- **复现（probe 7）**：`NewData(New(c), []byte("SEED"))` 返回同一对象，`peeked=false`。
- **修复**：对已有 `*Conn` 追加种子（`append` 到 peek 头）或直接 `panic`/返回 error。

### B22　`go.mod` 版本要求过高 + 依赖标记错误

- `go 1.25.0`：实际只用到 `min/max`（1.21）、`atomic.Bool/Int64`（1.19）→ 建议降到 `go 1.21`，否则挡住大量用户。
- `golang.org/x/sys v0.46.0 // indirect`：`checkconn_windows.go` **直接** import，不应标 indirect。
- 无 CI 配置：`readme.md:1` 的 Travis 徽章指向已失效服务，且仓库内无 `.travis.yml`（已确认文件不存在）。

### B23　README 文档与代码脱节（会直接误导用户）

| README 声明 | 实际 |
|---|---|
| `func NewConn(c net.Conn) net.Conn` | 不存在（实为 `New(c) *Conn`） |
| `func (T *Conn) SetReadDiscard(y bool)` | 不存在（实为 `SetBackgroundReadBuffer(-1)`） |
| `func (T *Conn) DisableBackgroundRead(y bool)` | 不存在（实为 `SetBackgroundReadBuffer(0)`） |
| `func (T *Conn) SetReadLimit(remain int)` | 签名是 `int64` |
| 缺少文档 | `NewData`、`RawConn`、`RawConnFull`、`SetBackgroundReadBuffer`、`WithContext`、`CancelNotify`、`IsClosed`、`ErrRawConnAlreadyUsed` |

### B24　测试质量问题（会掩盖真实缺陷）

1. `fatal := t.Fatal` 传入子 goroutine（`conn_test.go:27,71,126,161`）——Go 官方明确禁止：`t.Fatal` 在非测试 goroutine 中只会 `Goexit` 该 goroutine，**测试仍然 PASS**，缺陷被静默吞掉。
2. `TestConn_RawConnFullPeeked`（`conn_test.go:506-508`）在 `len(peeked)==0` 时 `peeked[0]` **越界 panic**（测试自身崩）。
3. `TestConn_CurrentRawConnAndReadWriteNoPanic` 用 `recover()` 掩盖预期外的 panic，无法区分"预期 panic"与"真 bug"。
4. 全部用例都是"对端不说话就断开"，恰好避开 B01；没有"先发数据再断开"的回归用例。
5. 无 `-race` CI、无跨平台 CI、无 fuzz、无 goroutine 泄漏断言。

---

## 5. 并发安全逐字段审计

| 字段 | 保护方式 | 判定 | 说明 |
|---|---|---|---|
| `Conn.closeErr` | `c.mu` | ⚠️ **有竞争** | 写有锁，`CancelNotify`（`conn.go:138`）读无锁 |
| `Conn.closeSignals` | `c.mu` | ✅ | 但只增不减（B10） |
| `Conn.closed` | `atomic.Bool` | ✅ | 语义不一（B05/B14） |
| `Conn.rawReadUsed` | `atomic.Bool` | ✅ | |
| `readDeadline/writeDeadline` | `atomic.Value` | ✅ | 但会被后台读覆盖（B03） |
| `backgroundReadBuffer` | `atomic.Int64` | ✅ | 无范围校验（B19） |
| `connReader.peekByte` | `r.mu` | ✅ | 切片重切保留底层数组（内存滞留） |
| `connReader.peeked` | `atomic.Bool` + 语义 | ⚠️ | 与 `peekByte` 非同一临界区，"先查后用"有窗口（B11） |
| `connReader.inRead` | `atomic.Bool` | ❌ **逻辑竞争** | 前后台共用、无所有权（B07/B08） |
| `connReader.aborted` | `atomic.Bool` | ⚠️ | 与 `inRead` 组合非原子（检查-设置分离） |
| `connReader.readLimit` | `atomic.Int64` | ⚠️ | load-then-add 非原子（B15） |
| `connReader.cond` | `r.mu` | ✅ | 但 `Wait` 无超时（B02） |

**根本问题**：用一个 `inRead` 原子布尔同时表达"后台在预读"和"前台在读"，再用"把 deadline 设成 1970"作为打断手段。这套机制在**任何**并发场景下都不成立，是这个库多数 P0/P1 的共同根因。

---

## 6. 功能盲区清单（能力缺口）

| # | 缺口 | 影响 |
|---|---|---|
| F1 | 无"连接健康检查"导出 API（`checkConnection` 是死代码，且 Windows 实现错误） | 用户无法主动探活（心跳/池健康） |
| F2 | 无半关闭支持 `CloseWrite/CloseRead` | 服务端优雅关闭、HTTP 请求结束无法通知对端 |
| F3 | 无 `SyscallConn() syscall.Conn` 透传 | 与 `ipv4`/`netlink`/`quic`/`tls` 生态不兼容；无法自己做 poll/零拷贝 |
| F4 | 无 `io.ReaderFrom`/`io.WriterTo` | `io.Copy` 失去 `splice/sendfile`（实测约 1.2x 基线损失，大文件更明显） |
| F5 | 无 `SetNoDelay/SetKeepAlive/SetKeepAlivePeriod/SetLinger` 透传 | 常见 `if tc, ok := c.(*net.TCPConn)` 断言静默失败 → Nagle 开着、keepalive 未开 |
| F6 | `WithContext` 取消不关闭连接、不解除阻塞、不可重复调用/不可取消 | "上下文取消"语义失效（B10） |
| F7 | 无"关闭原因"读取接口（`Err() error`），只能靠 channel | 多处需要同步查询时不便 |
| F8 | 无"订阅-退订"配对（只订阅不退订） | 长生命周期连接 + 短请求订阅 → 泄漏（B10） |
| F9 | 丢弃模式（负 buffer）无显式 API、无计量 | 误用即静默吞数据（B19） |
| F10 | 未处理/未文档化 `*tls.Conn` 场景 | 后台预读会消耗**已解密**字节；提取 `RawConn` 后这些字节若不手动前置 → 流损坏 |
| F11 | 无 `Wrap(net.PacketConn)`/`WrapListener` | 适用范围仅限流式连接 |
| F12 | 无指标/回调（预读字节数、中断次数、监控启停） | 线上无法观测这套机制是否在工作（B01 因此长期未被发现） |

---

## 7. 建议的目标架构（供选择）

```
Conn
├── fgMu   sync.Mutex        // 串行化前台 Read（peek 消费 + socket 读）
├── mu     sync.Mutex        // closed / closeErr / signals / addr 快照
├── mon    *monitor          // 唯一常驻监控协程（不再每次 Read 新建）
└── opts   { closeOnDrop, peekMode, ringSize, wakeInterval }
```

监控器两种实现：

- **M1（改动小，1~1.5 天）**：保留"预读 1 字节"思路，但
  1. 监控协程**常驻**，读到数据后 park（`<-wake`），由前台 `Read` 唤醒；
  2. 后台读用**自己的 deadline**，绝不触碰用户 deadline；
  3. 打断改为 `wake` channel，`Close()` 先 `rwc.Close()`。
  预期：修掉 B01/B02/B03/B07；性能从 7.5x 降到 ~1.3x。

- **M2（推荐，2~3 天）**：`SyscallConn()` + `unix.Poll(fd, POLLIN|POLLHUP|POLLERR, timeout)` 常驻探测
  - 零字节消耗、能区分 FIN（`POLLIN` + `read()==0`）/RST（`POLLERR`+`SO_ERROR`）；
  - 后台零 goroutine churn、零 deadline 篡改；
  - Windows 用 `WSAPoll` / `WSAEventSelect`；
  - 非 TCP/不支持 poll 的 conn 自动降级到 M1。
  预期：性能 ≈1.05x，且 `CloseNotify` 语义最准确。

---

## 8. 修复方案选择清单

> 勾选后我按你的选择实施（可多选）。工时按"有测试覆盖"估算。

### 批次 A —— P0 热修（低风险，保持 API 兼容，建议立即做）

| 选项 | 内容 | 对应用例 | 工时 | 风险 |
|---|---|---|---|---|
| [ ] **A1** | 后台读常驻 + 缓冲满不退出（修核心断线检测失效） | B01 | 1.0d | 低 |
| [ ] **A2** | `Close()` 先 `rwc.Close()` 再通知；abort 加超时兜底 | B02 | 0.3d | 低 |
| [ ] **A3** | 所有退出路径恢复用户 `ReadDeadline` 快照 | B03 | 0.3d | 低 |
| [ ] **A4** | `closed bool` + `closeErr` 分离，加锁；`CancelNotify` 不再做接口比较 | B04 | 0.2d | 低 |
| [ ] **A5** | 远端断开后自动关闭底层 + `IsClosed()=true`（新增 `SetCloseOnRemoteDrop`） | B05 | 0.3d | 中（行为变更） |
| [ ] **A6** | `n>0` 时返回 `nil` 错误，EOF 延后 | B06 | 0.1d | 低 |
| [ ] **A7** | 关闭后地址返回快照、deadline 返回 `net.ErrClosed`、记忆首次 Close 错误 | B13/B14 | 0.3d | 低（行为变更） |
| [ ] **A8** | 修正 README（删除不存在 API，补齐新 API 文档） | B23 | 0.2d | 无 |

### 批次 B —— 并发模型重构（中风险，建议同版本）

| 选项 | 内容 | 对应用例 | 工时 | 风险 |
|---|---|---|---|---|
| [ ] **B1** | 前台读 `fgMu` 串行化；`inRead` 拆成 `bgRunning`/`fgReading`（消除伪超时与双读） | B07/B08 | 0.8d | 中 |
| [ ] **B2** | 监控协程常驻 + park/unpark（消除每次 Read 建协程） | B09 | 1.0d | 中 |
| [ ] **B3** | `CloseNotify` 支持退订；`WithContext` 取消即 Close；nil ctx 直接返回 | B10 | 0.5d | 中（API 增量） |
| [ ] **B4** | `RawConn/RawConnFull` 原子快照 + 终止前台读 + 返回 `(conn, peek, err)` | B11/B12 | 0.5d | 中（API 变更） |
| [ ] **B5** | `readLimit` 用 CAS 原子扣减 | B15 | 0.2d | 低 |
| [ ] **B6** | 新增 `-race` + goroutine 泄漏断言 + 15 个回归用例（含本报告全部探针） | B24 | 1.0d | 无 |

### 批次 C —— 跨平台与能力补齐

| 选项 | 内容 | 对应用例 | 工时 | 风险 |
|---|---|---|---|---|
| [ ] **C1** | 精确构建标签 + `x/sys/unix`；`js/wasip1/plan9` 桩；errno 常量分平台 | B16 | 0.8d | 低 |
| [ ] **C2** | 导出 `CheckAlive()/IsAlive()`；Windows 改 `SO_ERROR`，TCP 叠加 `TCP_INFO` | B17 / F1 | 0.8d | 低 |
| [ ] **C3** | 重写 `isCommonNetError`（白名单 + 超时排除 + 补全错误码） | B18 | 0.4d | 低 |
| [ ] **C4** | `SetBackgroundReadBuffer` 限幅；丢弃模式拆成 `SetDiscardMode` | B19 | 0.2d | 低 |
| [ ] **C5** | `NewData` 对已包装连接追加种子或返回 error | B21 | 0.1d | 低 |
| [ ] **C6** | 透传 `SyscallConn` + `ReaderFrom/WriterTo` + `SetNoDelay/KeepAlive/Linger` + `CloseWrite/CloseRead` | F2-F5/F11 | 1.5d | 低 |
| [ ] **C7** | `go.mod` 降到 `go 1.21`，`x/sys` 去 indirect，加 GitHub Actions（含 `-race` + 12 平台交叉编译） | B22 | 0.3d | 低 |
| [ ] **C8** | 监控器改用 poll（M2 架构），彻底去掉预读与 deadline 打断 | B01/B09 根治 | 2.5d | 中高 |

### 打包建议

| 方案 | 组成 | 工作日 | 适用 |
|---|---|---|---|
| **方案 ①「止血」** | A1–A8 | ≈2.7d | 只想尽快上线，不动并发模型 |
| **方案 ②「稳健」**（**推荐**） | A1–A8 + B1–B6 | ≈7.7d | 生产库，要求并发正确 + 性能可接受 |
| **方案 ③「重写监控」** | A + B + C8（丢弃预读，poll 探测） | ≈10d | 追求零损耗、语义最准 |
| **方案 ④「全量」** | A + B + C1–C8 | ≈13.5d | 要打造成通用基础库 |

> 我的建议：**先做 A1–A4 + A6（半天，立刻消除 3 个 P0 与 1 个 panic），再做 B1/B2，最后按需 C**。其中 **A1 是最高性价比的一项**——它决定了这个库是否还值得使用。

---

## 9. 验证与证据

- 探针源码（15 项 + 4 组基准，可直接放回包内运行）：
  `kelivo://chat/outputs/vconn-audit-probes_test.go.txt`
- 关键原始输出：
  ```
  BUG-1 CONFIRMED: Read still blocked 1.2s after a 150ms deadline (n=0)
  BUG-2 CONFIRMED: Close() blocked >800ms; socket never closed -> goroutine + fd leak
  BUG-3 CONFIRMED: CloseNotify blocked after CancelNotify(nil); close-once flag never latched
  BUG-5 CONFIRMED: closeSignals=500, goroutines 2 -> 53
  BUG-6a CONFIRMED: peer wrote 1 byte then closed; CloseNotify never fired within 600ms
  BUG-6b: IsClosed()=false, underlying fd still open
  BUG-12 CONFIRMED: healthy idle connection, concurrent Read #2 injected error into Read #1: i/o timeout
  BUG-13 CONFIRMED: panic: comparing uncomparable type vconn.uncomparableErr
  BUG-15 CONFIRMED: Read -> n=3 err=EOF data="abc"
  BenchmarkProbe14_Plain-8           737.7 ns/op  5552 MB/s
  BenchmarkProbe14_Wrapped-8         901.1 ns/op  4545 MB/s
  BenchmarkProbe14_WrappedNotify-8  5506.0 ns/op   744 MB/s
  ```
- 交叉编译：12 个 GOOS 通过，`js/wasm`、`wasip1/wasm`、`plan9/amd64` 失败（错误信息见 B16）。
- 现有测试：`go test ./...` 全绿（10.9s），`go test -race` 两项通过 —— **说明现有测试覆盖不到上述缺陷**（见 B24）。

---

## 10. 附：建议新增的回归用例（命名即规格）

```
TestNotify_PeerSendsDataThenCloses        // B01
TestClose_UnsupportedDeadlineConn         // B02
TestClose_UnblocksPendingRead             // B02/B12
TestReadDeadline_SurvivesBackgroundRead   // B03
TestCancelNotify_NilIsNoop                // B04
TestCancelNotify_UncomparableError        // B04
TestRemoteDrop_ClosesSocket               // B05
TestRead_DataThenEOF_NotTogether          // B06
TestRead_ConcurrentNoSpuriousTimeout      // B07
TestRead_ConcurrentNoInterleave           // B08
TestNotify_NoGoroutineGrowthPerRead       // B09
TestNotify_Unsubscribe                     // B10
TestRawConnFull_NoPeekLoss                // B11
TestAddr_ValidAfterClose                  // B13
TestDeadline_ErrClosedAfterClose          // B13
TestPlatform_CrossCompile                 // B16（CI 矩阵）
TestIsCommonNetError_Table                // B18
TestReadLimit_ConcurrentExact             // B15
```

---

## 11. 修复实施记录（方案③：A + B + C8，严格 API 兼容）

> 实施日期：2026-11-15　验证环境：go1.27.3 / windows-amd64，21 个 GOOS 交叉编译

### 11.1 状态总览

| 批次 | 项目 | 状态 |
|---|---|---|
| A1 | 后台监控常驻 + 缓冲满不停摆（B01） | ✅ 已实现（poll 探测，零字节消耗） |
| A2 | `Close()` 先关底层再通知、无上限等待移除（B02） | ✅ 已实现 |
| A3 | 用户 `ReadDeadline` 不被污染（B03） | ✅ 已实现（预读用自身超时并恢复） |
| A4 | `closed`/`closeErr` 分离、移除接口比较（B04） | ✅ 已实现 |
| A5 | `SetCloseOnRemoteDrop`（B05，可选自动关闭） | ✅ 已实现（默认关闭，保持半关闭语义） |
| A6 | 数据与错误分两次返回（B06） | ✅ 已实现 |
| A7 | 地址快照 / deadline 返回 `net.ErrClosed` / 记住首次 Close 错误（B13/B14） | ✅ 已实现 |
| A8 | 重写 README（B23） | ✅ 已完成 |
| B1 | 前台读 `fg` 串行化，消除伪超时（B07/B08） | ✅ 已实现 |
| B2 | 单常驻协程 + 空闲计数，不做 per-read goroutine（B09） | ✅ 已实现 |
| B3 | `WatchClose` 退订、`WithContextClose`（B10） | ✅ 已实现 |
| B4 | `RawConnFull` 原子快照 + 停监控（B11/B12） | ✅ 已实现 |
| B5 | `readLimit` 用 CAS 扣减（B15） | ✅ 已实现 |
| B6 | `-race` + 20 个回归用例（B24） | ✅ 已实现（`conn_fix_test.go`） |
| C1 | 精确构建标签 + 平台桩（B16） | ✅ 已实现（21 平台全部通过） |
| C2 | 导出 `CheckAlive` + Windows 改用 `SO_ERROR`（B17） | ✅ 已实现 |
| C8 | poll 探测替代预读（B01/B09 根治） | ✅ 已实现 |
| C3 | 重写 `isCommonNetError`（B18） | ⏸ 未做（保持原判定，仅按平台拆分常量） |
| C4 | `SetBackgroundReadBuffer` 限幅拆分 | ◑ 已限幅（±1 MiB）；`SetDiscardMode` 未新增 |
| C5 | `NewData` 种子语义（B21） | ✅ 已实现 |
| C6 | `SyscallConn/ReaderFrom/NoDelay/KeepAlive/CloseWrite` | ⏸ 未做（方案③不含） |
| C7 | `go 1.21` + CI | ⏸ 未做（`go.mod` 仍为 1.25；`x/sys` 已改为直接依赖） |

### 11.2 文件结构变化

| 文件 | 说明 |
|---|---|
| `conn.go` | 重写：状态机、通知订阅表、`Close` 顺序、新增 API |
| `conn_reader.go` | 重写：前台读串行化、预读、限流（CAS）、错误分离 |
| `conn_monitor.go` | 新增：单协程监控循环（首轮阻塞预读 + 空闲计数） |
| `probe.go` / `probe_poll.go` / `probe_windows.go` / `probe_unix.go` / `probe_stub.go` | 新增：内核级零消耗探测 |
| `poll_rdhup_linux.go` / `poll_rdhup_bsd.go` | 新增：`POLLRDHUP` 平台常量 |
| `probe_helper.go` | 新增：`syscall.Conn` 解包（支持 `*tls.Conn`） |
| `sockopt_*.go` | 新增：`SO_ERROR`（替换原 `checkconn_*.go`，修正 Windows 用错选项） |
| `errno_*.go` | 新增：错误码按平台拆分（plan9/js/wasip1 可编译） |
| `conn_fix_test.go` / `conn_bench_test.go` | 新增：回归用例与基准 |

### 11.3 验证结果

```
go build ./...                     ✅
go vet ./...                       ✅ 无告警
gofmt -l （本次改动文件）            ✅
go test -count=3 ./...             ✅ 23.7s / 40.0s 全绿（41 个用例）
go test -race ./...                ✅ 25.6s，无 DATA RACE
交叉编译 21 个 GOOS/GOARCH          ✅ 含 js/wasm、wasip1/wasm、plan9/amd64
```

性能（4 KiB，内存连接）：

| 场景 | 修复前 | 修复后 |
|---|---|---|
| 裸 conn | 737 ns | ~220 ns |
| 包装（无通知） | 901 ns（1.22x） | ~300 ns（1.35x） |
| 包装 + CloseNotify | **5506 ns（7.47x）** | **~300 ns（≈1.35x）** |

（两轮基准的绝对值为不同测试框架，看相对倍数即可：**通知开销从 6.1 倍降到 ~0**。）

### 11.4 关键实现决策

1. **不再"读数据"来探测断开**：改用 `poll(POLLRDHUP/POLLHUP)` 与 `WSAPoll`，因此有未读数据时也能识别 FIN（B01 的根因），且完全不消耗数据。
2. **预读降级为可选能力**：仅当 `SetBackgroundReadBuffer(n>0)` 且**空闲**时才做，且只在"探测到可读"后立即读（不阻塞）；对 `*tls.Conn` 等包装不再预读（避免卡在 TLS 记录边界、也避免把解密数据搬走）。
3. **前台读加 `fg` 互斥锁**：彻底消除"用过去时间 deadline 打断别人的读"这一设计，B07/B08 消失。
4. **`Close()` 顺序反转**：先关 fd 解阻塞 → 再分发通知 → 最后有上限地等监控退出（B02）。
5. **关闭原因一旦落定不可覆盖**：`notified` 标志 + `notifyRemote` 过滤"本地关闭导致"的伪远端错误。
6. **对端 EOF 之后、底层被外部关闭**：`Read` 统一上报 `io.EOF`，避免上层把正常结束当异常（兼容旧用例 `TestConn_Read_2`）。

### 11.5 已知限制（未在本次范围内）

- Solaris/AIX 无 `POLLRDHUP`，需接收缓冲区排空后才能识别 FIN（其余平台不受影响）。
- `js/wasip1/plan9` 无内核探测，退化为"带 10ms 自身超时的预读"，检测能力受 `SetBackgroundReadBuffer` 限制。
- 对不支持 deadline 的自定义 `net.Conn`，监控协程可能阻塞在预读中（`Close()` 有 1s 上限并会关闭 fd 强制解阻塞），与旧版一致。
- `RawConn()` 在提取瞬间若仍有 goroutine 阻塞在 `Read`，该 goroutine 会与新的持有者竞争同一 socket（`RawConnFull` 已先停监控；彻底解决需要新 API，见 C6 批次的 `RawConnData`）。
- `isCommonNetError` 仍保留 `EINVAL` 判定（B18 未修，保持行为兼容）。

