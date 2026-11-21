package vconn

// ==================== error 载体：Check* 的唯一返回类型 ====================
//
// Check / CheckBlocking / CheckReusable 只返回 error：
//
//	nil  ⟺  StatusAlive
//
// 其余结论一律以 *result 返回，它把「观测结论」与「真实原因」装进同一个错误：
//
//	if err := c.CheckReusable(); err != nil {
//		switch vconn.StatusOf(err).Class() {
//		case vconn.StatusBusy, vconn.StatusUnverified:
//			backoff()          // 现在没法判定，稍后重试
//		default:
//			pool.Drop()        // 该丢弃
//		}
//	}
//
// 语义要点（与旧的 (Status, error) 二元组一一对应）：
//   - Status() 返回**完整**结论，含 unread 修饰位（旧 Check* 返回的 Status 原值）；
//   - Cause() 返回真实原因，可能就是 nil —— StatusDataPending / StatusUnverified /
//     StatusBusy 都不是故障，只是「有话说」；
//   - Unwrap() 保证 errors.Is/As 的旧语义完全不变。

// result 是 Check* 返回错误的唯一实现。
//
// 字段刻意保持极小：status（含 unread 位）+ cause。
// cause 里**不再套** result —— 避免 Cause() 返回另一个 result 造成双重包装，
// 也保证 terminalReadErr / isCleanPeerClose 的 Cause() 探测语义不被污染。
type result struct {
	status Status
	cause  error
}

// newResult 把 (Status, error) 二元组折叠成单一 error。
//
// status 的类别为 StatusAlive 时返回 nil —— 这是「nil ⟺ 存活且干净」的唯一来源，
// 也是 StatusOf(nil) == StatusAlive 的另一半。
func newResult(status Status, cause error) error {
	if status.Class() == StatusAlive {
		return nil
	}
	return &result{status: status, cause: cause}
}

func (r *result) Error() string {
	if r.cause == nil {
		return "vconn: " + r.status.String()
	}
	return "vconn: " + r.status.String() + ": " + r.cause.Error()
}

// Unwrap 让 errors.Is/As 的旧语义保持不变：
//
//	errors.Is(err, io.EOF)                    // 干净 FIN / 带 unread 的 FIN
//	errors.Is(err, net.ErrClosed)             // 本地已关闭
//	errors.Is(err, syscall.ECONNRESET)        // 异常中断（不会被降级成 EOF）
//	errors.Is(err, ErrPeerClosedWithUnread)   // 对端已结束但还有未读数据
//	errors.Is(err, ErrNotReusable)            // CheckReusable 的全部失败
//	errors.Is(err, ErrDataPending)            // 具体拒绝理由
func (r *result) Unwrap() []error {
	if r.cause == nil {
		return nil
	}
	return []error{r.cause}
}

// Cause 返回真实终止原因（StatusDataPending / StatusUnverified / StatusBusy 时为 nil）。
func (r *result) Cause() error { return r.cause }

// Status 返回完整结论（含 unread 位）。StatusOf 会优先取用它（Statuser 分支）。
func (r *result) Status() Status { return r.status }
