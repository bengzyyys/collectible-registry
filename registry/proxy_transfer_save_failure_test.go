package registry

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

// 本文件为限时一次性代转（ProxyTransfer）补充"拒绝结果保存失败"场景的
// 回归保障，与单件转让（transfer_save_failure_test.go）、撤销授权
// （revoke_authorization_save_failure_test.go）等同类用例对齐：授权已到期、
// 已撤销、已使用、操作者无权、相关账户停用或持有版本不符等状态类业务拒绝，
// 原本应作为该请求的首次结果落盘，相同请求重提时回放。若拒绝结果尚未写入
// 原登记册就发生写入失败，ProxyTransfer 必须返回实际保存错误，不能只用到期、
// 无权或停用等业务错误掩盖，让调用者误以为这次拒绝已经记住。
//
// 保存失败时返回的结果必须为空：不带授权或藏品编号，不带转让与金额信息，
// Replayed 为 false，结果内的 Err 为空，单独返回的 error 保留保存失败原因。
// 这次失败不能占用操作者的请求号；授权内容、使用标记、授权变更记录，以及
// 藏品持有人、版本、转让历史和版税应付都保持调用前的业务状态——即使写入
// 失败后原数据暂时无法读取，同一个仍打开的登记册也必须满足这些要求。随后
// 其他操作成功保存不能把这次未保存的拒绝一并记住，也不能影响此前已经保存
// 的其他请求结果。
//
// 保存条件恢复后，完全相同的请求按重提时的状态重新判断，而不是回放失败时
// 的拒绝：到期拒绝保存失败后授权人完成撤销，重提应返回已撤销；该次保存
// 成功不标回放，此后原样重提才回放已撤销。已经成功保存的拒绝仍按首次结果
// 回放；改动业务内容仍按请求号冲突拒绝。必填内容缺失或引用不存在沿用现有
// 错误且不占用请求号。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先建
// 一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；另有
// 用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// assertProxySaveFailureEmpty 核对代转"拒绝结果保存失败"的返回：error 是
// 保存错误而非任何业务拒绝（含授权类的到期、已撤销、已使用等哨兵）；结果
// 为空——不携带授权或藏品编号、转让与金额信息，业务错误为空，也不标记为
// 重复返回。
func assertProxySaveFailureEmpty(t *testing.T, res ProxyTransferResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	for _, s := range []error{
		ErrAuthorizationExpired, ErrAuthorizationRevoked, ErrAuthorizationUsed,
	} {
		if errors.Is(err, s) {
			t.Fatalf("返回的应是保存错误，不能是业务拒绝 %v: %v", s, err)
		}
	}
	if res.AuthID != "" || res.ItemID != "" || res.FromID != "" || res.ToID != "" ||
		res.Version != 0 || res.TxSeq != 0 || res.Replayed || res.Err != nil ||
		res.Price != 0 || res.Payables != nil || res.Remainder != 0 {
		t.Fatalf("拒绝结果保存失败必须返回空结果: %+v", res)
	}
}

// assertProxyRequestFree 核对请求号未被这次未保存的拒绝占用（在同一个已
// 打开的登记册上检查）。
func assertProxyRequestFree(t *testing.T, r *Registry, req ProxyTransferRequest) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的拒绝不应占用请求号 %s", req.RequestID)
	}
}

// assertProxyRequestSaved 核对拒绝结果已作为首次结果登记落盘。
func assertProxyRequestSaved(t *testing.T, r *Registry, req ProxyTransferRequest) {
	t.Helper()
	prev, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]
	if !ok {
		t.Fatalf("拒绝保存成功后应登记请求结果，请求号 %s 却空闲", req.RequestID)
	}
	if prev.Kind != "proxy_transfer" || !prev.Rejected || prev.AuthID != req.AuthID {
		t.Fatalf("登记的请求结果异常: %+v", prev)
	}
}

// proxyExpiredWorld 在固定时钟下建立一份 1 小时后到期的授权 a1，并把时钟
// 拨到到期点之后：a1 实时状态为已过期，但未被撤销或使用。
func proxyExpiredWorld(t *testing.T, r *Registry) time.Time {
	t.Helper()
	authzWorld(t, r)
	base := time.Unix(2_000_000_000, 0)
	r.now = func() time.Time { return base }
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return base.Add(time.Hour + time.Second) }
	return base
}

// TestProxyTransferExpiredRejectSaveFailure 覆盖主线场景：授权到期的
// ErrAuthorizationExpired 拒绝在拒绝结果自身落盘失败时，返回实际保存错误与
// 空结果，业务错误被掩盖的问题得到修正；请求号不被占用，授权仍只是实时到期
// （没有使用标记）、授权变更记录只有创建一条，藏品持有人、版本、转让历史、
// 历史序号与版税应付保持操作前内容。保存仍被阻断时重提只再次失败在保存上；
// 保存恢复后其他操作成功保存不带入这次拒绝，此前已经保存的另一请求拒绝
// 仍正常回放；原请求重提在拒绝条件仍在时先成功保存此次拒绝再返回到期错误
// （首次不标回放、结果带授权编号与业务错误），此后完全相同提交才回放。
func TestProxyTransferExpiredRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	base := proxyExpiredWorld(t, r)

	// 预先保存另一请求的拒绝结果（a2 被撤销后 bob 代转），用于核对本次
	// 失败不影响此前已保存的其他请求结果。
	a2req := createAuthReq(r, "a2", time.Hour)
	a2req.RequestID = "create-a2"
	r.now = func() time.Time { return base }
	if _, err := r.CreateAuthorization(a2req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "改主意", RequestID: "rv-a2", AuthID: "a2",
	}); err != nil {
		t.Fatal(err)
	}
	savedReq := proxyReq("bob", "a2", "saved-revoked")
	if sr, err := r.ProxyTransfer(savedReq); !errors.Is(err, ErrAuthorizationRevoked) ||
		sr.Replayed || sr.AuthID != "a2" {
		t.Fatalf("预置的已撤销拒绝首次结果异常: %+v, %v", sr, err)
	}
	r.now = func() time.Time { return base.Add(time.Hour + time.Second) }

	req := proxyReq("bob", "a1", "p-late")
	beforePayables, err := r.PayablesOf("carol")
	if err != nil {
		t.Fatal(err)
	}

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)
	// 授权只实时到期：没有使用标记，也没有被撤销。
	a, err := r.GetAuthorization("a1")
	if err != nil || a.Status != AuthExpired || a.UsedTxSeq != 0 || a.UsedAt != (time.Time{}) {
		t.Fatalf("未保存的到期拒绝不应改变授权: %+v, %v", a, err)
	}
	// 授权变更记录仍为：create a1、create a2、revoke a2。
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 3 || evs[0].Kind != "create" {
		t.Fatalf("未保存的到期拒绝不应新增授权变更记录: %+v", evs)
	}
	// 藏品持有人、版本、转让历史与历史序号不变。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
	if r.state.NextSeq != 1 {
		t.Fatalf("未保存的拒绝不应消耗历史序号: NextSeq=%d", r.state.NextSeq)
	}
	if after, _ := r.PayablesOf("carol"); !reflect.DeepEqual(after, beforePayables) {
		t.Fatalf("版税应付应保持操作前内容: before=%+v after=%+v", beforePayables, after)
	}

	// 保存条件未恢复时再次提交：仍重新尝试保存并失败，不能把上次未保存的
	// 拒绝当成已保存拒绝回放。
	res, err = r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)

	// 保存条件恢复后，其他操作成功保存也不能把这次未保存的拒绝带入登记册。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	assertProxyRequestFree(t, r, req)
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthExpired || a.UsedTxSeq != 0 {
		t.Fatalf("其他操作保存后授权不应被改变: %+v", a)
	}
	// 此前已保存的其他请求结果不受影响，仍回放首次的已撤销拒绝。
	if sr, err := r.ProxyTransfer(savedReq); !errors.Is(err, ErrAuthorizationRevoked) ||
		!sr.Replayed || sr.AuthID != "a2" {
		t.Fatalf("已保存的其他请求结果应照常回放: %+v, %v", sr, err)
	}

	// 原请求重提：拒绝条件仍在，先成功保存此次拒绝，再返回到期错误本身；
	// 结果带授权编号与业务错误，这次重新处理不标记为回放。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) {
		t.Fatalf("重提应在保存成功后返回 ErrAuthorizationExpired: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationExpired) ||
		res.ItemID != "" || res.TxSeq != 0 {
		t.Fatalf("到期拒绝的重提结果异常: %+v", res)
	}
	assertProxyRequestSaved(t, r, req)
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthExpired || a.UsedTxSeq != 0 {
		t.Fatalf("到期拒绝不应使用授权: %+v", a)
	}
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) || !res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationExpired) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestProxyTransferExpiredSaveFailureRetriedAfterRevoke 覆盖题目给出的重新
// 判断场景：授权到期后的拒绝保存失败，随后授权人完成撤销，再用完全相同的
// 请求重提时应按重提时的状态返回 ErrAuthorizationRevoked 并首次保存（不标
// 回放）；此后原样重提才回放已撤销的结果。
func TestProxyTransferExpiredSaveFailureRetriedAfterRevoke(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	base := proxyExpiredWorld(t, r)
	req := proxyReq("bob", "a1", "p-late")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)

	// 保存恢复后授权人完成撤销（到期授权仍可撤销，记录从 expired 到
	// revoked）。
	restoreBatchSave(t, r)
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "改主意", RequestID: "rv-a1", AuthID: "a1",
	}); err != nil {
		t.Fatalf("到期授权应可撤销: %v", err)
	}
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthRevoked {
		t.Fatalf("撤销后授权状态异常: %+v", a)
	}

	// 完全相同的请求按重提时状态重新判断：返回已撤销，这一次保存成功不标
	// 回放，结果带授权编号与业务错误。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) {
		t.Fatalf("撤销后重提应返回 ErrAuthorizationRevoked，而不是回放失败时的到期拒绝: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationRevoked) {
		t.Fatalf("已撤销拒绝的首次保存结果异常: %+v", res)
	}
	assertProxyRequestSaved(t, r, req)

	// 此后原样重提才回放已撤销的结果。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) || !res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationRevoked) {
		t.Fatalf("再次提交应回放已保存的已撤销拒绝: %+v, err %v", res, err)
	}

	// 时钟回到授权存续期也不改变已保存结果：回放内容仍是首次保存的撤销拒绝。
	r.now = func() time.Time { return base }
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) || !res.Replayed {
		t.Fatalf("已保存拒绝必须按首次结果回放: %+v, err %v", res, err)
	}
}

// TestProxyTransferRevokedRejectSaveFailure 覆盖：授权已撤销的
// ErrAuthorizationRevoked 拒绝在拒绝结果自身落盘失败时，返回保存错误与空
// 结果；撤销终态与授权变更记录保持操作前内容，藏品持有不变。保存恢复后
// 重提重新保存该拒绝（首次不标回放），再次提交才回放。
func TestProxyTransferRevokedRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "改主意", RequestID: "rv-a1", AuthID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	req := proxyReq("bob", "a1", "p-revoked")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthRevoked || a.RevokedAt == (time.Time{}) {
		t.Fatalf("未保存的拒绝不应改变授权撤销状态: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 ||
		evs[1].Kind != "revoke" {
		t.Fatalf("授权变更记录应保持操作前的创建、撤销两条: %+v", evs)
	}
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) {
		t.Fatalf("重提应在保存成功后返回 ErrAuthorizationRevoked: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationRevoked) {
		t.Fatalf("已撤销拒绝的重提结果异常: %+v", res)
	}
	assertProxyRequestSaved(t, r, req)

	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) || !res.Replayed {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestProxyTransferUsedRejectSaveFailure 覆盖：授权已使用的
// ErrAuthorizationUsed 拒绝在拒绝结果自身落盘失败时，返回保存错误与空结果；
// 已使用终态、使用关联与授权变更记录保持操作前内容，藏品持有不变。保存
// 恢复后重提重新保存该拒绝，再次提交才回放。
func TestProxyTransferUsedRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupUsedAuthzWorld(t, r)
	req := proxyReq("bob", "a2", "p-used")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)
	a, _ := r.GetAuthorization("a2")
	if a.Status != AuthUsed || a.UsedTxSeq != 2 {
		t.Fatalf("未保存的拒绝不应改变已使用授权: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 3 ||
		evs[2].Kind != "use" || evs[2].AuthID != "a2" {
		t.Fatalf("授权变更记录应保持操作前的 3 条: %+v", evs)
	}
	assertHoldingUnchanged(t, r, "i1", "carol", 2, 2)

	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("重提应在保存成功后返回 ErrAuthorizationUsed: %v", err)
	}
	if res.Replayed || res.AuthID != "a2" || !errors.Is(res.Err, ErrAuthorizationUsed) {
		t.Fatalf("已使用拒绝的重提结果异常: %+v", res)
	}
	assertProxyRequestSaved(t, r, req)
	if a, _ := r.GetAuthorization("a2"); a.Status != AuthUsed || a.UsedTxSeq != 2 {
		t.Fatalf("已使用拒绝不应改变授权: %+v", a)
	}

	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestProxyTransferForbiddenRejectSaveFailure 覆盖：非受托人发起代转的
// ErrForbidden 拒绝在拒绝结果自身落盘失败时，返回保存错误与空结果；授权
// 仍可由真正的受托人使用。保存恢复后重提重新保存无权拒绝（首次不标回放），
// 再次提交才回放；受托人本人另用请求号仍可正常完成代转。
func TestProxyTransferForbiddenRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := proxyReq("dave", "a1", "p-forbidden")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthActive {
		t.Fatalf("未保存的无权拒绝不应改变授权: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
		t.Fatalf("未保存的无权拒绝不应新增授权变更记录: %+v", evs)
	}
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("重提应在保存成功后返回 ErrForbidden: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("无权拒绝的重提结果异常: %+v", res)
	}
	assertProxyRequestSaved(t, r, req)

	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrForbidden) || !res.Replayed {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}

	// 真正的受托人不受这次已保存拒绝影响，仍可正常完成代转。
	ok, err := r.ProxyTransfer(proxyReq("bob", "a1", "p-ok"))
	if err != nil || ok.Replayed || ok.AuthID != "a1" || ok.ToID != "carol" ||
		ok.Version != 2 || ok.TxSeq != 2 {
		t.Fatalf("受托人应仍可正常代转: %+v, %v", ok, err)
	}
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthUsed {
		t.Fatalf("正常代转后授权应为已使用: %+v", a)
	}
}

// TestProxyTransferInactiveRejectSaveFailure 覆盖：受托人、授权人或接收人
// 停用的 ErrAccountInactive 拒绝在拒绝结果自身落盘失败时，同样返回保存错误
// 与空结果、不占用请求号、不改变任何状态；保存恢复后重提重新保存停用拒绝
// （首次不标回放），再次提交才回放。
func TestProxyTransferInactiveRejectSaveFailure(t *testing.T) {
	cases := []struct {
		name       string
		deactivate string
	}{
		{"trustee", "bob"},
		{"granter", "alice"},
		{"receiver", "carol"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := mustCreate(t, tempDir(t))
			authzWorld(t, r)
			if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
				t.Fatal(err)
			}
			if err := r.DeactivateAccount(tc.deactivate); err != nil {
				t.Fatal(err)
			}
			req := proxyReq("bob", "a1", "p-inactive")

			blockBatchSave(t, r)
			res, err := r.ProxyTransfer(req)
			assertProxySaveFailureEmpty(t, res, err)
			assertProxyRequestFree(t, r, req)
			if a, _ := r.GetAuthorization("a1"); a.Status != AuthActive || a.UsedTxSeq != 0 {
				t.Fatalf("未保存的停用拒绝不应改变授权: %+v", a)
			}
			assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

			restoreBatchSave(t, r)
			res, err = r.ProxyTransfer(req)
			if !errors.Is(err, ErrAccountInactive) {
				t.Fatalf("重提应在保存成功后返回 ErrAccountInactive: %v", err)
			}
			if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAccountInactive) {
				t.Fatalf("停用拒绝的重提结果异常: %+v", res)
			}
			assertProxyRequestSaved(t, r, req)

			res, err = r.ProxyTransfer(req)
			if !errors.Is(err, ErrAccountInactive) || !res.Replayed {
				t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
			}
		})
	}
}

// TestProxyTransferRejectSaveFailureUnreadableDisk 覆盖：拒绝结果保存失败且
// 原数据暂时无法读取（commit 无法按磁盘重建状态）时，未保存的拒绝也不能
// 留在当前已打开的登记册中；恢复正常读写后其他操作成功保存不带入这次拒绝，
// 原请求重提才首次保存对应业务拒绝，再次提交才回放。到期与已使用两种拒绝
// 分别覆盖。
func TestProxyTransferRejectSaveFailureUnreadableDisk(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		proxyExpiredWorld(t, r)
		req := proxyReq("bob", "a1", "p-late")

		orig, err := os.ReadFile(dataFile(r.dir))
		if err != nil {
			t.Fatal(err)
		}
		blockBatchSave(t, r)
		if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
			t.Fatal(err)
		}

		res, err := r.ProxyTransfer(req)
		assertProxySaveFailureEmpty(t, res, err)
		// 状态未能重建时也必须显式撤销：请求号空闲，授权没有使用标记，
		// 授权变更记录只有创建。
		assertProxyRequestFree(t, r, req)
		if a, _ := r.GetAuthorization("a1"); a.Status != AuthExpired || a.UsedTxSeq != 0 {
			t.Fatalf("不可读磁盘失败后授权不应改变: %+v", a)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
			t.Fatalf("不可读磁盘失败后授权变更记录应只有创建: %+v", evs)
		}
		assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

		if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
			t.Fatal(err)
		}
		restoreBatchSave(t, r)
		if err := r.RegisterAccount("dave", "路人"); err != nil {
			t.Fatal(err)
		}
		assertProxyRequestFree(t, r, req)

		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationExpired) || res.Replayed ||
			!errors.Is(res.Err, ErrAuthorizationExpired) {
			t.Fatalf("恢复后重提应首次保存到期拒绝: %+v, err %v", res, err)
		}
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationExpired) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的到期拒绝: %+v, err %v", res, err)
		}
	})

	t.Run("used", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupUsedAuthzWorld(t, r)
		req := proxyReq("bob", "a2", "p-used")

		orig, err := os.ReadFile(dataFile(r.dir))
		if err != nil {
			t.Fatal(err)
		}
		blockBatchSave(t, r)
		if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
			t.Fatal(err)
		}

		res, err := r.ProxyTransfer(req)
		assertProxySaveFailureEmpty(t, res, err)
		assertProxyRequestFree(t, r, req)
		if a, _ := r.GetAuthorization("a2"); a.Status != AuthUsed || a.UsedTxSeq != 2 {
			t.Fatalf("不可读磁盘失败后已使用授权不应改变: %+v", a)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 3 {
			t.Fatalf("不可读磁盘失败后授权变更记录应保持 3 条: %+v", evs)
		}
		assertHoldingUnchanged(t, r, "i1", "carol", 2, 2)

		if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
			t.Fatal(err)
		}
		restoreBatchSave(t, r)
		if err := r.RegisterAccount("dave", "路人"); err != nil {
			t.Fatal(err)
		}
		assertProxyRequestFree(t, r, req)

		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationUsed) || res.Replayed ||
			!errors.Is(res.Err, ErrAuthorizationUsed) {
			t.Fatalf("恢复后重提应首次保存已使用拒绝: %+v, err %v", res, err)
		}
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的已使用拒绝: %+v, err %v", res, err)
		}
	})
}

// TestProxyTransferFailedSaveRequestIDReusable 覆盖：保存失败的拒绝不占用
// 请求号——该受托人用同一请求号对另一份仍可使用的授权发起代转时不能遭遇
// 请求号冲突，代转正常成功且不标回放；只有实际保存过的结果才继续适用原有
// 的同号不同参数冲突规则，未保存的拒绝不影响另一件藏品的持有与授权状态。
func TestProxyTransferFailedSaveRequestIDReusable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	// 再发行一件 alice 持有的 i2，并在两件藏品上各建一份授权。
	if _, err := r.Issue(issueReq("i2", "alice")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	a2req := createAuthReq(r, "a2", time.Hour)
	a2req.RequestID = "create-a2"
	a2req.ItemID = "i2"
	if _, err := r.CreateAuthorization(a2req); err != nil {
		t.Fatal(err)
	}
	// a2 被撤销：bob 对 a2 的代转将得到 ErrAuthorizationRevoked。
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "改主意", RequestID: "rv-a2", AuthID: "a2",
	}); err != nil {
		t.Fatal(err)
	}
	failedReq := proxyReq("bob", "a2", "p-x")
	otherReq := proxyReq("bob", "a1", "p-x")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(failedReq)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, failedReq)

	// 保存条件未恢复时重放原失败请求：仍只失败在保存上。
	res, err = r.ProxyTransfer(failedReq)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, failedReq)

	// 保存恢复后，同一操作者用同一请求号对另一份授权代转：未保存的拒绝
	// 不适用冲突规则，代转正常成功且不标回放。
	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(otherReq)
	if err != nil {
		t.Fatalf("未保存的拒绝不应阻碍同号使用另一份授权: %v", err)
	}
	if res.Replayed || res.Err != nil || res.AuthID != "a1" || res.ItemID != "i1" ||
		res.FromID != "alice" || res.ToID != "carol" || res.Version != 2 || res.TxSeq != 3 {
		t.Fatalf("同号代转另一份授权的结果异常: %+v", res)
	}
	if a1, _ := r.GetAuthorization("a1"); a1.Status != AuthUsed {
		t.Fatalf("a1 应已使用: %+v", a1)
	}
	// i2 的持有与 a2 的撤销状态不受影响。
	assertHoldingUnchanged(t, r, "i2", "alice", 1, 1)
	if a2, _ := r.GetAuthorization("a2"); a2.Status != AuthRevoked {
		t.Fatalf("a2 应仍为已撤销: %+v", a2)
	}

	// 请求号此刻已被实际保存的 a1 成功结果占用：同号不同参数（改用 a2）
	// 按原有规则返回 ErrRequestConflict，而不是回放或重新判断。
	if _, err := r.ProxyTransfer(failedReq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("实际保存过的结果才应触发同号不同参数冲突: %v", err)
	}
	// 完全相同的已保存请求（用 a1）回放首次成功结果。
	res, err = r.ProxyTransfer(otherReq)
	if err != nil || !res.Replayed || res.AuthID != "a1" || res.TxSeq != 3 {
		t.Fatalf("已保存的同号成功结果应被回放: %+v, err %v", res, err)
	}
}

// TestProxyTransferRejectSaveFailureAfterReopen 覆盖磁盘视角：原数据可读时
// 拒绝结果保存失败后关闭重开，看到的仍是拒绝前状态——授权没有使用标记、
// 请求号未占用、授权变更记录与藏品持有不变；恢复保存后用原请求重提才首次
// 保存该拒绝，再次提交回放。
func TestProxyTransferRejectSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	base := proxyExpiredWorld(t, r)
	req := proxyReq("bob", "a1", "p-late")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	// 重开后时钟默认回到真实时间，恢复为用例的固定时钟（到期点之后）。
	r2.now = func() time.Time { return base.Add(time.Hour + time.Second) }
	assertProxyRequestFree(t, r2, req)
	if a, _ := r2.GetAuthorization("a1"); a.Status != AuthExpired || a.UsedTxSeq != 0 {
		t.Fatalf("重开后授权不应被改变: %+v", a)
	}
	if evs, _ := r2.AuthorizationHistory("i1"); len(evs) != 1 {
		t.Fatalf("重开后授权变更记录应只有创建: %+v", evs)
	}
	assertHoldingUnchanged(t, r2, "i1", "alice", 1, 1)

	restoreBatchSave(t, r2)
	res2, err := r2.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) || res2.Replayed ||
		!errors.Is(res2.Err, ErrAuthorizationExpired) || res2.AuthID != "a1" {
		t.Fatalf("重开后重提应首次保存到期拒绝: %+v, err %v", res2, err)
	}
	res2, err = r2.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) || !res2.Replayed {
		t.Fatalf("再次提交应回放已保存的到期拒绝: %+v, err %v", res2, err)
	}
}

// TestProxyTransferValidationIgnoresSaveFailure 覆盖：必填内容缺失与授权不
// 存在的请求直接按原错误拒绝、不占用请求号、不要求保存，即使数据位置暂时
// 不可写也仍返回原参数/引用错误，不能改报保存错误。
func TestProxyTransferValidationIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 必填内容缺失：返回 ErrInvalidArgument，不是保存错误。
	if _, err := r.ProxyTransfer(ProxyTransferRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	missing := proxyReq("bob", "a1", "p-miss")
	missing.AuthID = ""
	if _, err := r.ProxyTransfer(missing); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument: %v", err)
	}

	// 授权不存在：返回 ErrNotFound（结果带授权编号与业务错误），不是保存
	// 错误，也不占用请求号。
	notFoundReq := proxyReq("bob", "ghost", "p-nf")
	res, err := r.ProxyTransfer(notFoundReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("授权不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.AuthID != "ghost" || !errors.Is(res.Err, ErrNotFound) || res.Replayed {
		t.Fatalf("授权不存在的结果异常: %+v", res)
	}
	assertProxyRequestFree(t, r, notFoundReq)

	// 保存条件恢复后，同一操作者用同一请求号对真实存在的授权发起代转应
	// 正常成功：引用不存在没有占用请求号，成功代转行为保持不变。
	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(proxyReq("bob", "a1", "p-nf"))
	if err != nil || res.Replayed || res.Err != nil || res.AuthID != "a1" ||
		res.ItemID != "i1" || res.ToID != "carol" || res.Version != 2 || res.TxSeq != 2 {
		t.Fatalf("引用不存在不应占用请求号，同号代转真实授权应成功: %+v, err %v", res, err)
	}
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthUsed || a.UsedTxSeq != 2 {
		t.Fatalf("成功代转应一次性使用授权: %+v", a)
	}
	assertHoldingUnchanged(t, r, "i1", "carol", 2, 2)
}
