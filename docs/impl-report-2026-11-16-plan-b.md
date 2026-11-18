# vconn 分层加固（方案 B）实施报告

> 日期：2026-11-16
> 范围：`conn.go` / `conn_monitor.go` / `conn_reader.go` / `probe*.go` / `sockopt*.go` / `errno*.go`
> 约束：**未改动任何大写字母开头的导出符号**（名称、签名、语义保持兼容）；仅调整内部实现、非导出字段与非导出辅助函数。

---

## 0. 结论

| 项目 | 结果 |
|---|---|
| `go test ./...` | 全部通过（此前 8 个用例失败） |
| `go test -race ./...` | 通过，无竞态告警 |
| `go vet ./...`（22 个 GOOS/GOARCH 组合） | 全部通过 |
| `go build ./...`（22 个组合，含 illumos） | 全部通过 |
| 导出 API 变更 | **无** |

修复前失败用例（基线）：

```
--- FAIL: TestConn_DiscardReadBufferSize        (3 组断言)
--- FAIL: TestConn_CloseNotifyRemoteClose       (net.Pipe 收不到 FIN)
--- FAIL: TestConn_ContinuousIdleBackgroundRead (二次空闲检测失效)
--- FAIL: TestConn_RawConnFullPeeked            (后台预读未发生)
--- FAIL: TestConn_RawConnFull                  (后台预读未发生)
--- FAIL: TestConn_SetBackgroundReadBuffer      (预读未发生 → 写端超时)
--- FAIL: TestResolvePeekBlock                  (unsupported 分支判定错误)
--- FAIL: TestCheckAliveBlocking_ProbeTimeout   (探活未等待)
```

---

## 1. 分层结构

```
┌─────────────────────────────────────────────────────────┐
│ L1 状态机层   Conn.closed/notified/peerDown/closeRet     │
│               transition 统一迁移（Close/autoClose/Reset）│
├─────────────────────────────────────────────────────────┤
│ L2 探测层     probeResult 四态                            │
│               probeUnsupported / Idle / Readable /        │
│               ReadableHangup / Closed                     │
├─────────────────────────────────────────────────────────┤
│ L3 生命周期层 monitorRun（quit/done）+ c.mon 一致性        │
└─────────────────────────────────────────────────────────┘
```

---

## 2. 逐项修改

### 2.1 探测层：新增 `probeReadableHangup`

**问题**：原实现把「有未读数据 + 对端 FIN」和「已断开」混为一谈。

- Linux：`POLLIN|POLLRDHUP` 同时置位 → 先判挂断 → `probeClosed` → 开启 `SetCloseOnRemoteDrop` 时立刻 `Close()`，**未读响应体被丢弃**；
- macOS/BSD：`POLLHUP` 不置位，只置 `POLLIN` → 被当成「有数据」→ `peekSize==0` 时直接返回，**FIN 永远检测不到**（核心能力失效）。

**修改**：`probe_poll.go` 改为「poll 判就绪 → `recv(MSG_PEEK)` 复核」两步，结果四态：

| poll 事件 | MSG_PEEK 结果 | 判定 |
|---|---|---|
| 无事件 | — | `probeIdle` |
| 就绪 | `n > 0` 且无挂断位 | `probeReadable` |
| 就绪 | `n > 0` 且有挂断位 | `probeReadableHangup`（新） |
| 就绪 | `n == 0` | `probeClosed` |
| `POLLNVAL` | — | `probeClosed` |

Windows 同步对齐（`WSAPoll` → `WSARecv(MSG_PEEK)`），`probeReadableHangup` 语义一致。

**上层处理**（`conn_monitor.go`）：

```go
case probeReadableHangup:
    c.notifyCloseEx(perr, kindRemote, false) // 通知，但不自动关底层
    return true
```

### 2.2 状态机层：`notifyCloseEx` + 延迟自动关闭

- `notifyClose(err, kind)` 保留为薄封装（`allowAutoClose=true`）；
- 新增 `notifyCloseEx(err, kind, allowAutoClose bool)`：
  - `allowAutoClose=false` 且开启了 `SetCloseOnRemoteDrop` → 置 `autoCloseDeferred`，暂不关闭；
- `Conn.Read` 在「读到终止错误且非超时」时调用 `tryDeferredAutoClose()`，数据读空后才真正关闭；
- `closeUnderlying()` 统一 autoClose / 延迟 autoClose 的状态迁移，保证 `Close()` 之后幂等返回首次结果。

### 2.3 状态机层：并发 `Close` 结果一致

原实现：后到者看到 `c.closed==true` 立刻返回 `c.closeRet`，但首个 Close 可能尚未写入 → 返回 `nil`（首个返回错误）。

修改：新增非导出字段 `closeDone chan struct{}`，后到者 `<-closeDone` 等首个 Close 收尾后再读 `closeRet`。

### 2.4 生命周期层：`c.mon` 一致性

- `monitorLoop` 的 defer 改为 `CompareAndSwap(m, nil)` + `close(m.done)`，监控自行退出时不再留下死实例；
- `stopMonitor` 引入 `pushed` 标记，只有真的把 deadline 改成「过去时间」催促预读时才恢复，避免多余 syscall 覆盖调用方刚设置的值。

### 2.5 生命周期层：`ResetNotify` 超时重试

`ErrResetTimeout` 后状态已清空、监控已退出。调用方按文档重试会命中幂等分支直接返回 `nil`，**监控不再启动**。
修改：幂等分支内 `if len(c.signals) > 0 { c.startMonitor() }`（内部已去重）。

### 2.6 数据面：`RawConnFull` 零丢失

原实现 `stopMonitor()` 超时返回被忽略，随后 `takePeek()` 可能早于在途 `peekRead` 完成 → 读出的字节写入已放弃的缓冲区。

修改：先 `r.fg.Lock()`（与 `peekRead` 互斥）→ `stopMonitor()` → `takePeek()` → 解锁。预读最长阻塞 `peekReadTimeout`，不会长时间等待。

### 2.7 数据面：限流含预读字节

`connReader.Read` 的 `consumeLimit(rn, limit)` 改为 `consumeLimit(n, limit)`。
`n = cn(peek) + rn(socket)`，只扣 `rn` 会让 `SetReadLimit(N)` 实际返回 `N + len(peek)`。

### 2.8 错误语义

| 文件 | 修改 |
|---|---|
| `errno_unix.go` | 移除 `EINVAL`（通用参数错误）；补齐 `ENETDOWN` / `ENETUNREACH` / `EHOSTUNREACH` |
| `sockopt_unix.go` | `getsockopt` 失败 → `ErrUnsupported`（原来返回 `gerr`，`Control` 失败返回 `cerr`，都会被 `checkKernel` 当成 `probeClosed`） |
| `sockopt_windows.go` | `Control` 失败 → `ErrUnsupported` |
| `conn_reader.go` | 数据 + 非超时错误同时返回时，错误一律进 `pending`（原来只留 `isCommonNetError` 的） |

### 2.9 阻塞探测判定 `isBlockProbeSafe`

原 `resolvePeekBlock` 对 `probeUnsupported` 只承认「直接 syscall.Conn」或「显式 SafeBlockProber」，
导致 `net.Pipe`（无 fd、无包装）连用户显式 `SetBackgroundReadBuffer(n>0)` 都无法预读 —— 与
`SetBackgroundReadBuffer` 的文档承诺直接冲突，也是 8 个失败用例的共同根因。

新规则（顺序敏感）：

1. 直接 `syscall.Conn` → 允许；
2. 实现 `SafeBlockProber` → 以 `SafeToBlockProbe()` 为准（可 opt-in / opt-out）；
3. 包装型连接（声明 `NetConn() net.Conn`，如 `*tls.Conn`）→ 跳过；
4. 其它叶子连接（`net.Pipe`、内存管道）→ 允许。

`isWrapperConn` 放在无构建标签的 `probe.go`，`probe_stub.go`（js/wasip1/plan9）复用同一判定。

### 2.10 探活无副作用

`ensurePeekForBlocking()`（`CAS(0 → 4096)`）删除，改为 `peekProbeBudget()`：只返回**本次**额度，
`peekProbe` 增加 `size` 参数，不再读写 `peekSize`。一次 `CheckAliveBlocking` 不再永久开启后台预读。

### 2.11 平台与死代码

| 项 | 处理 |
|---|---|
| illumos | `sockopt_unix.go`、`probe_unix.go` 补 `illumos` 构建标签（原先编译失败） |
| `checkconn_unix.go` / `checkconn_windows.go` | 删除（`checkConnection` 包内零调用；Windows 版还用 `SO_KEEPALIVE` 冒充 `SO_ERROR`） |
| 包文档 | 修正「unix: recv(MSG_PEEK)」→「poll(2) + recv(MSG_PEEK)」；`CheckAliveBlocking` 超时 10ms → 2ms |

---

## 3. 新增回归用例

| 用例 | 守护点 |
|---|---|
| `TestReadLimit_IncludesPeekedBytes` | 限流不被预读绕过 |
| `TestAutoCloseDeferred_KeepsDataReadable` | 延迟自动关闭保留数据 |
| `TestAutoClose_ThenCloseIsIdempotent` | 自动关闭后 `Close` 返回首次结果 |
| `TestClose_ConcurrentSameResult` | 并发 `Close` 结果一致 |
| `TestResetNotify_IdempotentRearmsMonitor` | 幂等重置补回监控 |
| `TestRawConnFull_NoByteLoss` | 提取原始连接零丢失 |
| `TestMonitor_SelfExitClearsInstance` | 监控自清 `c.mon` |
| `TestStopMonitor_NoRedundantDeadlineWrite` | 不再多余写 `ReadDeadline` |
| `TestIsCommonNetError_IgnoresGenericError` | 普通错误/超时不误判 |
| `TestIsNetErrno_RejectsGenericErrno` / `_WindowsDisconnect` / `_UnixDisconnect` | errno 表正确性 |
| `TestSocketError_UnsupportedNotClosed` | 探测失败 ≠ 已断开 |
| `TestRead_NonCommonErrorPreserved` | 遗留错误不丢 |
| `TestCheckAliveBlocking_NoSideEffectOnPeekSize` | 探活无副作用 |
| `TestIsBlockProbeSafe` | 阻塞探测判定（含 opt-in/opt-out） |
| `TestProbeConn_DataThenFIN_NotClosed` | 有未读数据不得判 closed |
| `TestProbeConn_LinuxHangupWithData` / `_LinuxPeerFINOnEmptyQueue` | Linux `POLLRDHUP` 区分 |
| `TestSyscallConnOf_UncomparableConn` / `_WrapperChain` | 解包不 panic、包装链可达 |

---

## 4. 兼容性说明

- **导出符号零变更**：`Conn` 及其全部方法、`CloseNotifier`、`SafeBlockProber`、`WrapSafeBlockProbe`、
  `New` / `NewData`、全部 `Err*` 哨兵，名称与签名均未改动。
- **`Conn` 结构体新增非导出字段**：`closeDone`、`autoCloseDeferred`。`Conn` 无导出字段、只能经
  `New` / `NewData` 构造，因此不影响任何外部代码。
- **删除的非导出符号**：`checkConnection`（两平台）、`ensurePeekForBlocking`、`probeConn` 内部的旧分支。
  外部不可引用。
- **行为变化**（均为修正方向，已在 README「与旧版本的行为差异」表登记）：
  1. `net.Pipe` 等叶子连接默认允许阻塞探测（原先被静默跳过，导致关闭检测失效）；
  2. `SetCloseOnRemoteDrop(true)` 在「有未读数据」时改为延迟关闭；
  3. `EINVAL` 不再被判定为断开。

---

## 5. 残余风险与后续建议

| 风险 | 说明 | 建议 |
|---|---|---|
| TLS wrapper 未实现 `NetConn()` | 会被当作叶子连接而阻塞探测，可能触发握手 | 文档已标注；建议这类 wrapper 实现 `SafeToBlockProbe() bool { return false }` |
| `peekTmp` 常驻不收缩 | 丢弃模式 `-1 MiB` 长期占 1 MiB/连接 | 可在 `SetBackgroundReadBuffer` 缩小时释放（需额外同步，暂未做） |
| iOS `go vet` | 测试二进制需 cgo 外部链接 | 用 `go build` 校验，或 CI 开 cgo |
| 结构性重构（方案 C） | 监控/预读/deadline 三处状态仍分散 | 待测试矩阵稳定后再评估抽 `session` 组件 |
