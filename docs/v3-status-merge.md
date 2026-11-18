# v3：`State` / `Kind` 合并为 `Status`

> 影响范围：state.go / kind.go / conn.go / conn_reader.go / reuse.go / probe.go /
> status_test.go（原 kind_state_test.go）/ readme.md
>
> 不提供兼容层：`State`、`Kind`、`KindOf`、`Kinder`、`State()` 等旧名一律移除。

## 1. 问题：两个枚举，一张表

v2 把「连接结论」拆成两个枚举（见 [v2-probe-api.md](v2-probe-api.md)）：

* `State`：探测结论，`Check / CheckBlocking / CheckReusable / State()` 返回；
* `Kind`：错误对调用方的处置分类，`KindOf(err)` 返回。

两者其实是**同一张表的两个投影**——9 个类别一一对应，只在 4 个「纯错误空间」的类别
（`Timeout / Misuse / Unsupported / Unknown`）和 `unread` 修饰位上不同：

| 类别 | v2 的 `State` | v2 的 `Kind` |
|---|---|---|
| 存活、干净 | `StateAlive` | `KindNone` |
| 存活但有未读数据 | `StateDataPending` | `KindDirty` |
| 对端正常结束 | `StatePeerClosed(±WithUnread)` | `KindPeerClosed` |
| 对端异常中断 | `StatePeerReset(±WithUnread)` | `KindPeerReset` |
| 本地已关闭 | `StateLocalClosed` | `KindLocalClosed` |
| 无法判定 | `StateUnverified` | `KindUnverified` |
| 正被占用 | `StateBusy` | `KindBusy` |
| 通知已落定 | `StateLatched` | `KindLatched` |
| 配额耗尽 | `StateQuota` | `KindQuota` |
| 超时 / 用法错误 / 不支持 / 无法归类 | —— | `KindTimeout/Misuse/Unsupported/Unknown` |
| 是否还有未读数据 | `flagUnread`（bit 4） | —— |

代价不是「多一个别名」，而是两份映射必然会漂移，而且是**可观测**的：

```go
// CheckReusable 里同一个 pending 错误给出两个矛盾答案
st, err := c.CheckReusable() // st = StatePeerReset（异常中断）
vconn.KindOf(err)            //    = KindUnknown  （未知）
```

同类漂移还有 `io.ErrUnexpectedEOF`：`KindOf` 归 peer-closed，而同一次断开的 `State`
是 peer-reset——一个是「对端正常收尾」，一个是「异常中断」，上层据此对非幂等请求做出
相反推断。调用方还得学两套术语、两套 `String()`、两次 `switch`。

## 2. 设计：一个类型，两个轴

```go
type Status uint8
//  低 4 位：类别（Class）
//  第 4 位：是否还有未读数据（Unread）
```

* **观测轴**（`Check / CheckBlocking / CheckReusable / Snapshot` 的现实结论）：
  `Readable() / Reusable() / Ended() / Unread()`；
* **处置轴**（`StatusOf(err)` 的动作建议）：`Retryable() / Terminal()`。

类型同一 ≠ 取值相同，两条不对称要记住：

* `StatusOf(err)` 可能答出 `StatusTimeout`（连接没坏，是调用方的控制流），
  `Check*` 永不返回它；
* `Check*` 可能返回 `StatusDataPending`（有数据，不是错误），而
  `StatusOf(ErrConnDirty)` 恰好也是同一个值——「脏」对两条轴本就是同一件事。

类别清单（`StatusAlive` 是零值，等价于旧 `KindNone`）：

```
共用（观测 + 处置）
  StatusAlive / StatusDataPending / StatusPeerClosed(±unread) / StatusPeerReset(±unread)
  / StatusLocalClosed / StatusUnverified / StatusBusy / StatusLatched / StatusQuota
仅错误空间（StatusOf 才可能返回，Check* / Snapshot 永不返回）
  StatusTimeout / StatusMisuse / StatusUnsupported / StatusUnknown
```

位域约束在编译期挡住：`const _ = 1 / (1 - StatusUnknown/16)` —— 类别一旦越过 16 个，
除零、编译失败，避免与 `flagUnread` 互相污染。

## 3. 分类只有一份

`StatusOf(err)` 是全包唯一的「错误 → 类别」映射（先问错误自己：`Statuser`，
再集中映射标准库 / 系统错误）。两处从它派生：

* `isCleanPeerClose(err)` = `StatusOf(err)` 判为 `StatusPeerClosed` **且**不带真实 `cause`；
* `peerState(err, unread)` = 把 `StatusOf` 的类别**收窄**到「对端结束」两个值
  （无法归类时保守报 `StatusPeerReset`），再叠加 unread 位。

`peerState` 不再自己写第二份判定，因此「同一个错误两个答案」在结构上不可能再出现。

唯一保留的合法例外（有测试钉住）：**对端确实结束了、但原因无法归类**时，
`CheckReusable` 保守报 `StatusPeerReset`，而 `StatusOf(err)` 如实报 `StatusUnknown`。

### 语义修正：`io.ErrUnexpectedEOF`

合并时必须二选一，这里选 **`StatusPeerReset`**：流在中途被截断意味着对端没有正常收尾，
报 `StatusPeerClosed` 会让上层对非幂等请求做出「请求已执行完」的错误推断。
`Retryable() / Terminal()` 在两个类别上完全相同，所以这个修正不改变重试策略，
只影响「能不能推断已执行完」这一点。

## 4. 迁移对照

| v2 | v3 |
|---|---|
| `State` / `Kind` | `Status` |
| `StateAlive` | `StatusAlive` |
| `StateDataPending` / `KindDirty` | `StatusDataPending` |
| `StatePeerClosed` / `KindPeerClosed` | `StatusPeerClosed` |
| `StatePeerClosedWithUnread` | `StatusPeerClosedWithUnread` |
| `StatePeerReset` / `KindPeerReset` | `StatusPeerReset` |
| `StatePeerResetWithUnread` | `StatusPeerResetWithUnread` |
| `StateLocalClosed` / `KindLocalClosed` | `StatusLocalClosed` |
| `StateUnverified` / `KindUnverified` | `StatusUnverified` |
| `StateBusy` / `KindBusy` | `StatusBusy` |
| `StateLatched` / `KindLatched` | `StatusLatched` |
| `StateQuota` / `KindQuota` | `StatusQuota` |
| `KindNone` | `StatusAlive`（`String()` 由 `none` 变为 `alive`） |
| `KindTimeout` / `KindMisuse` / `KindUnsupported` / `KindUnknown` | `StatusTimeout` / `StatusMisuse` / `StatusUnsupported` / `StatusUnknown` |
| `KindOf(err) Kind` | `StatusOf(err) Status` |
| `Kinder { Kind() Kind }` | `Statuser { Status() Status }` |
| `Check() (State, error)` | `Check() (Status, error)` |
| `CheckReusable() (State, error)` | `CheckReusable() (Status, error)` |
| `State() State` | `Snapshot() Status` |
| `Kind.Terminal() / Retryable()` | `Status.Terminal() / Retryable()`（不变） |

**方法命名为 `Snapshot()` 的原因**：`Statuser` 的方法签名是 `Status() Status`，
快照方法若叫 `Status()`，`*Conn` 会意外实现该接口（`StatusOf` 面对被包成 `error`
的连接就可能认错）。测试 `TestConnIsNotStatuser` 钉住了这一点。

## 5. 测试策略

原来是两张互不相干的表（`KindOf` 黄金表 + `State` 谓词表），这正是漂移能长期存活的
原因。现在合并成一张**契约表**，并加上交叉断言：

* `TestStatusOf_GoldenTable`：错误 → `Status`，唯一权威映射（含包装、`OpError`、
  `SyscallError`、errno、`context` 错误）。
* `TestStatus_Predicates`：`statusRows` 总表，逐值校验观测轴 + 处置轴 + `String()` +
  修饰位一致性。
* `TestStatus_TableInvariants`：表自身的元断言——`Ended ⊂ Terminal`、`Reusable ⟺ Alive`、
  `unread ⇒ Readable`、`errOnly` 标记与常量分组一致、每个类别恰好一行契约
  （新增常量忘了补行会失败）。
* `TestStatusOf_TerminationContract`：把「错误 → 类别 → 处置」串起来钉一遍
  （断链类必须 `Ended`，控制流类不得 `Ended`，脏 / 配额是「终结但未结束」）。
* `TestProbe_NeverReturnsErrorOnlyClasses`：`Check*` / `Snapshot` 只返回观测空间类别。
* `TestStatusOf_NoDriftWithProbe`：两轴答案必须一致，并钉住那唯一一处合法例外。
* `TestConnIsNotStatuser`：命名决策的护栏。
