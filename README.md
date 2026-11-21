# vconn

[![Go Reference](https://pkg.go.dev/badge/github.com/456vv/vconn.svg)](https://pkg.go.dev/github.com/456vv/vconn)

`vconn` 把任意 `net.Conn` 包成一条带**连接状态**的连接。它回答两个标准库没有直接给出的问题：

- 对端是不是已经结束，哪怕本端还没把数据读完，也还没等到 `Read` 返回 `io.EOF`；
- 这条连接现在能不能交给下一个使用者。

探测在能拿到 socket 时不消耗字节。通知只说明发生了什么，**不会替你关闭底层连接**。

```
go get github.com/456vv/vconn
```

要求 Go 1.25 及以上。

## 它补上的三件事

标准 `net.Conn` 只有在 `Read` 碰到流结束时才知道对端走了。这在三种场景里不够用：

| 场景 | 只用 `Read` 的问题 | `vconn` 的做法 |
|---|---|---|
| 对端先发数据再 `FIN` | 必须把缓冲读空才看得到结束 | 内核探测区分「还有数据」和「队列已空」 |
| 只写不读（上传、推送、请求已发出） | 永远等不到 `EOF` | 空闲时按周期做零消耗探测 |
| 连接放回池子之前 | 不知道里面是否残留字节或半开 | `CheckReusable` 只接受干净存活 |

`New` 本身不启动后台协程。第一次 `CloseNotify` 或 `WatchClose` 才为一整条连接启动**一个**监控协程。没有人订阅时，不做周期探测。

## 五分钟接上

```go
raw, err := net.Dial("tcp", "127.0.0.1:8080")
if err != nil {
    return err
}
c := vconn.New(raw)
defer c.Close()

go func() {
    err := <-c.CloseNotify()
    switch {
    case errors.Is(err, vconn.ErrPeerClosedWithUnread):
        // 对端已经结束，但还有字节没取走。先读完再 Close。
        io.Copy(io.Discard, c)
        c.Close()
    case errors.Is(err, io.EOF):
        c.Close()
    default:
        log.Printf("连接结束: %v", err)
        c.Close()
    }
}()
```

`ErrPeerClosedWithUnread` 同时满足 `errors.Is(err, io.EOF)`。只判断 `EOF` 的旧逻辑行为不变；需要「先读完再收尾」时再加一个判断。

收到通知后是否关闭由调用方决定。对端只关闭写方向时，本端仍可能要写完响应；库如果在通知里直接 `Close`，会把这种半关闭变成全关闭。

## 三个日常问题

### 连接是不是已经结束

```go
err := c.Check()             // 非阻塞，不消耗字节
st := vconn.StatusOf(err)    // 完整结论（含 unread 修饰位）
switch st.Class() {
case vconn.StatusAlive:
    // 已确认存活，当前没有待读数据（此时 err == nil）
case vconn.StatusDataPending:
    // 还活着，但已经有数据。对探活来说是活的，对连接池来说是脏的
case vconn.StatusPeerClosed:
    // 干净结束。st.Unread() 为真说明接收队列里还有数据：先读完再收尾
case vconn.StatusPeerReset:
    // RST、断网、对端异常中止。st.Unread() 同上
case vconn.StatusLocalClosed:
    // 本端 Close，或原始连接已被取走
case vconn.StatusUnverified:
    // 这条连接拿不到 fd。这不是死亡
}
```

`Check*` 只返回一个 `error`：`nil` 当且仅当 `StatusAlive`，其余结论都装在错误里，用 `StatusOf(err)` 取出。所以 `err != nil` 不等于连接坏了——`StatusDataPending` / `StatusUnverified` / `StatusBusy` 都是「有话说」。

`CheckBlocking` 与 `Check` 的结论空间相同。差别只在拿不到 fd 时：前者会在确认读取安全后，用 2ms 超时读 1 字节；后者直接返回 `StatusUnverified`。

### 能不能放回连接池

```go
if err := c.CheckReusable(); err != nil {
    // err 一定满足 errors.Is(err, vconn.ErrNotReusable)
    st := vconn.StatusOf(err)
    log.Printf("丢弃连接: status=%s class=%s err=%v", st, st.Class(), err)
    c.Close()
    return nil
}
pool.Put(c) // 走到这里说明连接干净存活（err == nil ⟺ StatusAlive）
```

`CheckReusable` 比 `Check` 更严。有残留数据、前台正在读、通知已锁存、读配额耗尽、无法验证，全部不能复用。

不要对「服务端会主动推消息」的协议使用它。这类连接空闲时本来就可能有未读字节，结果会稳定地落在 `StatusDataPending`。

### 这个错误该怎么处理

`StatusOf` 是唯一的错误分类入口，`Read`、`Write`、`CloseNotify` 和 `Check*` 都能用：

```go
switch vconn.StatusOf(err).Class() {
case vconn.StatusPeerClosed:
    // 对端正常收尾。幂等请求可以换连接重试；请求可能已经执行完
case vconn.StatusPeerReset:
    // 异常中断。换连接；非幂等请求不要直接重试
case vconn.StatusDataPending:
    // 协议失步或残留响应。不要交给下一个人
case vconn.StatusBusy, vconn.StatusUnverified:
    // 此刻无法判定。稍后重试，不要当成死亡
case vconn.StatusTimeout:
    // 调用方自己的 deadline。连接不一定坏
case vconn.StatusQuota:
    // SetReadLimit 用尽。调大或重置限额，不是断线
case vconn.StatusLatched:
    // 这一轮通知被 CancelNotify / Context 取消锁住
case vconn.StatusLocalClosed:
    // 本端已经关闭
case vconn.StatusInUse, vconn.StatusUnsupported:
    // 用法或平台不支持。修调用，不要重试
}
```

自定义错误可以实现 `Statuser`：

```go
type Statuser interface {
    Status() vconn.Status
}
```

没有实现时，无法归类的错误是 `StatusUnknown`。`Status()` 必须是纯函数，不要在里面读 socket。

## 状态怎么读

`Status` 用一个字节同时表达类别和「还有没有未读数据」：

```text
低 4 位 = 类别
第 4 位 = 仍有未读数据
```

因此 `StatusPeerClosedWithUnread` 就是 `StatusPeerClosed` 加上未读标志。`switch` 请用 `Class()`，否则带未读标志的值会掉进 `default`。

| 结论 | 含义 | 还读得出来 | 能给下一个人 | 已经结束 |
|---|---|---:|---:|---:|
| `StatusAlive` | 干净存活 | 是 | 是 | 否 |
| `StatusDataPending` | 有残留数据 | 是 | 否 | 否 |
| `StatusPeerClosed` | 干净 `FIN`，队列已空 | 否 | 否 | 是 |
| `StatusPeerClosedWithUnread` | 干净 `FIN`，还有数据 | 是 | 否 | 是 |
| `StatusPeerReset` | 异常断开，队列已空 | 否 | 否 | 是 |
| `StatusPeerResetWithUnread` | 异常断开，还有数据 | 是 | 否 | 是 |
| `StatusLocalClosed` | 本端已关闭或已取走原连接 | 否 | 否 | 是 |
| `StatusUnverified` | 无法验证 | 否 | 否 | 否 |
| `StatusBusy` | 前台正在读 | 否 | 否 | 否 |
| `StatusLatched` | 通知已锁存，连接未必坏 | 否 | 否 | 否 |
| `StatusQuota` | 读配额用尽 | 否 | 否 | 否 |

后四个只出现在错误分类里，探测方法不会返回它们：`StatusTimeout`、`StatusInUse`、`StatusUnsupported`、`StatusUnknown`。

几个谓词的关系：

- `Reusable()` 只在 `StatusAlive` 时为真，并且可复用一定还可读；
- `Ended()` 只包含对端结束和本端关闭；
- `Terminal()` 比 `Ended()` 多两个：有残留数据、读配额用尽。连接可能还活着，但不能再交给别人；
- `PeerDown()` 只看对端，本端 `Close` 不算。

`Snapshot()` 不探测。它只报告已经确定的事实，所以**永远不返回 `StatusAlive`**：一份缓存证明不了对端现在还活着。已知结束时用它可以省掉一次系统调用；要肯定「还活着」，调用 `Check`。

## 通知的生命周期

关闭通知是一轮一次性事件：

```text
订阅 → 等待 → 某个原因首次落定 → 本轮所有 channel 收到同一个错误并关闭
```

落定之后，新的订阅会立刻拿到同一个原因。同一条连接要监听下一轮，必须先 `ResetNotify`，再重新订阅。旧 channel 不会被第二次写入。

```go
ch, cancel := c.WatchClose()
defer cancel()

select {
case err := <-ch:
    log.Printf("本轮结束: %v", err)
case <-time.After(time.Minute):
    cancel() // 只撤销自己的订阅，不关闭连接，也不影响其它订阅者
}
```

`CloseNotify` 是不能退订的 `WatchClose`。生命周期跟连接一样长的等待可以用它；请求级、可取消的等待用 `WatchClose`。

| 方法 | 做什么 | 不做什么 |
|---|---|---|
| `CloseNotify` | 返回本轮通知 channel，并在首次调用时启动监控 | 不提供退订 |
| `WatchClose` | 返回 channel 和幂等退订函数 | 退订不产生错误值，只关闭 channel |
| `CancelNotify(err)` | 主动落定一轮。`nil` 会写成 `ErrNotifyCanceled` | 不关闭底层连接 |
| `ResetNotify(false)` | 清掉可恢复的锁存，有订阅者时重启监控 | 本端已关闭或对端已断开时拒绝 |
| `ResetNotify(true)` | 连「对端已断开」的锁存也清掉，并立即重新探测 | 仍然越不过本端 `Close` / `RawConn` |
| `WithContext` | `ctx` 结束时落定通知，原因是 `ctx.Err()` | 不打断正在阻塞的 `Read` |
| `WithContextClose` | `ctx` 结束时调用 `Close` | 会真正断开底层连接 |

`ResetNotify` 的三个拒绝理由需要分开处理：

| 返回值 | 原因 | 下一步 |
|---|---|---|
| `ErrPeerDown` | 已经确认对端结束 | 默认丢掉连接。只有确认要再上报一次时才用 `ResetNotify(true)` |
| `ErrLocalClosed` | 本端已 `Close` 或已取走原连接 | 不能恢复。`force` 也无效 |
| `ErrBusy` | 上一轮监控还卡在预读里，但锁存已经清空（`StatusBusy`，不是超时） | 直接重试，不会因此启动第二轮监控 |

`CancelNotify` 与 `ResetNotify` 方向相反。前者把「未落定」变成「已落定」，后者把「已落定」变回「未落定」。

## 读取、预读和限额

`Read` 的交付顺序是：

1. 上次已经取到数据、但按 `io.Reader` 惯例延后返回的终止错误；
2. `SetReadLimit` 的剩余配额；
3. 本包预读缓冲里的字节；
4. 底层连接的 `Read`。

有字节时返回 `(n, nil)`，终止错误留到下一次。这样调用方不会因为同时看到数据和 `EOF` 而丢掉最后一段。

```go
c.SetBackgroundReadBuffer(32 << 10) // 空闲时最多预读 32 KiB
c.SetReadLimit(1 << 20)             // 本次响应最多向调用方交付 1 MiB
c.SetMonitorInterval(100 * time.Millisecond)
```

`SetBackgroundReadBuffer` 的符号决定后台读的用途：

| 值 | 行为 | 适合 |
|---|---|---|
| `0` | 不预读。有 fd 时仍做内核级关闭探测 | 默认值，也是连接池的正确选择 |
| `> 0` | 连接空闲时把最多这么多字节放进预读缓冲 | 希望对端一发数据就能尽快读到 |
| `< 0` | 空闲时直接丢弃读到的字节 | 明确不关心数据、只想发现关闭。**会丢数据** |

绝对值超过 1 MiB 会截成 1 MiB。缓冲按连接保留；曾经用过很大的丢弃长度后，需求降到 64 KiB 以下时会释放那块大缓冲。

显式调用过 `SetBackgroundReadBuffer` 之后，`CheckBlocking` 和 `CheckReusable` 不再使用自己的默认 4 KiB 额度。设为 `0` 或负数都表示「这次探测不准读数据」，无 fd 连接因此返回 `StatusUnverified`。

`SetReadLimit(n)`：

- `n < 0`：取消限额；
- `n >= 0`：之后还可交付给调用方的字节数；
- 限额用尽返回 `ErrQuota`。它满足 `io.EOF`，但 `StatusOf` 给出 `StatusQuota`，不会把连接标成对端已关闭。

`SetMonitorInterval` 是空闲连接两次探测之间的间隔，不是探测的超时。小于等于 `0` 恢复默认 200ms，低于 10ms 会提升到 10ms。正在运行的监控会被立刻唤醒并使用新间隔。

## 哪些连接可以安全探测

探测分两条路：

```text
能解包出 syscall.Conn
    → SO_ERROR + poll / WSAPoll + 零消耗查看
      TCP、Unix socket、以及实现了 NetConn() 的 *tls.Conn 走这里

不能解包
    → 只有 CheckBlocking / CheckReusable 才可能读 1 字节
      读不读由 SafeBlockProber 和连接形态决定
```

| 连接 | 实际路径 |
|---|---|
| `*net.TCPConn` | 内核探测，不读应用字节 |
| 直接包住的 `*tls.Conn` | 自动沿 `NetConn()` 解到 TCP，仍是内核探测 |
| 有 `NetConn()` 的包装 | 最多向下解 8 层；解到 socket 就走内核探测 |
| 无 `NetConn()` 的包装 | 无法判断它里面是不是 TLS，阻塞探测可能被当成叶子连接 |
| `net.Pipe`、纯内存连接 | 没有 fd，允许 2ms 阻塞读 |
| `SafeToBlockProbe() == false` | 跳过阻塞读，返回 `StatusUnverified` |

隐藏了 TLS 或多路复用、又没有 `NetConn()` 的包装必须显式拒绝阻塞读：

```go
func (w *wrapper) SafeToBlockProbe() bool { return false }
```

更好的修复是把内层连接暴露出来：

```go
func (w *wrapper) NetConn() net.Conn { return w.inner }
```

只有「提前 `Read` 不会改变协议状态」的纯字节连接，才实现 `SafeToBlockProbe() bool { return true }`，或使用 `WrapSafeBlockProbe`。不要对 `*tls.Conn`、HTTP/2、yamux、smux 这样做。提前读会触发握手、拆记录或吞掉帧头。

`WrapSafeBlockProbe` 会转发底层 `SyscallConn`。包住 `*net.TCPConn` 不会把内核探测降级成阻塞读。

## 交还原始连接

有两种所有权交还方式，每条连接只能成功调用一次，第二次 panic。

```go
// 不关心 vconn 已缓冲的字节时：
raw := c.RawConn()

// 连接池、代理或任何不能丢字节的场景：
raw, buffered := c.RawConnFull()
```

`RawConn` 把底层连接交回去，但丢弃本包预读缓冲。`RawConnFull` 会先停掉监控，再取走全部预读字节。调用期间若另有 goroutine 正阻塞在 `Read` 上，那个 `Read` 会被一个过去时间的 deadline 唤醒并返回超时；返回前会恢复调用方原来设置的读超时。

交还后，`vconn.Conn` 的 `Read` / `Write` 返回 `net.ErrClosed`，订阅者收到 `ErrInUse`。底层连接保持打开，关闭责任已经回到调用方。

已经通过 `NewData` 放进缓冲、或后台预读得到的字节，都要靠 `RawConnFull` 才能不丢。

## 平台行为

| 平台 | 零消耗探测 | 对端已结束但队列还有数据 |
|---|---|---|
| Linux、Android | `poll`，包含 `POLLRDHUP`；再用 `recv(MSG_PEEK)` 确认 | 能识别，并保留可能存在的真实错误 |
| macOS、iOS、FreeBSD、NetBSD、OpenBSD、Dragonfly | `poll` + `recv(MSG_PEEK)` | 能识别。BSD 的纯 `FIN` 往往只给 `POLLIN`，所以不能只看事件位 |
| Windows | `WSAPoll` + `FIONREAD`，必要时再 poll 一次 | 能识别。不使用同步 `WSARecv`，避免与前台读互相阻塞 |
| Solaris、illumos、AIX | 一次 `recv(MSG_PEEK)` | 队列里还有字节时先报告可读；读空后才能看到 `FIN` |
| js、wasip1、plan9 | 没有内核探测 | 只在阻塞读被判定安全时，退化为短超时预读 |

「探测不了」一律是 `StatusUnverified`，不是连接死亡，也不是可复用。

Windows 上有一个已知边界：Winsock 经常要到真正 `recv` 时才给出 TCP RST。零消耗探测可能先把这种状态看成干净结束；随后前台 `Read` 仍会收到真实的重置错误，库不会把它降级成 `io.EOF`。

## 时间与内存参数

| 参数 | 值 | 作用 |
|---|---|---|
| 监控周期 | 200ms，可改，最小 10ms | 只作用于空闲连接 |
| 阻塞探测超时 | 2ms | `CheckBlocking`、`CheckReusable` 和后台等待型预读 |
| 无超时兜底 | 10ms | 只在内部调用漏传超时时使用 |
| 停止监控上限 | 1s | `ResetNotify` 忙时返回 `ErrBusy`，状态已经清空 |
| 单次缓冲下限 | 4 KiB | 丢弃模式的最小读取块 |
| 单次缓冲上限 | 1 MiB | `SetBackgroundReadBuffer` 的绝对值上限 |
| 大缓冲保留上限 | 64 KiB | 超过后、下次只需小缓冲时释放 |

这些值是库的内部时序，不是业务超时。业务读写超时仍然只用 `SetDeadline`、`SetReadDeadline` 和 `SetWriteDeadline`。后台探测改写 deadline 时会在结束后恢复调用方最后一次设置的值。

## 接入时注意

- 所有导出方法都可并发调用。并发 `Read` 会串行执行，不会互相制造超时。
- `New(c)` 在 `c` 已经是 `*Conn` 时返回它本身。`New(nil)` 会 panic。
- `NewData(c, b)` 把 `b` 放进预读缓冲。连接池回收前，这些字节会让 `CheckReusable` 判定为脏。
- 监控只在连接空闲时探测。繁忙连接由 `Read` / `Write` 的错误路径负责报告断开。
- 后台探测看到对端结束时只发通知。调用方要关闭时，有未读数据就先读完。
- `SetKeepAlive`、`SetLinger`、`CloseRead`、`CloseWrite` 等可选能力会原样转给底层；底层没有该方法时返回 `ErrUnsupported`，关闭后返回 `net.ErrClosed`。
- `LocalAddr` 和 `RemoteAddr` 在 `New` 时复制常见地址类型，关闭后仍可读取。
- 不要让连接池里的连接开启正数预读。池化判定会把预读字节视为上一个使用者留下的数据。
- `Snapshot().Ended()` 适合做快速拒绝；它不能代替 `Check` 证明连接存活。

## 开发与测试

```bash
go test ./...
go test -race ./...
go test -bench . ./...
```

跨平台编译检查：

```bash
for spec in linux/amd64 darwin/arm64 freebsd/amd64 openbsd/amd64 illumos/amd64 aix/ppc64 windows/amd64 js/wasm wasip1/wasm plan9/386; do
  GOOS=${spec%/*} GOARCH=${spec#*/} go test -c -o NUL .
done
```

Windows 下把 `-o NUL` 换成 `-o NUL.exe` 即可。测试文件覆盖通知生命周期、并发读写、重置、原始连接交还、状态分类，以及 Linux、Windows 和 Unix 探测路径。

## 许可

本项目以仓库中的 `license` 文件为准。
