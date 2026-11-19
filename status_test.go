package vconn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
)

// TestStatusOf_GoldenTable 全包唯一的「错误 → Status」映射表。
// 新增任何导出错误时必须补一行。
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
		{"ErrPeerDown", ErrPeerDown, StatusPeerClosed},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, StatusPeerReset},

		// —— 链式包装 ——
		{"notReusable(io.EOF)", notReusable(io.EOF), StatusPeerClosed},
		{"notReusable(WithUnread)", notReusable(ErrPeerClosedWithUnread), StatusPeerClosed},
		{"notReusable(ErrDataPending)", notReusable(ErrDataPending), StatusDataPending},
		{"notReusable(ErrBusy)", notReusable(ErrBusy), StatusBusy},
		{"notReusable(ErrUnverified)", notReusable(ErrUnverified), StatusUnverified},
		{"notReusable(ErrNotifyLatched)", notReusable(ErrNotifyLatched), StatusLatched},
		{"notReusable(ErrQuota)", notReusable(ErrQuota), StatusQuota},
		{"notReusable(net.ErrClosed)", notReusable(net.ErrClosed), StatusLocalClosed},

		// —— 本地关闭 ——
		{"net.ErrClosed", net.ErrClosed, StatusLocalClosed},
		{"io.ErrClosedPipe", io.ErrClosedPipe, StatusLocalClosed},
		{"ErrLocalClosed", ErrLocalClosed, StatusLocalClosed},

		// —— 复用判定 ——
		{"ErrDataPending", ErrDataPending, StatusDataPending},
		{"ErrBusy", ErrBusy, StatusBusy},
		{"ErrUnverified", ErrUnverified, StatusUnverified},
		{"ErrNotifyLatched", ErrNotifyLatched, StatusLatched},
		{"ErrNotifyCanceled", ErrNotifyCanceled, StatusLatched},
		{"ErrQuota", ErrQuota, StatusQuota},

		// —— 用法 / 不支持 ——
		{"ErrInUse", ErrInUse, StatusInUse},
		{"ErrUnsupported", ErrUnsupported, StatusUnsupported},
		{"ErrNotReusable(裸)", ErrNotReusable, StatusUnknown},

		// —— 控制流 ——
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

// customStatuser 自定义错误也能接入同一套分类
type customStatuser struct{}

func (customStatuser) Error() string  { return "custom" }
func (customStatuser) Status() Status { return StatusQuota }

func TestStatusOf_CustomStatuser(t *testing.T) {
	err := notReusable(customStatuser{})
	if got := StatusOf(err); got != StatusQuota {
		t.Fatalf("自定义 Statuser 应生效，得到 %v", got)
	}
	if !errors.Is(err, ErrNotReusable) {
		t.Fatal("notReusable 包装应满足 errors.Is(err, ErrNotReusable)")
	}
}

// statusRow 是某个 Status 取值的完整行为契约（观测轴 + 处置轴）
type statusRow struct {
	st        Status
	str       string
	readable  bool
	reusable  bool
	ended     bool
	unread    bool
	retryable bool
	terminal  bool
	errOnly   bool
}

var statusRows = []statusRow{
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
	{StatusQuota, "quota", false, false, false, false, false, true, false},
	{StatusTimeout, "timeout", false, false, false, false, true, false, true},
	{StatusInUse, "in-use", false, false, false, false, false, false, true},
	{StatusUnsupported, "unsupported", false, false, false, false, false, false, true},
	{StatusUnknown, "unknown", false, false, false, false, false, false, true},
}

// TestStatus_Predicates 逐值校验两个轴上的全部谓词
func TestStatus_Predicates(t *testing.T) {
	for _, r := range statusRows {
		t.Run(r.str, func(t *testing.T) {
			if got := r.st.String(); got != r.str {
				t.Fatalf("String() = %q, 期望 %q", got, r.str)
			}
			checks := []struct {
				name string
				got  bool
				want bool
			}{
				{"Unread", r.st.Unread(), r.unread},
				{"Readable", r.st.Readable(), r.readable},
				{"Reusable", r.st.Reusable(), r.reusable},
				{"Ended", r.st.Ended(), r.ended},
				{"Retryable", r.st.Retryable(), r.retryable},
				{"Terminal", r.st.Terminal(), r.terminal},
			}
			for _, c := range checks {
				if c.got != c.want {
					t.Fatalf("%s() = %v, 期望 %v", c.name, c.got, c.want)
				}
			}
			if differs := r.st != r.st.Class(); differs != r.unread {
				t.Fatalf("%v 的 unread 位与表不符：Class() = %v", r.st, r.st.Class())
			}
			if r.st.Class().Retryable() != r.retryable || r.st.Class().Terminal() != r.terminal {
				t.Fatalf("%v 带修饰位时的处置结论与不带修饰时不一致", r.st)
			}
		})
	}
}

// TestStatus_TableInvariants 表自身的元断言 + 覆盖性
func TestStatus_TableInvariants(t *testing.T) {
	for _, r := range statusRows {
		if r.errOnly != (r.st.Class() > StatusQuota) {
			t.Fatalf("%v 的 errOnly 标记与常量分组不符", r.st)
		}
		if r.ended && !r.terminal {
			t.Fatalf("%v 已结束却报告还能继续用（Ended ⊂ Terminal 被破坏）", r.st)
		}
		if r.reusable && (!r.readable || r.st != StatusAlive) {
			t.Fatalf("%v 不应可复用：可复用当且仅当 StatusAlive", r.st)
		}
		if r.unread && !r.readable {
			t.Fatalf("%v 带 unread 修饰却报告不可读", r.st)
		}
	}

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
		t.Fatalf("谓词表覆盖 %d 个类别，常量有 %d 个", len(plain), want)
	}
}

// 内部错误类型必须实现 Statuser
var (
	_ Statuser = peerClosedWithUnreadError{}
	_ Statuser = notReusableError{}
)

// TestConnIsNotStatuser 钉住命名决策：*Conn 不得实现 Statuser
func TestConnIsNotStatuser(t *testing.T) {
	if _, ok := any((*Conn)(nil)).(Statuser); ok {
		t.Fatal("*Conn 不应实现 Statuser：快照方法因此命名为 Snapshot")
	}
	if _, ok := any((*Conn)(nil)).(interface{ Snapshot() Status }); !ok {
		t.Fatal("*Conn 应提供 Snapshot() Status")
	}
}

// TestStatusOf_TerminationContract 断链 / 控制流 / 终结未结束 三类契约
func TestStatusOf_TerminationContract(t *testing.T) {
	for _, err := range []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		ErrPeerClosedWithUnread,
		peerClosedWithUnreadError{cause: syscall.ECONNRESET},
		net.ErrClosed,
		io.ErrClosedPipe,
		syscall.ECONNRESET,
		ErrPeerDown,
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
		ErrNotifyLatched,
		ErrInUse,
		ErrUnsupported,
	} {
		if st := StatusOf(err); st.Ended() {
			t.Fatalf("StatusOf(%v) = %v：控制流/用法错误不得报告连接已结束", err, st)
		}
	}

	for _, err := range []error{ErrDataPending, ErrQuota} {
		if st := StatusOf(err); st.Ended() || !st.Terminal() {
			t.Fatalf("StatusOf(%v) = %v：应「终结但未结束」", err, st)
		}
	}
}

// TestPeerClosedWithUnreadError 哨兵本身的双重匹配与分类
func TestPeerClosedWithUnreadError(t *testing.T) {
	clean := peerClosedWithUnreadError{}
	if !errors.Is(clean, io.EOF) {
		t.Fatal("必须满足 errors.Is(err, io.EOF)")
	}
	if !errors.Is(clean, ErrPeerClosedWithUnread) {
		t.Fatal("必须满足 errors.Is(err, ErrPeerClosedWithUnread)")
	}
	if clean.Cause() != nil {
		t.Fatal("干净 FIN 的 Cause 应为 nil")
	}
	if !isCleanPeerClose(clean) || clean.Status() != StatusPeerClosed {
		t.Fatal("干净 FIN 应被判定为 StatusPeerClosed")
	}

	reset := peerClosedWithUnreadError{cause: syscall.ECONNRESET}
	if !errors.Is(reset, io.EOF) || !errors.Is(reset, ErrPeerClosedWithUnread) {
		t.Fatal("带 cause 的哨兵仍须同时匹配 io.EOF 与哨兵")
	}
	if !errors.Is(reset, syscall.ECONNRESET) {
		t.Fatal("带 cause 的哨兵必须能在错误链里找到真实原因")
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

// TestTerminalReadErr_KeepsCause Read 路径不得把异常断开降级为干净的 io.EOF
func TestTerminalReadErr_KeepsCause(t *testing.T) {
	s, c := pipePair()
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

	vc.mu.Lock()
	vc.closeErr = peerClosedWithUnreadError{}
	vc.mu.Unlock()
	if err := vc.terminalReadErr(net.ErrClosed); err != io.EOF {
		t.Fatalf("干净 FIN 时应归一为 io.EOF，得到 %v", err)
	}
}

// TestStatus_Is Status.Is 必须与 StatusOf 永远等价（且只比较类别）
func TestStatus_Is(t *testing.T) {
	cases := []struct {
		st   Status
		err  error
		want bool
	}{
		{StatusAlive, nil, true},
		{StatusPeerClosed, io.EOF, true},
		{StatusPeerClosed, ErrPeerClosedWithUnread, true}, // 带 unread 修饰也命中类别
		{StatusPeerReset, peerClosedWithUnreadError{cause: syscall.ECONNRESET}, true},
		{StatusPeerClosed, peerClosedWithUnreadError{cause: syscall.ECONNRESET}, false},
		{StatusBusy, ErrBusy, true},
		{StatusLatched, ErrNotifyLatched, true},
		{StatusLatched, ErrNotifyCanceled, true},
		{StatusDataPending, ErrDataPending, true},
		{StatusQuota, ErrQuota, true},
		{StatusLocalClosed, notReusable(net.ErrClosed), true}, // 穿透伞错误
		{StatusPeerClosed, notReusable(io.EOF), true},
		{StatusUnknown, errors.New("boom"), true},
		{StatusBusy, io.EOF, false},
	}

	for _, c := range cases {
		if got := c.st.Is(c.err); got != c.want {
			t.Fatalf("%v.Is(%v) = %v，期望 %v", c.st, c.err, got, c.want)
		}
		// 等价性：Is 就是 StatusOf(...).Class() 的比较
		if got := StatusOf(c.err).Class() == c.st.Class(); got != c.want {
			t.Fatalf("%v.Is(%v) 与 StatusOf 的类别不一致", c.st, c.err)
		}
	}

	// 带 unread 修饰的状态只比类别，因此结论与不带修饰时相同
	if !StatusPeerClosedWithUnread.Is(io.EOF) {
		t.Fatal("StatusPeerClosedWithUnread.Is(io.EOF) 应为 true：Is 只比较类别")
	}
}
