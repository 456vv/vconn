# vconn 方案二（稳健增强）实施报告

> 日期：2026-11-17
> 范围：`conn.go` / `conn_monitor.go` / `conn_reader.go` / `probe*.go` / `sockopt_windows.go`
> 目标：在保持既有导出 API 语义的前提下，修复关闭状态机竞态、探测误判与资源泄漏，并补齐常用透传/观测能力。
>
> **勘误（2026-11-18）**：本文第 2 节列出的 `SetDeferredCloseTimeout`（延迟自动关闭，含 B8 兜底 timer）
> 与 `Stats()`（可观测计数）**未进入当前代码**：前者已随「延迟自动关闭」方案撤销，
> 后者的结构体虽保留但计数器与方法均未接线。阅读时请以代码为准。
> 当前探测/状态相关的公开能力见 [v2-probe-api.md](v2-probe-api.md)。

---

## 1. 修复的缺陷

| 编号 | 问题 | 处理 |
|---|---|---|
| B1 | Solaris/illumos/AIX 上非 socket fd 的 `ENOTSOCK/EOPNOTSUPP/EINVAL` 被判为 `probeClosed` | `probe_unix.go` 增加错误分类，返回 `probeUnsupported`；新增 `probe_unix_test.go` |
| B2/B10 | `RawConn` 与 `closeUnderlying` 的 TOCTOU：RawConn 返回后自动关闭仍可能关闭 rwc | `rawUsed` 置位与 `closeUnderlying` 检查统一到 `c.mu`；`closeUnderlying` 发现 `rawUsed` 直接放弃 |
| B3/B17 | `checkconn_*.go` 死代码，Windows 版用 `SO_KEEPALIVE` 冒充 `SO_ERROR` | 删除两个文件（包内无调用、未导出） |
| B4 | `Close` 与 `closeUnderlying` 并发时重复 `rwc.Close()`，返回值不一致 | 两者共用 `closed/closeDone/closeRet`；底层 `Close` 只执行一次，后到者等待并复用结果 |
| B5 | 监控启动前的读不计入 `readSeq`，首轮可能误判空闲 | `connReader.Read` 无条件累加 `readSeq` |
| B6 | `stopMonitor` 推的过去 deadline 可能被 `peekRead` 覆盖 | `peekBusy` 与 `SetReadDeadline` 统一到 `deadlineMu`；`peekRead` 发起前检查 `monitorStopping` |
| B7 | Close 并发时 `CheckAlive` 可能 fail-open | `probeUnsupported` 后复查 `closedA`，已关闭返回 `net.ErrClosed` |
| B8 | 「对端 FIN + 未读数据」且上层不再 Read 时 fd 泄漏 | 新增 `SetDeferredCloseTimeout`（默认 30s）兜底 timer；读空/Close/Reset 会取消 |
| B9 | `NewData` 对已关闭连接仍 `pushPeek` | 增加 `closedA` 检查 |
| B14 | Windows `sockErrorOf` 吞掉 `GetsockoptInt` 错误 | 改为返回 `(socketErr, getsockoptErr)`，后者归类为 `probeUnsupported` |
| B21 | `poll` 可读后数据被前台 Read 抢走，`MSG_PEEK` 的 EAGAIN 被当成 FIN | Unix 引入 `peekOutcome` 三态，`peekWouldBlock → probeIdle` |
| B22 | 前台 Read 阻塞时 `RawConnFull` 等 `fg` 可能永久卡住 | 先用过去时间 `ReadDeadline` 唤醒前台 Read，取得 `fg` 后恢复最新 deadline，再移交所有权 |
| B23 | `WrapSafeBlockProbe(*net.TCPConn)` 丢失 `SyscallConn`，退化为阻塞预读 | `safeBlockConn` 转发 `SyscallConn`（`probe_syscallconn.go`） |
| B13 | `SetMonitorInterval` 不唤醒现有 timer | `monitorRun` 增加 `wake` 通道，改周期立即生效 |
| F9 | `WSAPoll` 动态加载失败会 panic | `pollReadable` 先 `procWSAPoll.Find()`，错误按 `probeUnsupported` 处理 |

### Windows 探测方式调整

原实现 `WSAPoll + 同步 WSARecv(MSG_PEEK)` 有一个隐患：Windows 上 Go 管理的 socket 在 Win32 层是阻塞模式，若数据在 `poll` 与 `WSARecv` 之间被前台 `Read` 消费，`WSARecv` 会永久阻塞，而不是返回 `WSAEWOULDBLOCK`。

现改为：

1. `WSAPoll(0)` 判断可读/挂断；
2. `FIONREAD`（`WSAIoctl`）查询接收队列字节数（不消费、不阻塞）；
3. 若「可读但 0 字节」，再 `poll` 一次：
   - 不再可读 → 数据被并发读消费，返回 `probeIdle`；
   - 仍可读 → 按对端 FIN 处理。

该路径不会阻塞监控协程，也不会把并发读误判为 FIN。

---

## 2. 新增 API（向后兼容）

| API | 说明 |
|---|---|
| `SetDeferredCloseTimeout(d time.Duration)` | 延迟自动关闭兜底时限，`<=0` 恢复 30s |
| `Stats() Stats` | 返回 `ReadSeq/ProbeCount/PeekBytes/MonitorRounds/Notified/PeerDown/Closed` |
| `SetKeepAlive(bool) error` | 透传 TCP keepalive |
| `SetKeepAlivePeriod(time.Duration) error` | 透传 keepalive 周期 |
| `SetLinger(int) error` | 透传 linger |
| `CloseWrite() error` / `CloseRead() error` | 半关闭透传 |

底层不支持时统一返回 `ErrUnsupported`；连接已关闭时返回 `net.ErrClosed`。

---

## 3. 新增/修改的测试

| 文件 | 覆盖点 |
|---|---|
| `conn_plan2_test.go` | 关闭状态机（RawConn vs closeUnderlying、Close vs closeUnderlying）、延迟关闭 timer、RawConnFull 解阻塞、`classifyPeek` 三态、`readSeq`、监控 timer 唤醒、透传 API |
| `probe_unix_test.go` | Solaris/illumos/AIX 非 socket fd → `probeUnsupported` |
| `probe_windows_test.go` | Windows 空闲/可读/FIN/有数据+FIN 探测（`WSAPoll + FIONREAD`） |

验证命令：

```bash
go test ./...
CGO_ENABLED=1 go test -race ./...
go test -bench . -run '^$' ./...
```

跨平台：

```bash
GOOS=windows  GOARCH=amd64 go vet ./...
GOOS=linux    GOARCH=amd64 go vet ./...
GOOS=darwin   GOARCH=arm64 go vet ./...
GOOS=freebsd  GOARCH=amd64 go vet ./...
GOOS=netbsd   GOARCH=amd64 go vet ./...
GOOS=openbsd  GOARCH=amd64 go vet ./...
GOOS=dragonfly GOARCH=amd64 go vet ./...
GOOS=solaris  GOARCH=amd64 go vet ./...
GOOS=illumos  GOARCH=amd64 go vet ./...
GOOS=aix      GOARCH=ppc64 go vet ./...
GOOS=android  GOARCH=arm64 go build ./...
GOOS=js       GOARCH=wasm  go build ./...
GOOS=wasip1   GOARCH=wasm  go build ./...
GOOS=plan9    GOARCH=amd64 go build ./...
```

---

## 4. 残余风险

| 风险 | 说明 |
|---|---|
| `RawConn` 与 `Close` 同时调用 | 若 `Close` 先取得状态锁，`RawConn` 会等待关闭完成后返回一个**可能已关闭**的连接；调用方应在提取前检查 `IsClosed`。若 `RawConn` 先取得所有权，`Close` 不会关闭底层。 |
| `RawConnFull` 对不支持 deadline 的连接 | 无法用过去 deadline 唤醒前台 Read，仍可能等待调用方停止并发读；文档已注明。 |
| TLS 包装 + `SetCloseOnRemoteDrop(true)` | 探测的是底层 socket，`tls.Conn` 内部已缓冲的明文对探测不可见；极端情况下可能在 TLS 缓冲数据交付前关闭底层，建议业务侧补测试/评估。 |
| `peekTmp` 常驻不收缩 | 丢弃模式传入大负值会长期占用对应内存，文档已警告。 |
