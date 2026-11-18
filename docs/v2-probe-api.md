# v2：连接状态探测与错误分类

> **历史文档**：本文记录 v2 的设计。v3 已把 `State` 与 `Kind` 合并为同一个 `Status`
> （见 [v3-status-merge.md](v3-status-merge.md)）；文中出现的标识符已按 v3 命名刷新
> （`KindOf` → `StatusOf`、`StateAlive` → `StatusAlive`、`State()` → `Snapshot()` 等），
> 但「两个枚举」这一结构描述属于 v2，请以 v3 文档与源码为准。

> 2026-11-18 · 影响范围：conn.go / conn_reader.go / state.go / kind.go / reuse.go /
> probe.go / probe_*.go / errno_windows.go

## 1. 问题

上一版把探测结论压进 `error`，同时暴露十余个哨兵错误（`ErrPeerClosedWithUnread`、
`ErrConnDirty`、`ErrBusy`、`ErrUnverified`、`ErrNotificationLatched`、`ErrNotReusable`、
`ErrReset*`、`ErrUnsupported` …），加上标准库的 `io.EOF` / `net.ErrClosed` /
`syscall.Errno` / `os.ErrDeadlineExceeded`。调用方要区分「怎么处置这条连接」，
只能靠一串 `errors.Is`，而且：

* 探测方法签名不统一：`CheckAlive() error`、`CheckAliveBlocking() error`，
  而池化需要的是「State + 原因」；
* `nil` 同时表示「干净存活」「有数据可读」「无法验证 fail-open」三种互斥语义；
* 同一个原因有多种表示（`syscall.ECONNRESET` 与 Winsock 原始码 10054），
  分类结果会随来源漂移。

## 2. 设计：两个正交的枚举

```
State（我观测到了什么）          Kind（我该怎么做）
────────────────────────         ────────────────────
低 4 位：类别                     PeerClosed / PeerReset / LocalClosed /
第 4 位：是否还有未读数据         Dirty / Busy / Quota / Latched /
                                  Unverified / Timeout / Misuse /
                                  Unsupported / Unknown
```

* **State 管控制流**：`Alive / DataPending / PeerClosed(±Unread) / PeerReset(±Unread)
  / LocalClosed / Unverified / Busy / Latched / Quota`。
  未读数据是一个**修饰位**而不是常量：否则 `类别 × 未读` 会按笛卡尔积膨胀，
  而 Go 的 `switch` 不检查穷尽性，漏一个分支只会静默走 `default`。
* **Kind 管处置**：`StatusOf(err)` 是全包唯一的错误分类入口，先问错误自己
  （`Statuser` 接口，零猜测），再对标准库/系统错误做集中映射。新增哨兵必须登记，
  否则 `status_test.go` 的黄金表会失败。

两者的关系：`State` 是这次探测的结论，`Kind` 是对错误的处置建议；
同一个 `probe` 同时产出二者，不允许各自维护。

## 3. 公开 API

| API | 语义 |
|---|---|
| `Check() (State, error)` | 非阻塞、零消耗探测。`StatusUnverified` **不是死亡** |
| `CheckBlocking() (State, error)` | 无 fd 连接退化为带 2ms 超时的阻塞预读 |
| `CheckAlive() error` | **Deprecated**，`Check` 的兼容包装（保留「数据可读 ⇒ nil」「无法验证 ⇒ nil」旧语义） |
| `CheckAliveBlocking() error` | **Deprecated**，同上 |
| `CheckReusable() (State, error)` | 池化判定：仅 `StatusAlive` 可复用；错误都匹配 `ErrNotReusable` |
| `State() State` | 不探测的纯快照，**永不返回 `StatusAlive`** |
| `StatusOf(err) Kind` / `Statuser` | 错误处置分类（唯一入口 / 可扩展接口） |

`State()` 不返回 `Alive` 是刻意的：缓存无法证明「对端还活着」，那需要一次探测。
因此它是**单调**的（除 `ResetNotify` 外只增不减），过期即安全；
而过期的「存活」可能是假的。适合做探测前的快速否定过滤与埋点。

## 4. 实现要点

* **探测只有一份实现**：`Conn.probe(block bool)`。
  `Check / CheckBlocking / CheckAlive* / CheckReusable` 都是它的视图，
  杜绝「同一结论两份映射逻辑」。
* **干净 FIN 只有一处判定**：`isCleanPeerClose`，
  `State` / `Kind` / 通知内容都依赖它；带 cause 的哨兵不算干净。
* **哨兵带 cause**：`peerClosedWithUnreadError{cause}` 保留 RST 等真实原因。
  `Unwrap` 仍包含 `io.EOF`（只判 EOF 的老代码行为不变），
  但 `terminalReadErr` 会优先上抛 `Cause()`，避免 Read 把异常断开降级成干净 EOF。
* **配额耗尽**：Read 返回 `ErrQuotaExhausted`（`%w` 包住 `io.EOF`），
  分类为 `StatusQuota` 而不是 `StatusPeerClosed`；且**不会**落定 `peerDown`
  （配额是调用方自己设的上限，不是对端关闭）。
* **Windows errno**：同时接受 Winsock 原始码（10054）与
  `syscall.ECONNRESET`（`APPLICATION_ERROR|23`），否则同一个 RST 会因来源不同
  得到不同 `Kind`。

## 5. 测试

| 测试 | 覆盖 |
|---|---|
| `TestStatusOf_GoldenTable` | 全部导出哨兵 + 标准库 + errno + 包装的黄金表（新增哨兵必须补行） |
| `TestState_Predicates` | 11 个公开 State 的 `String/Readable/Reusable/Ended/Unread` |
| `TeStatusPeerClosedWithUnreadError` | `Is/Unwrap/Cause/Kind`、`isCleanPeerClose`、`peerState` |
| `TestTerminalReadErr_KeepsCause` | 不变量 5：RST 不得被降级为干净 EOF |
| `TestCheck_*` / `TestCheckBlocking_*` | 存活 / 干净 FIN / FIN+未读 / RST / 本地关闭 / 无 fd / 预读有数据 |
| `TestCheckAlive_BackwardCompat` | 旧 API 的两条特殊语义 |
| `TestState_Snapshot*` | 快照永不说「存活」、单调、四种场景 |
| `TestCheckReusable_*` | Alive / Dirty / Latched / Quota / LocalClosed / 延迟错误 / 对端关闭 |
| `TestProbeConcurrent` | Check / State / CheckReusable 与读写并发 |

同时修正了三处**陈旧断言**（`TestConn_Read_2`、`Test_conn_SetBackgroundReadDiscard`、
`TestConn_DisableBackgroundRead`）：它们用 `err == io.EOF` 判定关闭通知，
而对端「发完数据再关闭」时本包按设计返回 `ErrPeerClosedWithUnread`
（满足 `errors.Is(err, io.EOF)`），直接比较会在监控轮次与前台读的时序竞争下
随机失败——正是本次要解决的「错误太多、调用方分不清」问题。
断言已改为 `StatusOf(err) == StatusPeerClosed`，并把结果经 channel 回传主 goroutine，
避免在子 goroutine 里 `t.Fatal` 导致的死锁。

## 6. 平台差异（已知局限）

* Windows 上 Winsock 只在 recv 时报告 TCP RST，`SO_ERROR` 常为 0，
  因此探测可能把 RST 报成干净的 FIN；真实原因仍由前台 Read 给出，
  且不会被 `terminalReadErr` 降级（`TestCheck_PeerReset` 已固化该行为）。
* BSD/macOS 无 `POLLRDHUP`，统一按 `recv(MSG_PEEK)` 的 0 字节判定 FIN。
* `net.Pipe` 等拿不到 fd 的连接只能靠阻塞预读；用户显式
  `SetBackgroundReadBuffer(0)` 时返回 `StatusUnverified`（fail-open）。
