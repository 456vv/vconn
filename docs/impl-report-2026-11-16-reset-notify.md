# 实现报告：允许反复订阅 / 重置（vconn）

> 实施日期：2026-11-16　环境：go1.27.3 / windows-amd64（gcc 走 mingw 以支持 `-race`）
> 交付范围：**A1–A7（反复订阅/重置）+ B8（交叉编译修复）**
> 已按用户要求**撤销 B4（历史轮次/LastErr）与 B6（指标/回调）**，相关代码与文档已删除
> 相关文档：[审计与修复报告](audit-report-2026-11-15.md)
>
> **修订（API 精简）**：`ResetNotify(opts ...ResetOption)` + `ResetForce()` 已合并为
> `ResetNotify(force bool)`。原文中所有 `ResetNotify()` 读作 `ResetNotify(false)`，
> 所有 `ResetNotify(ResetForce())` 读作 `ResetNotify(true)`；`ResetOption` / `resetConfig` /
> `ResetForce` 三个标识符已删除，语义与安全边界不变。

---

## 1. 需求与结论

**需求**：同一条 `*Conn` 上要能「反复订阅 / 重置」关闭通知，而不是"通知一次、永久锁存"。

**结论**：已实现。把关闭通知从**一次性事件**升级为**可多轮事件**：

- 新增 `ResetNotify(force bool)`：清除 `notified/closeErr/peerDown` 锁存，重新武装；
- 监控协程由"一次性 channel"改为**按轮实例化**（`monitorRun`），可停止 → 重启，整条连接始终至多 1 个存活实例；
- 新增 `NotifyRound()` / `WatcherCount()` 两个轻量观测接口；
- 顺带修正：`CancelNotify(err)` / `WithContext` 不再把连接误标为「对端已断开」（`IsPeerClosed()` 不再说谎，也不会误触发 `SetCloseOnRemoteDrop` 的自动关闭）；
- B8：`checkconn_unix.go` 的 build tag 收窄，恢复 `js/wasip1/plan9` 交叉编译。

**兼容性**：无删除、无签名变更；未调用 `ResetNotify` 时行为与旧版完全一致（落定后订阅立即得到同一结果）。

---

## 2. 实现结果详细信息

### 2.1 新增/变更 API

| 签名 | 语义 | 失败原因 |
|---|---|---|
| `ResetNotify(force bool) error` | 清除已落定的通知状态；有订阅者时重启监控 | `ErrResetLocallyClosed` / `ErrResetPeerDown` / `ErrResetTimeout` |
| （`force=true`） | 允许在 `peerDown` 时也重置（越过第二条边界） | 第一条边界仍不可越过 |
| `NotifyRound() uint64` | 已完成的通知轮次（每次落定 +1，重置不回退） | — |
| `WatcherCount() int` | 当前仍在等待通知的订阅者数（不含已落定/已退订） | — |
| `ErrResetLocallyClosed` | 本地已 `Close`/`RawConn`，底层不可再用 → 拒绝重置（`force=true` 也越不过） | — |
| `ErrResetPeerDown` | 已探测到对端断开 → 默认拒绝重置 | — |
| `ErrResetTimeout` | 上一轮监控未在 `monitorStopTimeout`(1s) 内退出；状态已清空，可直接重试 | — |

### 2.2 多轮状态机

```
              WatchClose/CloseNotify
   ┌──────────────────────────────────────────┐
   ▼                                          │
 Unlatched ──notifyClose(kind)──► Latched ──ResetNotify(false)──► Unlatched
 (监控运行中)                    (监控已停)                  (下一次订阅重启新一轮)
```

| 状态 | `notified` | `Err()` | `IsPeerClosed()` | 监控 |
|---|---|---|---|---|
| Unlatched | false | nil | 事实值（仅真实断开才 true） | 有订阅者时运行 |
| Latched | true | 落定原因 | 仅 `kindRemote` 时 true | 已停止 |
| 重置后 | false | nil | false | 下一次订阅时启动新一轮 |

### 2.3 关键改动点

1. **监控实例化（核心）**：新增 `monitorRun{quit, done, stopOnce}`，`Conn.mon atomic.Pointer[monitorRun]` + `monMu` 串行化创建；删除 `monStarted/monQuit/monDone/monQuitOnce`。
   旧 `monQuit` 只能 `close` 一次，天然不支持"再启动"，这是"重置"无法实现的根因。
2. **`startMonitor` 幂等且可换代**：已有存活实例直接返回；若上一轮实例已收到退出信号（`quit` 已关闭）则新建一轮，旧实例自行收尾。
3. **`stopMonitor` 返回是否确认退出**，并用 `CompareAndSwap` 只清理自己的实例，不会误清并发重置写入的新实例；超时（1s）不再无限等待。
4. **`monitorLoop` 首轮探测不再依赖 `readSeq==0`**：重置后立刻做一次内核探测（`poll`/`WSAPoll`，零字节消耗），不必等一个 200ms 周期；`allowBlock`（阻塞预读）仍只给"全新连接"的首轮。
5. **`ResetNotify` 三条安全边界**：本地已关闭 → 拒绝；`peerDown` → 拒绝（`force=true` 可越过）；本来未落定 → 幂等 `nil` 且**不打断正在工作的监控**。
6. **通知来源分类 `notifyKind`（内部）**：`peerDown` 只由 `kindRemote` 置位，`autoClose` 只由 `kindRemote` 触发。
   旧实现用"错误值是否等于哨兵"反推来源，导致 `CancelNotify(errors.New("x"))` 把连接标记成"对端已断开"——在"反复订阅"场景里会让 `ResetNotify` 永远被拒绝。
7. **`notifyRound` 计数**：`notifyClose` 落定时 +1，供 `NotifyRound()` 与"我等到的是第几轮"判定。
8. **`notifyClose(err==nil)` → `io.EOF`**：远端探测理论上可给出空错误，落定内容不落 `nil`，避免"已关闭但原因为空"。
9. **`conn_reader.go` 热路径 gate**：`monStarted.Load()` → `mon.Load() != nil`（同样是 1 次原子读）。
10. **B8**：`checkconn_unix.go` 的 `//go:build !windows` 改为 `//go:build !windows && !js && !wasip1 && !plan9`，并在文件头标注"死代码、可删除"。未删除文件，保持非破坏性。

### 2.4 改动文件清单

| 文件 | 变更 |
|---|---|
| `conn.go` | `Conn` 字段（`monMu/mon/round`）、内部 `notifyKind`、`ResetNotify(force bool)`、`NotifyRound`、`WatcherCount`、3 个错误值、`notifyClose(err, notifyKind)` |
| `conn_monitor.go` | 重写：`monitorRun` 实例化、`startMonitor` 幂等换代、`stopMonitor` 返回确认 + CAS、首轮探测调整 |
| `conn_reader.go` | 1 行：监控存活判定改用 `mon.Load()` |
| `conn_reset_test.go` | **新增**：11 个多轮订阅用例 |
| `checkconn_unix.go` | build tag 收窄 + 死代码说明（B8） |
| `readme.md` | 「反复订阅 / 重置」章节、接口表、迁移差异行、平台支持说明 |
| `docs/impl-report-2026-11-16-reset-notify.md` | 本文件 |

> 已删除（B4/B6 撤销）：`conn_observe.go`、`conn_observe_test.go`；`conn.go / conn_monitor.go / conn_reader.go` 中的历史记录、指标计数、回调、`NotifyKind/NotifyRecord/NotifyStats` 公开类型全部回退。

### 2.5 验证结果

```
gofmt -l（本次改动文件）                  ✅ 无输出
go build ./...                            ✅
go vet ./...                              ✅ 无告警
go test -timeout 600s -count=1 ./...      ✅ 14.7s（55 个用例，全绿）
go test -race -count=1 ./...              ✅ 15.7s，无 DATA RACE（CGO_ENABLED=1 + mingw gcc）
```

交叉编译（`go build ./...`，全部通过）：

| GOOS/GOARCH | 结果 |
|---|---|
| windows/amd64、windows/386 | ✅ |
| linux/amd64、darwin/arm64、freebsd/amd64、solaris/amd64 | ✅ |
| js/wasm、wasip1/wasm、plan9/amd64 | ✅（B8 修复前为 ❌） |

性能（4 KiB 读取，内存连接，i7-4700MQ，`-count=3`）：

| 场景 | ns/op | 相对 |
|---|---|---|
| 裸 `net.Conn` | 220.1 ~ 222.0 | 1.00x |
| `vconn` 包装 | 279.4 ~ 305.0 | 1.27 ~ 1.39x |
| `vconn` 包装 + 订阅通知 | 309.0 ~ 334.8 | 1.40 ~ 1.52x |

与重构前同一量级；订阅通知后比不订阅高约 5~10%（`readSeq` 原子自增，仅监控存活时计数）；0 alloc。
（B4/B6 撤销后指标计数已移除，实际热路径只剩这一处原子自增。）

### 2.6 新增回归用例（11 个，命名即规格）

| 用例 | 覆盖 |
|---|---|
| `TestResetNotify_RepeatedRounds` | 5 轮「订阅→通知→重置」，`NotifyRound` 累计、`WatcherCount` 归零、重置不改 `IsClosed` |
| `TestResetNotify_OldChannelNotRedelivered` | 旧 channel 保持关闭，不被二次投递；新订阅进入新一轮 |
| `TestResetNotify_ManualNotifyNotPeerDown` | `CancelNotify` 不再置 `peerDown`，重置无需 force |
| `TestResetNotify_NoopOnHealthyConn` | 未落定时重置幂等、不重启监控、不影响订阅者、监控仍能发现真实断开 |
| `TestResetNotify_RearmsMonitor` | 重置后重新订阅 → 新监控实例 → 对端关闭仍能通知（`NotifyRound=2`） |
| `TestResetNotify_PeerDownNeedsForce` | `ErrResetPeerDown` 拒绝；`ResetNotify(true)` 后立刻重新探测并再通知一轮 |
| `TestResetNotify_RefusedAfterClose` | 本地关闭后 `ErrResetLocallyClosed`（`force=true` 也越不过）；关闭后订阅不阻塞 |
| `TestResetNotify_RawConnRefused` | 提取原始连接后同样拒绝 |
| `TestResetNotify_KeepsPeekData` | 重置不清空预读缓冲区（`NewData` 种子仍可读出） |
| `TestResetNotify_NoGoroutineLeak` | 100 轮反复订阅/通知/重置，协程数 `base+8` 内，无残留监控 |
| `TestResetNotify_Concurrent` | 4 订阅者 + 1 重置者并发 300ms（配合 `-race`） |

### 2.7 已知限制与边界（重要）

1. **旧 channel 不会二次投递**：`closeSignal.fire` 的 `sync.Once` + channel 已关闭决定了一次投递。多轮必须重新订阅，不能复用旧 channel。
2. **`ResetNotify(true)` 在真正死掉的连接上会立刻再次通知**：强制重置后新一轮监控会马上探测到 FIN/RST 并落定第二轮。若在循环里 `for { ResetNotify(true) }` 会形成"通知风暴"——这是显式 opt-in 的代价，默认路径（`force=false`）不会出现。
3. **重置与 `Close` 竞争**：以最终状态为准。若 `Close` 先完成，重置返回 `ErrResetLocallyClosed`；若重置先完成，`Close` 正常落定新一轮通知。
4. **两代监控的极短重叠**：`ResetNotify` 采用「先 `stopMonitor` 等退出 → 再 `startMonitor`」规避；`WatchClose` 触发的换代（上一轮正在退出）不等待，此时若上一轮恰好卡在阻塞预读，理论上可能短暂覆盖 `ReadDeadline`——预读结束后会按最新快照恢复，且两代不会同时读同一 socket（`fg` 锁串行化）。
5. **`ResetNotify` 不清理**：`closeRet`（首次 Close 返回值）、预读缓冲区、`readLimit`、`monInterval` 等数据面状态一律保留。
6. **`ErrResetTimeout` 后连接处于"未落定但无监控"状态**：下一次 `WatchClose` 会重新启动监控，或直接重试 `ResetNotify`。
7. **`WithContext` / `WithContextClose` 不会自动重新武装**：它们内部的等待协程在上一轮通知后已退出；重置后需要重新调用。
8. **已撤销的观测能力**：B4（`LastErr/LastRecord/NotifyHistory`）与 B6（`Stats/OnNotify/OnMonitor`）按需求移除；`ResetNotify` 后**无法再查询上一轮原因**，只能靠 `Err()` 在重置前读取，或用 `NotifyRound()` 判轮次。若日后需要，可按原设计恢复（见第 3 节）。

---

## 3. 方案选择清单

### 批次 A —— 多轮订阅（已交付）

| 选项 | 内容 | 状态 |
|---|---|---|
| [x] **A1** | `ResetNotify` 解除 `notified/closeErr/peerDown` 锁存，支持多轮订阅 | ✅ |
| [x] **A2** | 监控协程按轮实例化（`monitorRun`），可停止 → 重启，至多 1 个存活 | ✅ |
| [x] **A3** | `ResetNotify(force bool)` + `ErrResetLocallyClosed/ErrResetPeerDown/ErrResetTimeout` | ✅ |
| [x] **A4** | `NotifyRound()` / `WatcherCount()` | ✅ |
| [x] **A5** | 修正 `CancelNotify` 误置 `peerDown`（内部 `notifyKind` 分类） | ✅ |
| [x] **A6** | 11 个回归用例（含 `-race`、协程泄漏、并发压测） | ✅ |
| [x] **A7** | README「反复订阅 / 重置」章节 + 迁移差异行 | ✅ |

### 批次 B —— 已撤销 / 未做

| 选项 | 内容 | 状态 |
|---|---|---|
| [—] **B4** | `LastErr/LastRecord` + 环形历史 | ❌ 按需求撤销（代码/文档/用例已删除） |
| [—] **B6** | `Stats()` 指标 + `OnNotify/OnMonitor` 回调 | ❌ 按需求撤销（代码/文档/用例已删除） |
| [x] **B8** | `checkconn_unix.go` build tag 收窄，恢复 js/wasip1/plan9 交叉编译（未删文件） | ✅ |
| [ ] **B1** | `SetAutoReset(true)`：每次通知后自动重置 | 0.3d / 中 |
| [ ] **B2** | 可复用 channel 多轮投递（channel 不关闭，直到退订） | 1.0d / 中高（破坏"关闭即结束"惯例） |
| [ ] **B3** | `WatchCloseRound(round)`：订阅时声明期望轮次，避免错过中间轮 | 0.5d / 低 |
| [ ] **B5** | `WithContext` 重置后自动重新订阅 | 0.3d / 中 |
| [ ] **B7** | `ResetOnlyNotified()`：只清 `notified` 不清 `peerDown` | 0.1d / 低 |
| [ ] **B9** | `go.mod` 降级 + CI（`-race` + 12 平台交叉编译） | 0.3d / 低 |
| [ ] **C1** | 删除 `checkconn_*.go` 死代码（B8 的非破坏性替代方案） | 0.05d / 低 |
| [ ] **C2** | `gofmt` 统一既有 CRLF 文件（`conn_test.go`、`checkconn_windows.go`） | 0.05d / 低 |

### 打包建议

| 方案 | 组成 | 工作日 | 适用 |
|---|---|---|---|
| **方案 ①「够用」**（**当前已交付**） | A1–A7 + B8 | 已完成 | 只需要"多轮订阅"能力，且交叉编译干净 |
| **方案 ②「可观测」** | ① + B4 + B6 | ≈0.8d | 需要查历史原因/上报指标（本次已按要求撤销） |
| **方案 ③「事件流」** | ① + B1/B2/B3 | ≈1.8d | 把断开当"可持续消费的事件流" |
| **方案 ④「全量」** | ① + B1–B9 + C1/C2 | ≈3.3d | 打造成通用基础库 |

---

## 4. 迁移说明

- 只调用 `CloseNotify()` 的既有代码：**行为不变**（落定后订阅立即返回同一结果）。
- 依赖 `CancelNotify(err)` 之后 `IsPeerClosed()==true` 的代码：**需改**。请改用 `Err()` 判断是否已落定；`IsPeerClosed()` 现在只表示"真实探测到对端断开/网络异常"。
- 依赖 `SetCloseOnRemoteDrop(true)` 在 `CancelNotify` 后自动关连接的代码：**需改**。手动通知不再触发自动关闭（这正是原语义 bug）。
- 需要"重置后仍能查上一轮原因"的代码：当前版本不支持（B4 已撤销）。替代做法是在通知到达时自行保存 `err` 与 `NotifyRound()`。
