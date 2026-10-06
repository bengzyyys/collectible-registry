package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为限时一次性代转（ProxyTransfer）补充"拒绝结果保存失败"场景的
// 回归保障，与发行、转让、授权创建/撤销的同类用例行为对齐：授权已到期、
// 已撤销、已使用、操作者无权、任一相关账户停用或持有版本已变化等状态类
// 业务拒绝，原本要作为该请求的首次结果落盘。若拒绝结果尚未写入原登记册
// 就发生写入失败，ProxyTransfer 必须返回实际保存错误，不能只返回原业务
// 拒绝，让调用者误以为这次拒绝已经记住。
//
// 保存失败时返回的结果必须为空：不带授权或藏品编号，不带转让与金额信息，
// Replayed 为 false，结果内的 Err 为空，单独返回的 error 保留保存失败的
// 原因。授权内容、使用标记、授权变更记录，以及藏品持有人、版本、转让历史
// 与版税应付都保持调用前状态；该操作者的本次请求号没有被占用——即使写入
// 失败后原登记册数据暂时无法读取，也不能让尚未保存的拒绝留在当前已打开的
// 登记册中，或随之后另一次正常操作被保存下来。保存条件恢复后用完全相同的
// 请求重提，按重提时的业务状态重新判断（例如到期拒绝保存失败、授权随后被
// 撤销时，重提返回已撤销），首次重新处理不标回放，保存成功后再次原样重提
// 才回放。已经成功保存的拒绝仍按首次结果回放，改动该请求的业务内容仍报
// 请求号冲突；必填内容缺失或引用不存在沿用原错误且不占用请求号。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// assertProxySaveFailureEmpty 核对代转"拒绝结果保存失败"的返回：error 是
// 保存错误而非任何业务拒绝（含授权类的已撤销、已到期、已使用与账户停用
// 哨兵）；结果为空——不携带授权或藏品编号、前后持有人、版本、历史序号或
// 金额，业务错误为空，也不标记为重复返回。
func assertProxySaveFailureEmpty(t *testing.T, res ProxyTransferResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	for _, s := range []error{
		ErrAuthorizationRevoked, ErrAuthorizationExpired, ErrAuthorizationUsed,
		ErrAccountInactive,
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

// assertProxyRequestSavedRejected 核对拒绝结果已按指定业务原因登记落盘。
func assertProxyRequestSavedRejected(t *testing.T, r *Registry, req ProxyTransferRequest, wantErr error) {
	t.Helper()
	prev, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]
	if !ok {
		t.Fatalf("拒绝保存成功后应登记请求结果，请求号 %s 却空闲", req.RequestID)
	}
	if !prev.Rejected || prev.Kind != "proxy_transfer" || prev.AuthID != req.AuthID {
		t.Fatalf("登记的请求结果异常: %+v", prev)
	}
	if wantErr != nil && !errors.Is(codeErr(prev.Reason), wantErr) {
		t.Fatalf("登记的拒绝原因为 %s，应对应 %v", prev.Reason, wantErr)
	}
}

// assertNoPayables 核对版税应付保持调用前内容（无新增应付）。
func assertNoPayables(t *testing.T, r *Registry) {
	t.Helper()
	for _, acct := range []string{"alice", "bob", "carol"} {
		got, err := r.PayablesOf(acct)
		if err != nil {
			t.Fatalf("PayablesOf %s: %v", acct, err)
		}
		if len(got) != 0 {
			t.Fatalf("未保存的拒绝不应新增版税应付，%s 却有 %+v", acct, got)
		}
	}
}

// TestProxyTransferExpiredRejectSaveFailureThenRevoke 覆盖题目主路径：授权
// 到期后的代转拒绝在拒绝结果自身落盘失败时，返回实际保存错误与空结果，
// 请求号不被占用，授权内容、使用标记、授权变更记录、藏品持有、历史与版税
// 应付保持操作前状态（含"保存仍失败时再次提交"与"原数据暂时无法读取"两种
// 情形）。保存恢复后先有一次其他操作成功保存，也不能把这次未保存的拒绝带
// 入登记册；随后授权人撤销授权，原样重提必须按重提时状态返回已撤销（不标
// 回放、首次保存），此后原样重提才回放已撤销的拒绝。
func TestProxyTransferExpiredRejectSaveFailureThenRevoke(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	createReq := createAuthReq(r, "a1", time.Hour) // 到期点 base+1h
	if _, err := r.CreateAuthorization(createReq); err != nil {
		t.Fatal(err)
	}
	// 越过到期点：代转应被到期拒绝。
	r.now = func() time.Time { return base.Add(2 * time.Hour) }
	req := proxyReq("bob", "a1", "p-exp")

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)

	// 拒绝结果落盘失败：返回保存错误与空结果。
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthExpired || a.UsedTxSeq != 0 || a.UsedAt != (time.Time{}) {
		t.Fatalf("未保存的到期拒绝不应改变授权: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("未保存的到期拒绝不应新增授权变更记录: %+v", evs)
	}
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
	assertNoPayables(t, r)

	// 保存条件未恢复时再次提交同一请求：仍重新尝试保存并失败，不能把上次
	// 未保存的拒绝当成已保存拒绝回放。
	res, err = r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)

	// 同时让原数据暂时无法读取（commit 无法按磁盘重建状态）：未保存的拒绝
	// 仍不能留在当前已打开的登记册中。
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	res, err = r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, req)
	a, _ = r.GetAuthorization("a1")
	if a.Status != AuthExpired || a.UsedTxSeq != 0 {
		t.Fatalf("不可读磁盘失败后授权不应改变: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
		t.Fatalf("不可读磁盘失败后授权变更记录应只有创建: %+v", evs)
	}
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 恢复正常读写；其他操作成功保存不能把这次未保存的拒绝带入登记册。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	assertProxyRequestFree(t, r, req)
	a, _ = r.GetAuthorization("a1")
	if a.Status != AuthExpired {
		t.Fatalf("其他操作保存后授权应仍只是到期、未终结: %+v", a)
	}

	// 授权人完成撤销：业务状态从到期变为已撤销。
	rv, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "到期后撤销", RequestID: "rv-1", AuthID: "a1",
	})
	if err != nil || rv.Status != AuthRevoked {
		t.Fatalf("授权人应能撤销到期未使用的授权: %+v, %v", rv, err)
	}

	// 完全相同的代转请求按重提时状态重新判断：返回已撤销，首次重新处理不标
	// 回放，结果带授权编号与业务错误，这次拒绝才首次落盘。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) {
		t.Fatalf("重提应按当前状态返回 ErrAuthorizationRevoked: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationRevoked) ||
		res.ItemID != "" || res.TxSeq != 0 {
		t.Fatalf("已撤销拒绝的重提结果异常: %+v", res)
	}
	assertProxyRequestSavedRejected(t, r, req, ErrAuthorizationRevoked)
	// 拒绝仍不改变持有、授权使用标记与历史。
	a, _ = r.GetAuthorization("a1")
	if a.Status != AuthRevoked || a.UsedTxSeq != 0 {
		t.Fatalf("已撤销拒绝不应把授权用掉: %+v", a)
	}
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 此后原样重提才回放已保存的已撤销拒绝。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) || !res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationRevoked) {
		t.Fatalf("保存后的重提应回放已撤销拒绝: %+v, err %v", res, err)
	}
}

// TestProxyTransferStateRejectSaveFailureVariants 逐类覆盖无权、已使用、
// 账户停用与持有版本已变化四种状态类拒绝：保存失败时返回保存错误与空结果，
// 请求号不被占用，授权、授权变更记录、藏品持有、历史与版税应付保持操作前
// 状态；保存恢复且拒绝条件仍在时，原请求重提先成功保存此次拒绝再返回对应
// 业务错误（首次不标回放），此后完全相同的提交才回放它。
func TestProxyTransferStateRejectSaveFailureVariants(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		// 操作者 alice 不是受托人 bob：ErrForbidden。
		req := proxyReq("alice", "a1", "p-forbidden")

		blockBatchSave(t, r)
		res, err := r.ProxyTransfer(req)
		assertProxySaveFailureEmpty(t, res, err)
		assertProxyRequestFree(t, r, req)
		if a, _ := r.GetAuthorization("a1"); a.Status != AuthActive || a.UsedTxSeq != 0 {
			t.Fatalf("未保存的无权拒绝不应改变授权: %+v", a)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
			t.Fatalf("未保存的无权拒绝不应新增授权变更记录: %+v", evs)
		}
		assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

		restoreBatchSave(t, r)
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrForbidden) || res.Replayed || res.AuthID != "a1" ||
			!errors.Is(res.Err, ErrForbidden) {
			t.Fatalf("重提应在保存成功后返回 ErrForbidden: %+v, err %v", res, err)
		}
		assertProxyRequestSavedRejected(t, r, req, ErrForbidden)
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrForbidden) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的无权拒绝: %+v, err %v", res, err)
		}
	})

	t.Run("used", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupUsedAuthzWorld(t, r)
		// a2 已由 bob 代转给 carol：i1 在 carol 手中版本 2，a2 已使用。
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
		if !errors.Is(err, ErrAuthorizationUsed) || res.Replayed || res.AuthID != "a2" ||
			!errors.Is(res.Err, ErrAuthorizationUsed) {
			t.Fatalf("重提应在保存成功后返回 ErrAuthorizationUsed: %+v, err %v", res, err)
		}
		assertProxyRequestSavedRejected(t, r, req, ErrAuthorizationUsed)
		a, _ = r.GetAuthorization("a2")
		if a.Status != AuthUsed || a.UsedTxSeq != 2 {
			t.Fatalf("已使用拒绝不应改变授权: %+v", a)
		}
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的已使用拒绝: %+v, err %v", res, err)
		}
	})

	t.Run("inactive", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		// 接收人停用：ErrAccountInactive。
		if err := r.DeactivateAccount("carol"); err != nil {
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
		if !errors.Is(err, ErrAccountInactive) || res.Replayed || res.AuthID != "a1" ||
			!errors.Is(res.Err, ErrAccountInactive) {
			t.Fatalf("重提应在保存成功后返回 ErrAccountInactive: %+v, err %v", res, err)
		}
		assertProxyRequestSavedRejected(t, r, req, ErrAccountInactive)
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAccountInactive) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的停用拒绝: %+v, err %v", res, err)
		}
	})

	t.Run("conflict", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		// 持有人另作直接转让：i1 变为 bob 版本 2，授权绑定的 alice 版本 1
		// 已不匹配：ErrConflict（藏品即使之后回到 alice 手中也不恢复）。
		if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "direct-1")); err != nil {
			t.Fatal(err)
		}
		req := proxyReq("bob", "a1", "p-conflict")

		blockBatchSave(t, r)
		res, err := r.ProxyTransfer(req)
		assertProxySaveFailureEmpty(t, res, err)
		assertProxyRequestFree(t, r, req)
		if a, _ := r.GetAuthorization("a1"); a.Status != AuthActive || a.UsedTxSeq != 0 {
			t.Fatalf("未保存的版本冲突拒绝不应改变授权: %+v", a)
		}
		assertHoldingUnchanged(t, r, "i1", "bob", 2, 2)

		restoreBatchSave(t, r)
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrConflict) || res.Replayed || res.AuthID != "a1" ||
			!errors.Is(res.Err, ErrConflict) {
			t.Fatalf("重提应在保存成功后返回 ErrConflict: %+v, err %v", res, err)
		}
		assertProxyRequestSavedRejected(t, r, req, ErrConflict)
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrConflict) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的版本冲突拒绝: %+v, err %v", res, err)
		}
	})
}

// TestProxyTransferRejectSaveFailureUnreadableDisk 覆盖：拒绝结果保存失败且
// 原数据暂时无法读取（commit 无法按磁盘重建状态）时，未保存的拒绝也不能
// 留在当前已打开的登记册中；恢复正常读写后其他操作成功保存不带入这次拒绝，
// 原请求重提才首次保存对应业务拒绝，再次提交才回放。已使用与无权两种拒绝
// 分别覆盖。
func TestProxyTransferRejectSaveFailureUnreadableDisk(t *testing.T) {
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
		a, _ := r.GetAuthorization("a2")
		if a.Status != AuthUsed || a.UsedTxSeq != 2 {
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
			!errors.Is(res.Err, ErrAuthorizationUsed) || res.AuthID != "a2" {
			t.Fatalf("恢复后重提应首次保存已使用拒绝: %+v, err %v", res, err)
		}
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的已使用拒绝: %+v, err %v", res, err)
		}
	})

	t.Run("forbidden", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		req := proxyReq("alice", "a1", "p-forbidden")

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
		if a, _ := r.GetAuthorization("a1"); a.Status != AuthActive {
			t.Fatalf("不可读磁盘失败后授权不应改变: %+v", a)
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
		if !errors.Is(err, ErrForbidden) || res.Replayed ||
			!errors.Is(res.Err, ErrForbidden) {
			t.Fatalf("恢复后重提应首次保存无权拒绝: %+v, err %v", res, err)
		}
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrForbidden) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的无权拒绝: %+v, err %v", res, err)
		}
	})
}

// TestProxyTransferFailedSaveRequestIDReusable 覆盖：保存失败的拒绝不占用
// 请求号——同一受托人用同一请求号对另一份仍可用的授权发起代转时不能遭遇
// 请求号冲突，应正常成功且不标回放；只有实际保存过的结果才继续适用原有的
// 同号不同参数冲突规则。
func TestProxyTransferFailedSaveRequestIDReusable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	// a1 绑定 i1；另发行 i2 并创建 a2，先把 a2 用掉（i2 转到 carol 手中，
	// 版本 2），a1 与 i1 不受影响、仍可使用。
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(issueReq("i2", "alice")); err != nil {
		t.Fatal(err)
	}
	a2req := CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托代转", RequestID: "create-a2",
		AuthID: "a2", ItemID: "i2", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 1, ExpiresAt: r.now().Add(time.Hour),
	}
	if _, err := r.CreateAuthorization(a2req); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a2", "p-a2")); err != nil {
		t.Fatal(err)
	}

	// bob 对已使用的 a2 代转将被拒绝；同一请求号稍后改用于仍有效的 a1。
	failedReq := proxyReq("bob", "a2", "p-x")
	otherReq := proxyReq("bob", "a1", "p-x")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(failedReq)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, failedReq)
	// i1 仍属 alice 版本 1，a1 仍可用。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 保存条件未恢复时重放原失败请求：仍只失败在保存上。
	res, err = r.ProxyTransfer(failedReq)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxyRequestFree(t, r, failedReq)

	// 保存恢复后，同一受托人用同一请求号代转另一份仍有效的授权：未保存的
	// 拒绝不适用冲突规则，请求正常成功且不标回放。
	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(otherReq)
	if err != nil {
		t.Fatalf("未保存的拒绝不应阻碍同号代转另一份有效授权: %v", err)
	}
	if res.Replayed || res.Err != nil || res.AuthID != "a1" || res.ItemID != "i1" ||
		res.FromID != "alice" || res.ToID != "carol" || res.Version != 2 {
		t.Fatalf("同号代转另一份授权的结果异常: %+v", res)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("a1 代转应正常完成: %+v", h)
	}
	a1, _ := r.GetAuthorization("a1")
	if a1.Status != AuthUsed || a1.UsedTxSeq != res.TxSeq {
		t.Fatalf("a1 应记为本次代转已使用: %+v", a1)
	}

	// 请求号此刻已被实际保存的 a1 成功结果占用：同号不同参数（改用 a2）按
	// 原有规则返回 ErrRequestConflict。
	if _, err := r.ProxyTransfer(failedReq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("实际保存过的结果才应触发同号不同参数冲突: %v", err)
	}
	// 完全相同的已保存请求（代转 a1）回放首次成功结果。
	res, err = r.ProxyTransfer(otherReq)
	if err != nil || !res.Replayed || res.AuthID != "a1" || res.ToID != "carol" {
		t.Fatalf("已保存的同号成功结果应被回放: %+v, err %v", res, err)
	}
}

// TestProxyTransferRejectSaveFailureAfterReopen 覆盖磁盘视角：原数据可读时
// 拒绝结果保存失败后关闭重开，看到的仍是拒绝前状态——授权未改变、请求号
// 未占用；恢复保存后用原请求重提才首次保存该拒绝，再次提交回放。
func TestProxyTransferRejectSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// alice 不是受托人：ErrForbidden。
	req := proxyReq("alice", "a1", "pf")

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
	if _, ok := r2.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("重开后未保存的拒绝不应占用请求号")
	}
	if a, _ := r2.GetAuthorization("a1"); a.Status != AuthActive || a.UsedTxSeq != 0 {
		t.Fatalf("重开后授权应仍有效且未使用: %+v", a)
	}
	if evs, _ := r2.AuthorizationHistory("i1"); len(evs) != 1 {
		t.Fatalf("重开后授权变更记录应只有创建: %+v", evs)
	}
	assertHoldingUnchanged(t, r2, "i1", "alice", 1, 1)

	restoreBatchSave(t, r2)
	res2, err := r2.ProxyTransfer(req)
	if !errors.Is(err, ErrForbidden) || res2.Replayed ||
		!errors.Is(res2.Err, ErrForbidden) || res2.AuthID != "a1" {
		t.Fatalf("重开后重提应首次保存无权拒绝: %+v, err %v", res2, err)
	}
	res2, err = r2.ProxyTransfer(req)
	if !errors.Is(err, ErrForbidden) || !res2.Replayed {
		t.Fatalf("再次提交应回放已保存的无权拒绝: %+v, err %v", res2, err)
	}
}

// TestProxyTransferValidationIgnoresSaveFailure 覆盖：必填内容缺失与引用
// 不存在（授权不存在、受托人未登记）的请求直接按原错误拒绝、不占用请求号、
// 不要求保存，即使数据位置暂时不可写也仍返回原参数/引用错误，不能改报保存
// 错误。
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

	// 受托人未登记：同样是引用不存在，返回 ErrNotFound 且不占用请求号。
	noOperatorReq := proxyReq("dave", "a1", "p-nop")
	res, err = r.ProxyTransfer(noOperatorReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("受托人未登记应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.AuthID != "a1" || !errors.Is(res.Err, ErrNotFound) || res.Replayed {
		t.Fatalf("受托人未登记的结果异常: %+v", res)
	}
	assertProxyRequestFree(t, r, noOperatorReq)

	// 保存条件恢复后，同一受托人用同一请求号对真实授权发起代转应正常成功：
	// 引用不存在没有占用请求号。
	restoreBatchSave(t, r)
	sameIDRealAuth := proxyReq("bob", "a1", "p-nf")
	res, err = r.ProxyTransfer(sameIDRealAuth)
	if err != nil || res.Replayed || res.Err != nil || res.AuthID != "a1" ||
		res.ToID != "carol" || res.Version != 2 {
		t.Fatalf("引用不存在不应占用请求号，同号代转真实授权应成功: %+v, err %v", res, err)
	}
}

// TestProxyTransferSavedRejectionReplaysAndConflicts 覆盖保存成功后的既有
// 幂等语义不变：到期拒绝保存后，完全相同的请求回放首次拒绝（即使授权随后
// 被撤销也仍回放到期，而非改判已撤销），改动原因则按请求号冲突拒绝。
func TestProxyTransferSavedRejectionReplaysAndConflicts(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	createReq := createAuthReq(r, "a1", time.Minute)
	if _, err := r.CreateAuthorization(createReq); err != nil {
		t.Fatal(err)
	}
	r.now = func() time.Time { return createReq.ExpiresAt.Add(time.Second) }
	req := proxyReq("bob", "a1", "late")

	// 首次：到期拒绝并成功保存。
	res, err := r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) || res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationExpired) {
		t.Fatalf("首次代转应返回未回放的到期拒绝: %+v, err %v", res, err)
	}
	assertProxyRequestSavedRejected(t, r, req, ErrAuthorizationExpired)

	// 完全相同的请求重提：回放首次到期拒绝。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) || !res.Replayed ||
		!errors.Is(res.Err, ErrAuthorizationExpired) {
		t.Fatalf("相同请求应回放首次到期拒绝: %+v, err %v", res, err)
	}

	// 改动该请求的业务内容（原因）：请求号冲突。
	changed := req
	changed.Reason = "换个原因"
	if _, err := r.ProxyTransfer(changed); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("改动业务内容应返回 ErrRequestConflict: %v", err)
	}

	// 授权随后被撤销：原请求仍回放保存过的到期拒绝，而不是改判已撤销。
	r.now = func() time.Time { return createReq.ExpiresAt.Add(2 * time.Second) }
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "撤销", RequestID: "rv", AuthID: "a1",
	}); err != nil {
		t.Fatal(err)
	}
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) || !res.Replayed {
		t.Fatalf("授权撤销后原请求仍应回放到期拒绝: %+v, err %v", res, err)
	}
}
