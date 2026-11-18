package vconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ==================== 辅助 ====================

// waitStatus 反复探测直到满足条件或超时（FIN/RST 的到达是异步的）。
func waitStatus(t *testing.T, vc *Conn, want func(Status) bool, d time.Duration) (Status, error) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		st, err := vc.Check()
		if want(st) {
			return st, err
		}
		if time.Now().After(deadline) {
			return st, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ==================== Status 分类 ====================

// TestStatusOf_GoldenTable 是本包唯一一张「错误 → Status」映射表。
//
// 新增任何导出错误时，必须在这里补一行：这是防止分类表腐烂的唯一手段。
// 用户可见的错误总数（sentinel + 标准库 + errno）远超调用方的分支承受能力，
// StatusOf 把它们收敛成同一套类别，本测试就是那张映射表的可执行版本。
//
// v3 之前这里有两张表（KindOf 的黄金表 + State 的谓词表），彼此没有交叉检查，
// 于是同一个延迟错误被分别答成「异常中断」与「未知」。现在两轴共用一个类型：
//
//	本表                     钉住「错误 → 类别」；
//	TestStatus_Predicates    钉住「类别 → 谓词（观测轴 + 处置轴）」；
//	TestStatusOf_NoDriftWithProbe 钉住两边不许互相打架（含唯一一处合法例外）。
func TestStatusOf_GoldenTable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want Status
	}{
		{"nil", nil, StatusAlive},

		// —— 对端结束 ——
		{"ErrPeerClosedWithUnread", ErrPeerClosedWithUnread, StatusPeerClosed},
		{"peerClosedWithUnreadError", peerClosedWithUnreadError{}, StatusPeerClosed},
		{"peerClosedWithUnread+cause", peerClosedWithUnreadError{cause: syscall.ECONNRESET}, StatusPeerReset},
		{"io.EOF", io.EOF, StatusPeerClosed},
		{"ErrResetPeerDown", ErrResetPeerDown, StatusPeerClosed},
		// 中途截断不是正常收尾：v2 的两张表在这一点上互相矛盾
		// （KindOf 说 peer-closed，State 说 peer-reset），合表时选了「异常中断」。
		// 截断意味着请求可能只执行了一半，报 PeerClosed 会让上层对非幂等请求
		// 做出「已执行完」的错误推断。Retryable/Terminal 不受影响。
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, StatusPeerReset},

		// —— 链式包装 ——
		{"notReusable(io.EOF)", notReusable(io.EOF), StatusPeerClosed},
		{"notReusable(WithUnread)", notReusable(ErrPeerClosedWithUnread), StatusPeerClosed},
		{"notReusable(ErrConnDirty)", notReusable(ErrConnDirty), StatusDataPending},
		{"notReusable(ErrBusy)", notReusable(ErrBusy), StatusBusy},
		{"notReusable(ErrUnverified)", notReusable(ErrUnverified), StatusUnverified},
		{"notReusable(ErrNotificationLatched)", notReusable(ErrNotificationLatched), StatusLatched},
		{"notReusable(ErrQuotaExhausted)", notReusable(ErrQuotaExhausted), StatusQuota},
		{"notReusable(net.ErrClosed)", notReusable(net.ErrClosed), StatusLocalClosed},

		// —— 本地关闭 ——
		{"net.ErrClosed", net.ErrClosed, StatusLocalClosed},
		{"io.ErrClosedPipe", io.ErrClosedPipe, StatusLocalClosed},
		{"ErrResetLocallyClosed", ErrResetLocallyClosed, StatusLocalClosed},

		// —— 复用判定 ——
		{"ErrConnDirty", ErrConnDirty, StatusDataPending},
		{"ErrBusy", ErrBusy, StatusBusy},
		{"ErrResetTimeout", ErrResetTimeout, StatusBusy},
		{"ErrUnverified", ErrUnverified, StatusUnverified},
		{"ErrNotificationLatched", ErrNotificationLatched, StatusLatched},
		{"ErrNotifyCanceled", ErrNotifyCanceled, StatusLatched},
		{"ErrQuotaExhausted", ErrQuotaExhausted, StatusQuota},

		// —— 调用方错误 / 不支持 ——
		{"ErrRawConnAlreadyUsed", ErrRawConnAlreadyUsed, StatusMisuse},
		{"ErrUnsupported", ErrUnsupported, StatusUnsupported},
		{"ErrNotReusable(裸)", ErrNotReusable, StatusUnknown},

		// —— 超时控制流 ——
		{"os.ErrDeadlineExceeded", os.ErrDeadlineExceeded, StatusTimeout},
		{"context.Canceled", context.Canceled, StatusLatched},
		{"context.DeadlineExceeded", context.DeadlineExceeded, StatusTimeout},
		{"OpError{ECONNRESET}", &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, StatusPeerReset},
		{"OpError{deadline}", &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}, StatusTimeout},
		{"SyscallError{ECONNRESET}", &os.SyscallError{Syscall: "recv", Err: syscall.ECONNRESET}, StatusPeerReset},
		{"OpError{net.ErrClosed}", &net.OpError{Op: "read", Net: "tcp", Err: net.ErrClosed}, StatusLocalClosed},
		{"wrap(io.EOF)", fmt.Errorf("read header: %w", io.EOF), StatusPeerClosed},
		{"wrap(context.Canceled)", fmt.Errorf("handler: %w", context.Canceled), StatusLatched},

		// —— 系统错误码 ——
		{"syscall.ECONNRESET", syscall.ECONNRESET, StatusPeerReset},
		{"syscall.EPIPE", syscall.EPIPE, StatusPeerReset},

		// —— 无法归类 ——
		{"自定义错误", errors.New("boom"), StatusUnknown},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StatusOf(c.err); got != c.want {
				t.Fatalf("StatusOf(%v) = %v, 期望 %v", c.err, got, c.want)
			}
		})
	}
}

// 自定义错误可以通过实现 Statuser 接入同一套分类。
type customStatuser struct{}

func (customStatuser) Error() string  { return "custom" }
func (customStatuser) Status() Status { return StatusQuota }

func TestStatusOf_CustomStatuser(t *testing.T) {
	err := notReusable(customStatuser{})
	if got := StatusOf(err); got != StatusQuota {
		t.Fatalf("自定义 Statuser 应生效，得到 %v", got)
	}
	// 仍然满足 ErrNotReusable 契约
	if !errors.Is(err, ErrNotReusable) {
		t.Fatal("notReusable 包装应满足 errors.Is(err, ErrNotReusable)")
	}
}

// ==================== Status 谓词总表 ====================

// statusRow 是某个 Status 取值的完整行为契约。
//
// 观测轴（Readable/Reusable/Ended/Unread）与处置轴（Retryable/Terminal）放在同一张表里：
// v3 合并两套枚举之后，两个轴共享同一组常量，契约也必须一起被盯着——
// 分成两张表正是 v2 出现漂移的土壤。
type statusRow struct {
	st        Status
	str       string
	readable  bool // 现在还能读出数据吗（观测轴）
	reusable  bool // 能交给下一个使用者吗（观测轴）
	ended     bool // 对端/本地已经结束了吗（观测轴）
	unread    bool // 是否带「还有未读数据」修饰位
	retryable bool // 换连接 / 稍后重试有意义吗（处置轴）
	terminal  bool // 这条连接还能用吗（处置轴）
	errOnly   bool // 仅错误空间：Check* / Snapshot 永不返回
}

var statusRows = []statusRow{
	//  st, str, readable, reusable, ended, unread, retryable, terminal, errOnly
	{StatusAlive, "alive", true, true, false, false, false, false, false},
	{StatusDataPending, "data-pending", true, false, false, false, false, true, false},
	{StatusPeerClosed, "peer-closed", false, false, true, false, true, true, false},
	{StatusPeerClosedWithUnread, "peer-closed+unread", true, false, true, true, true, true, false},
	{StatusPeerReset, "peer-reset", false, false, true, false, true, true, false},
	{StatusPeerResetWithUnread, "peer-reset+unread", true, false, true, true, true, true, false},
	{StatusLocalClosed, "local-closed", false, false, true, false, false, true, false},
	{StatusUnverified, "unverified", false, false, false, false, true, false, false},
	{StatusBusy, "busy", false, false, false, false, true, false, false},
	{StatusLatched, "latched", false, false, false, false, false, false, false},
	{StatusQuota, "quota-exhausted", false, false, false, false, false, true, false},
	{StatusTimeout, "timeout", false, false, false, false, true, false, true},
	{StatusMisuse, "misuse", false, false, false, false, false, false, true},
	{StatusUnsupported, "unsupported", false, false, false, false, false, false, true},
	{StatusUnknown, "unknown", false, false, false, false, false, false, true},
}

// TestStatus_Predicates 逐值校验两个轴上的全部谓词。
func TestStatus_Predicates(t *testing.T) {
	for _, r := range statusRows {
		t.Run(r.str, func(t *testing.T) {
			if got := r.st.String(); got != r.str {
				t.Fatalf("String() = %q, 期望 %q", got, r.str)
			}
			if got := r.st.Unread(); got != r.unread {
				t.Fatalf("Unread() = %v, 期望 %v", got, r.unread)
			}
			if got := r.st.Readable(); got != r.readable {
				t.Fatalf("Readable() = %v, 期望 %v", got, r.readable)
			}
			if got := r.st.Reusable(); got != r.reusable {
				t.Fatalf("Reusable() = %v, 期望 %v", got, r.reusable)
			}
			if got := r.st.Ended(); got != r.ended {
				t.Fatalf("Ended() = %v, 期望 %v", got, r.ended)
			}
			if got := r.st.Retryable(); got != r.retryable {
				t.Fatalf("Retryable() = %v, 期望 %v", got, r.retryable)
			}
			if got := r.st.Terminal(); got != r.terminal {
				t.Fatalf("Terminal() = %v, 期望 %v", got, r.terminal)
			}
			// 带修饰位的取值必须与 Class() 不同，且差别只在 unread 位。
			if differs := r.st != r.st.Class(); differs != r.unread {
				t.Fatalf("%v 的 unread 位与表不符：Class() = %v", r.st, r.st.Class())
			}
			// 处置谓词必须忽略修饰位：StatusPeerClosedWithUnread 与 StatusPeerClosed
			// 是同一个处置结论（只是"还有数据"这一个修饰不同）。
			if r.st.Class().Retryable() != r.retryable || r.st.Class().Terminal() != r.terminal {
				t.Fatalf("%v 带修饰位时的处置结论与不带修饰时不一致", r.st)
			}
		})
	}
}

// TestStatus_TableInvariants 表本身的元断言：从表里能算出来的关系必须自洽，
// 并且表必须覆盖每一个类别（新增常量却忘了补契约行，会在这里炸）。
func TestStatus_TableInvariants(t *testing.T) {
	for _, r := range statusRows {
		// 用 Class() 比：带 unread 修饰位的取值（0x12 等）数值上并不小。
		if r.errOnly != (r.st.Class() > StatusQuota) {
			t.Fatalf("%v 的 errOnly 标记与常量分组不符（错误空间专属类别从 StatusTimeout 起）", r.st)
		}
		if r.ended && !r.terminal {
			t.Fatalf("%v 已结束却报告还能继续用（Ended ⊂ Terminal 被破坏）", r.st)
		}
		if r.reusable && (!r.readable || r.st != StatusAlive) {
			t.Fatalf("%v 不应可复用：可复用当且仅当 StatusAlive（且必然可读）", r.st)
		}
		if r.unread && !r.readable {
			t.Fatalf("%v 带 unread 修饰却报告不可读", r.st)
		}
	}

	// 覆盖性：每个类别必须恰好有一行「不带修饰」的契约；
	// 带 unread 修饰的取值算同一个类别的第二个取值（允许有，且必须是 +unread 变体）。
	plain := make(map[Status]bool, len(statusRows))
	for _, r := range statusRows {
		c := r.st.Class()
		if r.st != c {
			if r.st != c|flagUnread {
				t.Fatalf("%v 不是类别 %v 的 unread 变体", r.st, c)
			}
			continue
		}
		if plain[c] {
			t.Fatalf("类别 %v 有多行不带修饰的契约", c)
		}
		plain[c] = true
	}
	if want := int(StatusUnknown) + 1; len(plain) != want {
		t.Fatalf("谓词表覆盖 %d 个类别，常量有 %d 个：新增类别后必须补一行", len(plain), want)
	}
}

// 内部错误类型必须实现 Statuser —— 这是 StatusOf 不需要猜测语义的前提。
var (
	_ Statuser = peerClosedWithUnreadError{}
	_ Statuser = notReusableError{}
)

// TestConnIsNotStatuser 钉住一个命名决策：*Conn 不得实现 Statuser。
//
// 正因为 Statuser 的方法是 Status() Status，连接快照方法才叫 Snapshot() 而不是
// Status()（否则 *Conn 会意外实现该接口，StatusOf 面对被包成 error 的连接会认错）。
func TestConnIsNotStatuser(t *testing.T) {
	if _, ok := any((*Conn)(nil)).(Statuser); ok {
		t.Fatal("*Conn 不应实现 Statuser：快照方法因此命名为 Snapshot")
	}
	if _, ok := any((*Conn)(nil)).(interface{ Snapshot() Status }); !ok {
		t.Fatal("*Conn 应提供 Snapshot() Status")
	}
}

// TestStatusOf_TerminationContract 把「错误 → 类别 → 处置」串起来钉一遍：
// 断链类错误必须报 Ended，控制流类错误不得报 Ended（它们是调用方自己的错，
// 不是连接死了——把它当成断链，池化与重试策略都会走偏）。
func TestStatusOf_TerminationContract(t *testing.T) {
	for _, err := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		ErrPeerClosedWithUnread,
		peerClosedWithUnreadError{cause: syscall.ECONNRESET},
		net.ErrClosed,
		io.ErrClosedPipe,
		syscall.ECONNRESET,
		ErrResetPeerDown,
	} {
		if st := StatusOf(err); !st.Ended() || !st.Terminal() {
			t.Fatalf("StatusOf(%v) = %v：断链类错误必须 Ended 且 Terminal", err, st)
		}
	}

	for _, err := range []error{
		os.ErrDeadlineExceeded,
		context.Canceled,
		context.DeadlineExceeded,
		ErrBusy,
		ErrUnverified,
		ErrNotificationLatched,
		ErrRawConnAlreadyUsed,
		ErrUnsupported,
	} {
		if st := StatusOf(err); st.Ended() {
			t.Fatalf("StatusOf(%v) = %v：控制流/用法错误不得报告连接已结束", err, st)
		}
	}

	// 脏与配额：连接本身可能还活着，但不能再交给下一个人 —— 终结但未结束。
	for _, err := range []error{ErrConnDirty, ErrQuotaExhausted} {
		if st := StatusOf(err); st.Ended() || !st.Terminal() {
			t.Fatalf("StatusOf(%v) = %v：应「终结但未结束」（Ended ⊂ Terminal）", err, st)
		}
	}
}

// TestProbe_NeverReturnsErrorOnlyClasses 探测 API 只返回观测空间的类别：
// StatusTimeout / StatusMisuse / StatusUnsupported / StatusUnknown 是错误空间专属，
// 一旦从 Check* / Snapshot 漏出来，调用方的 switch 就会把「连接没坏」读成别的意思。
func TestProbe_NeverReturnsErrorOnlyClasses(t *testing.T) {
	obs := func(name string, st Status, err error) {
		t.Helper()
		if st.Class() > StatusQuota {
			t.Fatalf("%s 返回了错误空间专属类别 %v (%v)", name, st, err)
		}
	}

	t.Run("健康TCP", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		st, err := vc.Check()
		obs("Check", st, err)
		st, err = vc.CheckBlocking()
		obs("CheckBlocking", st, err)
		st, err = vc.CheckReusable()
		obs("CheckReusable", st, err)
		obs("Snapshot", vc.Snapshot(), vc.Err())
	})

	t.Run("无fd连接", func(t *testing.T) {
		s, c := net.Pipe()
		defer s.Close()
		defer c.Close()

		vc := New(c)
		defer vc.Close()

		st, err := vc.Check()
		obs("Check", st, err)
		st, err = vc.CheckBlocking()
		obs("CheckBlocking", st, err)
		obs("Snapshot", vc.Snapshot(), vc.Err())
	})

	t.Run("无fd_对端关闭", func(t *testing.T) {
		s, c := net.Pipe()
		defer c.Close()

		vc := New(c)
		defer vc.Close()
		s.Close()

		st, err := vc.Check()
		obs("Check", st, err)
		st, err = vc.CheckBlocking()
		obs("CheckBlocking", st, err)
		obs("Snapshot", vc.Snapshot(), vc.Err())
	})

	t.Run("本地已关闭", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		vc.Close()

		st, err := vc.Check()
		obs("Check", st, err)
		obs("Snapshot", vc.Snapshot(), vc.Err())
	})

	t.Run("预读有数据", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.pushPeek([]byte("x"))
		st, err := vc.Check()
		obs("Check", st, err)
		obs("Snapshot", vc.Snapshot(), vc.Err())
	})
}

// TestStatusOf_NoDriftWithProbe 两轴不得互相打架：CheckReusable 给出的类别与
// StatusOf(err) 的类别必须一致。v2 的 State/Kind 双枚举正是在这里漂移。
//
// 唯一合法例外：对端确实结束了、但原因无法归类 —— CheckReusable 保守报
// StatusPeerReset，StatusOf 如实报 StatusUnknown。用例把这个例外钉死，
// 免得日后有人顺手「修」成随机漂移。
func TestStatusOf_NoDriftWithProbe(t *testing.T) {
	t.Run("可归类的延迟错误", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.fg.Lock()
		vc.r.pending = io.EOF
		vc.r.fg.Unlock()

		st, err := vc.CheckReusable()
		if st.Class() != StatusPeerClosed || StatusOf(err).Class() != StatusPeerClosed {
			t.Fatalf("两轴必须一致，得到 (%v, StatusOf=%v)", st, StatusOf(err))
		}
	})

	t.Run("无法归类的原因", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.fg.Lock()
		vc.r.pending = errors.New("boom")
		vc.r.fg.Unlock()

		st, err := vc.CheckReusable()
		if st.Class() != StatusPeerReset {
			t.Fatalf("原因无法归类时探测结论应保守报 StatusPeerReset，得到 %v", st)
		}
		if got := StatusOf(err).Class(); got != StatusUnknown {
			t.Fatalf("StatusOf 应如实报 StatusUnknown，得到 %v", got)
		}
	})
}

// ==================== 哨兵本身 ====================

func TestPeerClosedWithUnreadError(t *testing.T) {
	clean := peerClosedWithUnreadError{}

	if !errors.Is(clean, io.EOF) {
		t.Fatal("必须满足 errors.Is(err, io.EOF)：只判 EOF 的老代码行为不能变")
	}
	if !errors.Is(clean, ErrPeerClosedWithUnread) {
		t.Fatal("必须满足 errors.Is(err, ErrPeerClosedWithUnread)")
	}
	if clean.Cause() != nil {
		t.Fatal("干净 FIN 的 Cause 应为 nil")
	}
	if !isCleanPeerClose(clean) {
		t.Fatal("干净 FIN 应被 isCleanPeerClose 认可")
	}
	if clean.Status() != StatusPeerClosed {
		t.Fatal("干净 FIN 应归类为 StatusPeerClosed")
	}

	reset := peerClosedWithUnreadError{cause: syscall.ECONNRESET}
	if !errors.Is(reset, io.EOF) || !errors.Is(reset, ErrPeerClosedWithUnread) {
		t.Fatal("带 cause 的哨兵仍须同时匹配 io.EOF 与 ErrPeerClosedWithUnread")
	}
	if !errors.Is(reset, syscall.ECONNRESET) {
		t.Fatal("带 cause 的哨兵必须能在错误链里找到真实原因")
	}
	if reset.Cause() == nil {
		t.Fatal("带 cause 的哨兵 Cause() 不应为 nil")
	}
	if isCleanPeerClose(reset) {
		t.Fatal("带 cause 的哨兵不得被当作干净 FIN")
	}
	if got := StatusOf(reset); got != StatusPeerReset {
		t.Fatalf("RST 必须归类为 StatusPeerReset，得到 %v", got)
	}
	if peerState(reset, true) != StatusPeerResetWithUnread {
		t.Fatal("RST + 未读数据应映射为 StatusPeerResetWithUnread")
	}
	if peerState(clean, false) != StatusPeerClosed {
		t.Fatal("干净 FIN 应映射为 StatusPeerClosed")
	}
}

// TestTerminalReadErr_KeepsCause 哨兵带 cause 时，Read 路径不得把异常断开
// 降级成干净的 io.EOF（包文档不变量第 5 条）。
func TestTerminalReadErr_KeepsCause(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	vc := New(c)
	defer vc.Close()

	vc.mu.Lock()
	vc.peerDown = true
	vc.closeErr = peerClosedWithUnreadError{cause: syscall.ECONNRESET}
	vc.mu.Unlock()

	if err := vc.terminalReadErr(io.EOF); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("Read 应上抛真实原因 ECONNRESET，得到 %v", err)
	}
	if err := vc.terminalReadErr(net.ErrClosed); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("net.ErrClosed 也应被替换为真实原因，得到 %v", err)
	}

	// 干净 FIN 时保持旧语义：net.ErrClosed → io.EOF。
	vc.mu.Lock()
	vc.closeErr = peerClosedWithUnreadError{}
	vc.mu.Unlock()
	if err := vc.terminalReadErr(net.ErrClosed); err != io.EOF {
		t.Fatalf("干净 FIN 时应归一为 io.EOF，得到 %v", err)
	}
}

// ==================== Check ====================

func TestCheck_HealthyTCP(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	st, err := vc.Check()
	if st != StatusAlive || err != nil {
		t.Fatalf("健康连接应为 (StatusAlive, nil)，得到 (%v, %v)", st, err)
	}
	// 探测不应关闭连接
	if _, err := srv.Write([]byte("x")); err != nil {
		t.Fatalf("Check 不应影响底层连接: %v", err)
	}
}

func TestCheck_PeerClosedClean(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer vc.Close()

	srv.Close() // 干净 FIN，接收队列为空

	st, err := waitStatus(t, vc, func(s Status) bool { return s.Class() == StatusPeerClosed }, 2*time.Second)
	if st != StatusPeerClosed {
		t.Fatalf("对端干净关闭应为 StatusPeerClosed，得到 (%v, %v)", st, err)
	}
	if k := StatusOf(err); k != StatusPeerClosed {
		t.Fatalf("错误应归类为 StatusPeerClosed，得到 %v (%v)", k, err)
	}
	// 状态机一致性：Err / IsPeerClosed / Snapshot() 必须与探测结论一致
	if !vc.IsPeerClosed() {
		t.Fatal("探测结论应并入状态机（IsPeerClosed）")
	}
	if vc.Snapshot() != StatusPeerClosed {
		t.Fatalf("Snapshot() 快照应反映已落定结论，得到 %v", vc.Snapshot())
	}
}

func TestCheck_PeerClosedWithUnread(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer vc.Close()

	if _, err := srv.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	srv.Close() // 发完数据就关闭：FIN 到达时队列里还有未读字节

	st, err := waitStatus(t, vc, func(s Status) bool { return s.Readable() && s.Class() != StatusAlive }, 2*time.Second)
	if !st.Readable() {
		t.Fatalf("对端发完数据再关闭时，连接必须仍可读，得到 (%v, %v)", st, err)
	}
	if st == StatusAlive {
		t.Fatal("有未读数据时不得报告 StatusAlive")
	}
	if st.Unread() {
		// 能同时看到 FIN 与未读数据时必须给出哨兵，且归类仍是对端结束
		if k := StatusOf(err); k != StatusPeerClosed {
			t.Fatalf("未读数据 + FIN 应归类为 StatusPeerClosed，得到 %v (%v)", k, err)
		}
	}
}

func TestCheck_LocalClosed(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()

	if err := vc.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := vc.Check()
	if st != StatusLocalClosed || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("本地已关闭应为 (StatusLocalClosed, net.ErrClosed)，得到 (%v, %v)", st, err)
	}
	if k := StatusOf(err); k != StatusLocalClosed {
		t.Fatalf("应归类为 StatusLocalClosed，得到 %v", k)
	}
}

// TestCheck_NoFDUnverified 拿不到 fd 的连接（net.Pipe）无法内核探测：
// 非阻塞的 Check 必须如实报告 StatusUnverified，而不是伪装成存活。
func TestCheck_NoFDUnverified(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	vc := New(c)
	defer vc.Close()

	st, err := vc.Check()
	if st != StatusUnverified || err != nil {
		t.Fatalf("无 fd 连接应为 (StatusUnverified, nil)，得到 (%v, %v)", st, err)
	}
	if st.Ended() {
		t.Fatal("无法验证不等于已结束")
	}
}

// TestCheckBlocking_NoFDPeerClosed 无 fd 连接在阻塞兜底路径上仍能发现对端关闭。
func TestCheckBlocking_NoFDPeerClosed(t *testing.T) {
	s, c := net.Pipe()
	defer c.Close()

	vc := New(c)
	defer vc.Close()

	s.Close() // 对端关闭：net.Pipe 的 Read 会立刻返回 io.EOF

	st, err := vc.CheckBlocking()
	if st != StatusPeerClosed {
		t.Fatalf("阻塞兜底探测应发现对端关闭，得到 (%v, %v)", st, err)
	}
	if k := StatusOf(err); k != StatusPeerClosed {
		t.Fatalf("应归类为 StatusPeerClosed，得到 %v (%v)", k, err)
	}
}

// TestCheckBlocking_NoFDPeerAlive 无 fd 连接 + 对端存活：阻塞探测超时即视为存活。
func TestCheckBlocking_NoFDPeerAlive(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	vc := New(c)
	defer vc.Close()

	st, err := vc.CheckBlocking()
	if st != StatusAlive || err != nil {
		t.Fatalf("对端存活时应通过阻塞探测确认存活，得到 (%v, %v)", st, err)
	}
}

// TestCheckBlocking_BudgetZero 用户显式关闭预读时，无 fd 连接无法验证 → Unverified。
func TestCheckBlocking_BudgetZero(t *testing.T) {
	s, c := net.Pipe()
	defer s.Close()
	defer c.Close()

	vc := New(c)
	defer vc.Close()
	vc.SetBackgroundReadBuffer(0)

	st, err := vc.CheckBlocking()
	if st != StatusUnverified || err != nil {
		t.Fatalf("预读被显式关闭时应 fail-open 为 (StatusUnverified, nil)，得到 (%v, %v)", st, err)
	}
}

// TestCheck_PeekDataPending 预读缓冲里有字节时，即使内核队列为空也算脏。
func TestCheck_PeekDataPending(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	vc.r.pushPeek([]byte("abc"))

	st, err := vc.Check()
	if st != StatusDataPending || err != nil {
		t.Fatalf("预读缓冲有字节应为 (StatusDataPending, nil)，得到 (%v, %v)", st, err)
	}
	if !st.Readable() || st.Reusable() {
		t.Fatal("DataPending 应可读但不可复用")
	}
}

// TestCheck_PeerReset 对端强制 RST（SetLinger(0) + Close）。
//
// 两条要求：
//   - 探测必须判定连接已结束（是「干净 FIN」还是「RST」取决于平台能否从内核
//     取到原因：Winsock 只在 recv 时报告 RST，SO_ERROR 常为 0，
//     因此 Windows 上探测可能报成 PeerClosed —— 但绝不能报成存活）；
//   - 一旦探测判定为 RST（StatusPeerReset），前台 Read 就不得把原因降级：
//     必须仍然是 StatusPeerReset（不变量第 5 条）。
func TestCheck_PeerReset(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer vc.Close()

	if tc, ok := srv.(*net.TCPConn); ok {
		if err := tc.SetLinger(0); err != nil {
			t.Fatal(err)
		}
	}
	srv.Close()

	st, _ := waitStatus(t, vc, func(s Status) bool { return s.Ended() }, 2*time.Second)
	if !st.Ended() {
		t.Fatalf("RST 后必须判定连接已结束，得到 %v", st)
	}

	buf := make([]byte, 16)
	_, err := vc.Read(buf)
	if err == nil {
		t.Fatal("RST 后 Read 必须返回错误")
	}
	if st.Class() == StatusPeerReset {
		if k := StatusOf(err); k != StatusPeerReset {
			t.Fatalf("探测已判定 RST 时 Read 不得降级原因，得到 %v (%v)", k, err)
		}
	}
}

// ==================== 探测 API 语义 ====================

// TestCheck_ProbeSemantics Check/CheckBlocking 的关键语义：
// 数据可读（unread）不算终结结论，无法验证不是死亡，本地关闭返回 net.ErrClosed。
func TestCheck_ProbeSemantics(t *testing.T) {
	t.Run("无fd_阻塞兜底", func(t *testing.T) {
		s, c := net.Pipe()
		defer s.Close()
		defer c.Close()

		vc := New(c)
		defer vc.Close()
		st, err := vc.Check()
		if err != nil || st.Class() != StatusUnverified {
			t.Fatalf("内核探测不可用时应返回 StatusUnverified，得到 (%v, %v)", st, err)
		}
		st, err = vc.CheckBlocking()
		if err != nil || st.Class() != StatusAlive {
			t.Fatalf("阻塞兜底确认存活后应返回 StatusAlive，得到 (%v, %v)", st, err)
		}
	})

	t.Run("已关闭", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		vc.Close()

		for _, fn := range []func() (Status, error){vc.Check, vc.CheckBlocking} {
			st, err := fn()
			if !errors.Is(err, net.ErrClosed) || st.Class() != StatusLocalClosed {
				t.Fatalf("本地已关闭应返回 (StatusLocalClosed, net.ErrClosed)，得到 (%v, %v)", st, err)
			}
		}
	})

	t.Run("对端关闭", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer vc.Close()
		srv.Close()

		deadline := time.Now().Add(2 * time.Second)
		for {
			st, err := vc.Check()
			if err != nil {
				if st.Class() != StatusPeerClosed {
					t.Fatalf("应归类为 StatusPeerClosed，得到 %v (%v)", st, err)
				}
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("对端已关闭，Check 仍报存活")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
}

// ==================== Snapshot() 快照 ====================

// TestState_SnapshotNeverClaimsAlive 快照不能宣称「存活」：那需要一次探测。
func TestSnapshot_NeverClaimsAlive(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	if st := vc.Snapshot(); st != StatusUnverified {
		t.Fatalf("未探测过的健康连接快照应为 StatusUnverified，得到 %v", st)
	}

	if st, err := vc.Check(); st != StatusAlive || err != nil {
		t.Fatalf("Check 应为 (StatusAlive, nil)，得到 (%v, %v)", st, err)
	}
	// 探测过也不改变快照：本包不缓存「存活」这种会过期的乐观结论
	if st := vc.Snapshot(); st != StatusUnverified {
		t.Fatalf("快照仍应为 StatusUnverified，得到 %v", st)
	}
}

func TestSnapshot_Scenarios(t *testing.T) {
	t.Run("已落定通知", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.CancelNotify(nil)
		if st := vc.Snapshot(); st != StatusLatched {
			t.Fatalf("手动通知落定后快照应为 StatusLatched，得到 %v", st)
		}
	})

	t.Run("预读有数据", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		defer vc.Close()

		vc.r.pushPeek([]byte("x"))
		if st := vc.Snapshot(); st != StatusDataPending {
			t.Fatalf("快照应为 StatusDataPending，得到 %v", st)
		}
	})

	t.Run("本地关闭", func(t *testing.T) {
		vc, srv, ln := newTCPPair(t)
		defer ln.Close()
		defer srv.Close()
		vc.Close()

		if st := vc.Snapshot(); st != StatusLocalClosed {
			t.Fatalf("关闭后快照应为 StatusLocalClosed，得到 %v", st)
		}
	})
}

// ==================== CheckReusable ====================

func TestCheckReusable_Alive(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	st, err := vc.CheckReusable()
	if st != StatusAlive || err != nil {
		t.Fatalf("干净连接应为 (StatusAlive, nil)，得到 (%v, %v)", st, err)
	}
	if !st.Reusable() {
		t.Fatal("StatusAlive 应可复用")
	}
}

func TestCheckReusable_Dirty(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	vc.r.pushPeek([]byte("leftover"))

	st, err := vc.CheckReusable()
	if st != StatusDataPending {
		t.Fatalf("有未读数据应为 StatusDataPending，得到 (%v, %v)", st, err)
	}
	if !errors.Is(err, ErrNotReusable) || !errors.Is(err, ErrConnDirty) {
		t.Fatalf("应同时匹配 ErrNotReusable 与 ErrConnDirty，得到 %v", err)
	}
	if k := StatusOf(err); k != StatusDataPending {
		t.Fatalf("应归类为 StatusDataPending，得到 %v", k)
	}
}

func TestCheckReusable_Latched(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	vc.CancelNotify(nil) // 手动落定：连接未必坏，但没 Reset 前不可复用

	st, err := vc.CheckReusable()
	if st != StatusLatched || !errors.Is(err, ErrNotificationLatched) {
		t.Fatalf("应报告 StatusLatched/ErrNotificationLatched，得到 (%v, %v)", st, err)
	}
	if k := StatusOf(err); k != StatusLatched {
		t.Fatalf("应归类为 StatusLatched，得到 %v", k)
	}
}

func TestCheckReusable_Quota(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	vc.SetReadLimit(0)

	st, err := vc.CheckReusable()
	if st != StatusQuota || !errors.Is(err, ErrQuotaExhausted) {
		t.Fatalf("应报告 StatusQuota/ErrQuotaExhausted，得到 (%v, %v)", st, err)
	}
	if k := StatusOf(err); k != StatusQuota {
		t.Fatalf("应归类为 StatusQuota，得到 %v", k)
	}
	// 配额耗尽不是对端关闭：不得落定 peerDown
	if vc.IsPeerClosed() {
		t.Fatal("配额耗尽不得落定为对端断开")
	}
}

func TestCheckReusable_LocalClosed(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	vc.Close()

	st, err := vc.CheckReusable()
	if st != StatusLocalClosed || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("应报告 StatusLocalClosed/net.ErrClosed，得到 (%v, %v)", st, err)
	}
	if !errors.Is(err, ErrNotReusable) {
		t.Fatal("所有失败都必须匹配 ErrNotReusable")
	}
}

// TestCheckReusable_PendingError 延迟错误（上次 Read 遗留）必须让连接不可复用。
func TestCheckReusable_PendingError(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	vc.r.fg.Lock()
	vc.r.pending = io.EOF
	vc.r.fg.Unlock()

	st, err := vc.CheckReusable()
	if st != StatusPeerClosed {
		t.Fatalf("延迟 EOF 应报告 StatusPeerClosed，得到 (%v, %v)", st, err)
	}
	if k := StatusOf(err); k != StatusPeerClosed {
		t.Fatalf("应归类为 StatusPeerClosed，得到 %v", k)
	}
}

func TestCheckReusable_PeerClosed(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer vc.Close()

	srv.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		st, err := vc.CheckReusable()
		if st.Ended() {
			if !errors.Is(err, ErrNotReusable) {
				t.Fatalf("失败必须匹配 ErrNotReusable，得到 %v", err)
			}
			if k := StatusOf(err); k != StatusPeerClosed {
				t.Fatalf("应归类为 StatusPeerClosed，得到 %v (%v)", k, err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("对端已关闭，CheckReusable 仍报告 %v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ==================== 并发安全 ====================

// TestProbeConcurrent Check/Status/CheckReusable 与读写并发时不得 panic 或死锁。
func TestProbeConcurrent(t *testing.T) {
	vc, srv, ln := newTCPPair(t)
	defer ln.Close()
	defer srv.Close()
	defer vc.Close()

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		p := make([]byte, 64)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = vc.SetReadDeadline(time.Now().Add(5 * time.Millisecond))
			_, _ = vc.Read(p)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			srv.Write([]byte("ping"))
			time.Sleep(time.Millisecond)
		}
	}()

	for i := 0; i < 200; i++ {
		_ = vc.Snapshot()
		vc.Check()
		vc.CheckBlocking()
		_ = vc.IsPeerClosed()
		_ = vc.Err()
	}
	close(stop)
	wg.Wait()
}
