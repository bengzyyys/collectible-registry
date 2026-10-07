package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为撤销代转授权补充"撤销成功保存失败"场景的回归保障。撤销以本次
// 结果成功保存为准：撤销请求符合现有业务条件，但新结果尚未替换原登记册就
// 发生保存错误时，RevokeAuthorization 必须返回实际保存错误与空结果（不携带
// 授权编号或状态、不标记回放、结果内业务错误为空），即使原数据暂时读不到，
// 也必须在当前登记册中保留撤销前的授权内容和撤销时间：不增加撤销历史，不
// 占用请求号，也不消耗授权历史序号；藏品持有人、版本以及此前已保存的其他
// 授权和历史保持原样。保存恢复后用同一操作者、授权编号、原因和请求号再次
// 撤销，按此时的授权处理，不能回放那次未保存的成功。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// assertAuthzStillActive 核对授权保持撤销前内容：状态按当前时间计算为
// active、撤销时间为零值、未使用。
func assertAuthzStillActive(t *testing.T, r *Registry, authID string) {
	t.Helper()
	a, err := r.GetAuthorization(authID)
	if err != nil {
		t.Fatalf("GetAuthorization %s: %v", authID, err)
	}
	if a.Status != AuthActive || a.RevokedAt != (time.Time{}) || a.UsedTxSeq != 0 {
		t.Fatalf("未保存的撤销不应改变授权，应仍有效且无撤销时间: %+v", a)
	}
}

// assertAuthEventsOnlyCreate 核对授权变更记录只有创建一条，且授权历史序号
// 停留在创建序号（撤销没有消耗序号）。
func assertAuthEventsOnlyCreate(t *testing.T, r *Registry, itemID string) {
	t.Helper()
	evs, err := r.AuthorizationHistory(itemID)
	if err != nil {
		t.Fatalf("AuthorizationHistory %s: %v", itemID, err)
	}
	if len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("未保存的撤销不应新增授权变更记录: %+v", evs)
	}
	if r.state.NextAuthSeq != 1 {
		t.Fatalf("未保存的撤销不应消耗授权历史序号，NextAuthSeq=%d", r.state.NextAuthSeq)
	}
}

// TestRevokeAuthzSuccessSaveFailureKeepsAuthUsable 覆盖题目主路径：授权人
// 撤销一份尚未使用的授权，撤销结果落盘失败时返回实际保存错误与空结果；原
// 数据可读与暂时不可读两种情形下，当前已打开的登记册都保留撤销前的授权——
// 受托人仍可凭该授权正常完成代转，撤销不新增历史、不占用请求号与历史序号。
// 代转使用后用原撤销请求重提，按此时状态返回已使用拒绝并首次保存，而不是
// 回放那次未保存的撤销成功；再原样提交才回放该拒绝。
func TestRevokeAuthzSuccessSaveFailureKeepsAuthUsable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("alice", "a1", "rv-fail")

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)

	// 撤销成功路径落盘失败：返回保存错误与空结果。
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)
	assertAuthzStillActive(t, r, "a1")
	assertAuthEventsOnlyCreate(t, r, "i1")
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 保存条件未恢复时再次提交同一请求：仍实际尝试保存并失败，不能把上次
	// 未保存的撤销当成已保存成功回放。
	res, err = r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)
	assertAuthzStillActive(t, r, "a1")

	// 同时让原数据暂时无法读取（commit 无法按磁盘重建状态）：未保存的撤销
	// 仍不能留在当前已打开的登记册中。
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	res, err = r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)
	assertAuthzStillActive(t, r, "a1")
	assertAuthEventsOnlyCreate(t, r, "i1")
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 恢复正常读写；先完成另一项正常登记操作，不能把失败撤销的状态、历史或
	// 请求结果一并保存下来。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	assertRevokeRequestFree(t, r, req)
	assertAuthzStillActive(t, r, "a1")
	assertAuthEventsOnlyCreate(t, r, "i1")

	// 撤销失败不提前阻止受托人：持有版本未变、账户可用且未到期，bob 仍能凭
	// a1 正常代转给 carol。
	pres, perr := r.ProxyTransfer(proxyReq("bob", "a1", "p-after-fail"))
	if perr != nil {
		t.Fatalf("撤销未保存时受托人应仍可使用授权: %v", perr)
	}
	if pres.Replayed || pres.AuthID != "a1" || pres.ToID != "carol" ||
		pres.FromID != "alice" || pres.Version != 2 {
		t.Fatalf("失败撤销后的代转结果异常: %+v", pres)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("代转后持有应为 carol 版本 2: %+v", h)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthUsed || a.UsedTxSeq != pres.TxSeq {
		t.Fatalf("a1 应记为本次代转已使用: %+v", a)
	}
	// 授权变更记录依次为 create、use，从未出现撤销记录；撤销没有消耗序号。
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 2 || evs[0].Kind != "create" || evs[1].Kind != "use" ||
		evs[1].AuthID != "a1" || r.state.NextAuthSeq != 2 {
		t.Fatalf("撤销失败不应留下撤销记录或消耗序号: %+v", evs)
	}

	// 原撤销请求重提：授权已被使用，按此时状态返回 ErrAuthorizationUsed，
	// 首次保存这一拒绝（不标回放、结果带授权编号与业务错误、状态为空）。
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("代转使用后重提应返回 ErrAuthorizationUsed: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationUsed) ||
		res.Status != "" {
		t.Fatalf("已使用拒绝的重提结果异常: %+v", res)
	}
	assertRevokeRequestSaved(t, r, req)

	// 再原样提交才回放这个已保存的拒绝。
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationUsed) {
		t.Fatalf("再次提交应回放已保存的已使用拒绝: %+v, err %v", res, err)
	}
}

// TestRevokeAuthzSuccessSaveFailureRetryCompletes 覆盖：保存恢复后用同一操作
// 者、授权编号、原因和请求号再次撤销，授权仍未使用且允许撤销时本次才完成
// 撤销并新增一条记录（记录本次生效的时间、操作者、原因与前后状态，不标回
// 放），再原样提交才回放这一已保存结果；回放不重复新增记录。
func TestRevokeAuthzSuccessSaveFailureRetryCompletes(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("alice", "a1", "rv-retry")

	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)
	assertAuthzStillActive(t, r, "a1")

	// 保存恢复，时间推进到本次生效时刻：重提按当前状态完整执行一次撤销。
	restoreBatchSave(t, r)
	effectiveAt := base.Add(time.Minute)
	r.now = func() time.Time { return effectiveAt }
	res, err = r.RevokeAuthorization(req)
	if err != nil || res.AuthID != "a1" || res.Status != AuthRevoked || res.Replayed {
		t.Fatalf("恢复后重提应本次完成撤销而非回放: %+v, %v", res, err)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthRevoked || !a.RevokedAt.Equal(effectiveAt) {
		t.Fatalf("撤销应在本次生效时间落盘: %+v", a)
	}
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 2 || evs[1].Kind != "revoke" ||
		evs[1].AuthID != "a1" || evs[1].Operator != "alice" ||
		evs[1].Reason != req.Reason || evs[1].RequestID != req.RequestID ||
		evs[1].FromStatus != AuthActive || evs[1].ToStatus != AuthRevoked ||
		!evs[1].OccurredAt.Equal(effectiveAt) {
		t.Fatalf("本次撤销应新增一条完整记录: %+v", evs)
	}
	if r.state.NextAuthSeq != 2 {
		t.Fatalf("本次撤销才消耗授权历史序号，NextAuthSeq=%d", r.state.NextAuthSeq)
	}

	// 再原样提交：回放已保存的撤销成功，不新增记录、不改变撤销时间。
	res, err = r.RevokeAuthorization(req)
	if err != nil || !res.Replayed || res.AuthID != "a1" || res.Status != AuthRevoked {
		t.Fatalf("再次提交应回放已保存的撤销结果: %+v, %v", res, err)
	}
	if evs2, _ := r.AuthorizationHistory("i1"); len(evs2) != 2 {
		t.Fatalf("回放不应新增撤销记录: %+v", evs2)
	}
	// 受托人此后被按已撤销拒绝。
	if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p-late")); !errors.Is(err, ErrAuthorizationRevoked) {
		t.Fatalf("撤销保存后代转应被拒绝: %v", err)
	}
}

// TestRevokeAuthzSuccessSaveFailureAfterReopen 覆盖磁盘视角：原数据可读时
// 撤销保存失败后关闭重开，看到的仍是撤销前状态——授权仍有效、撤销时间为空、
// 只有创建记录、请求号未占用；恢复保存后用原请求重提才本次完成撤销，再次
// 提交回放。
func TestRevokeAuthzSuccessSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("alice", "a1", "rv-reopen")

	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	if _, ok := r2.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("重开后未保存的撤销不应占用请求号")
	}
	if a, _ := r2.GetAuthorization("a1"); a.Status != AuthActive ||
		a.RevokedAt != (time.Time{}) {
		t.Fatalf("重开后授权应仍有效且无撤销时间: %+v", a)
	}
	if evs, _ := r2.AuthorizationHistory("i1"); len(evs) != 1 ||
		evs[0].Kind != "create" {
		t.Fatalf("重开后授权变更记录应只有创建: %+v", evs)
	}
	assertHoldingUnchanged(t, r2, "i1", "alice", 1, 1)

	restoreBatchSave(t, r2)
	res2, err := r2.RevokeAuthorization(req)
	if err != nil || res2.Replayed || res2.AuthID != "a1" ||
		res2.Status != AuthRevoked {
		t.Fatalf("重开后重提应本次完成撤销: %+v, %v", res2, err)
	}
	res2, err = r2.RevokeAuthorization(req)
	if err != nil || !res2.Replayed || res2.Status != AuthRevoked {
		t.Fatalf("再次提交应回放已保存的撤销结果: %+v, %v", res2, err)
	}
	if evs, _ := r2.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("回放不应新增撤销记录: %+v", evs)
	}
}

// TestRevokeAuthzSuccessSaveFailureProxyRulesUnchanged 覆盖：失败后的授权
// 可否代转仍按原规则判断——已到期时代转继续返回到期拒绝，藏品已易手（持有
// 版本变化）时继续返回版本冲突；不能为了恢复撤销前状态让这两类旧授权重新
// 可用。
func TestRevokeAuthzSuccessSaveFailureProxyRulesUnchanged(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		base := time.Unix(1_700_000_000, 0)
		r.now = func() time.Time { return base }
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		// 越过到期点：授权已到期但尚未使用。
		r.now = func() time.Time { return base.Add(2 * time.Hour) }
		req := revokeAuthzSaveReq("alice", "a1", "rv-exp")

		blockBatchSave(t, r)
		res, err := r.RevokeAuthorization(req)
		assertRevokeAuthzSaveFailureEmpty(t, res, err)
		assertRevokeRequestFree(t, r, req)
		// 授权回到撤销前：到期状态按当前时间实时判断，撤销时间为空。
		a, _ := r.GetAuthorization("a1")
		if a.Status != AuthExpired || a.RevokedAt != (time.Time{}) {
			t.Fatalf("未保存的撤销后授权应仍只是到期: %+v", a)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
			t.Fatalf("未保存的撤销不应新增记录: %+v", evs)
		}

		// 保存恢复后受托人仍不能使用这份授权：到期按原规则拒绝，不能为恢复
		// 撤销前状态让旧授权重新可用（该到期拒绝随后正常落盘）。
		restoreBatchSave(t, r)
		if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p-exp")); !errors.Is(err, ErrAuthorizationExpired) {
			t.Fatalf("到期授权的代转应继续返回到期拒绝: %v", err)
		}

		// 原撤销请求重提：到期未使用仍可撤销，本次完成（expired -> revoked）。
		res, err = r.RevokeAuthorization(req)
		if err != nil || res.Replayed || res.Status != AuthRevoked {
			t.Fatalf("恢复后应本次撤销到期授权: %+v, %v", res, err)
		}
		evs, _ := r.AuthorizationHistory("i1")
		if len(evs) != 2 || evs[1].FromStatus != AuthExpired ||
			evs[1].ToStatus != AuthRevoked {
			t.Fatalf("撤销记录应为 expired -> revoked: %+v", evs)
		}
	})

	t.Run("item_moved", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		// 授权人把藏品直接转让给 bob：i1 变为 bob 版本 2，a1 绑定的 alice
		// 版本 1 已不匹配。
		if _, err := r.Transfer(xferReq("alice", "i1", "bob", 1, "direct-1")); err != nil {
			t.Fatal(err)
		}
		req := revokeAuthzSaveReq("alice", "a1", "rv-moved")

		blockBatchSave(t, r)
		res, err := r.RevokeAuthorization(req)
		assertRevokeAuthzSaveFailureEmpty(t, res, err)
		assertRevokeRequestFree(t, r, req)
		assertHoldingUnchanged(t, r, "i1", "bob", 2, 2)

		// 保存恢复后藏品已易手、版本不符：受托人继续被版本冲突拒绝，旧授权
		// 不因撤销失败回滚而恢复。
		restoreBatchSave(t, r)
		if _, err := r.ProxyTransfer(proxyReq("bob", "a1", "p-moved")); !errors.Is(err, ErrConflict) {
			t.Fatalf("藏品易手后代转应继续返回版本冲突: %v", err)
		}

		// 授权人仍可撤销这份未使用授权（撤销不受持有版本限制）；用原请求本次
		// 完成撤销，而不是回放那次未保存的成功。
		res, err = r.RevokeAuthorization(req)
		if err != nil || res.Replayed || res.Status != AuthRevoked {
			t.Fatalf("恢复后应本次完成撤销: %+v, %v", res, err)
		}
	})
}

// TestRevokeAuthzSuccessFailedSaveRequestIDReusable 覆盖：保存失败的撤销不
// 占用请求号——同一授权人用同一请求号撤销另一份自己有权撤销的授权时不能
// 遭遇请求号冲突，应正常成功且不标回放；只有实际保存过的结果才继续适用原有
// 的同号不同参数冲突规则。
func TestRevokeAuthzSuccessFailedSaveRequestIDReusable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 再发行 i2 并为其创建另一份未使用授权 a3。
	if _, err := r.Issue(issueReq("i2", "alice")); err != nil {
		t.Fatal(err)
	}
	a3req := CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托代转", RequestID: "create-a3",
		AuthID: "a3", ItemID: "i2", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 1, ExpiresAt: r.now().Add(time.Hour),
	}
	if _, err := r.CreateAuthorization(a3req); err != nil {
		t.Fatal(err)
	}
	failedReq := revokeAuthzSaveReq("alice", "a1", "rv-x")
	otherReq := revokeAuthzSaveReq("alice", "a3", "rv-x")

	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(failedReq)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, failedReq)
	assertAuthzStillActive(t, r, "a1")

	// 保存条件未恢复时重放原失败请求：仍只失败在保存上。
	res, err = r.RevokeAuthorization(failedReq)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, failedReq)

	// 保存恢复后，同一操作者用同一请求号撤销另一份有权撤销的授权：未保存的
	// 撤销不适用冲突规则，请求正常成功且不标回放。
	restoreBatchSave(t, r)
	res, err = r.RevokeAuthorization(otherReq)
	if err != nil || res.Replayed || res.Err != nil ||
		res.AuthID != "a3" || res.Status != AuthRevoked {
		t.Fatalf("未保存的撤销不应阻碍同号撤销另一份授权: %+v, %v", res, err)
	}
	if a3, _ := r.GetAuthorization("a3"); a3.Status != AuthRevoked {
		t.Fatalf("a3 应已撤销: %+v", a3)
	}
	// a1 不受影响、仍有效。
	assertAuthzStillActive(t, r, "a1")

	// 请求号此刻已被实际保存的 a3 成功结果占用：同号不同参数（改撤 a1）按
	// 原有规则返回 ErrRequestConflict。
	if _, err := r.RevokeAuthorization(failedReq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("实际保存过的结果才应触发同号不同参数冲突: %v", err)
	}
	// 完全相同的已保存请求（撤 a3）回放首次成功结果。
	res, err = r.RevokeAuthorization(otherReq)
	if err != nil || !res.Replayed || res.AuthID != "a3" ||
		res.Status != AuthRevoked {
		t.Fatalf("已保存的同号成功结果应被回放: %+v, %v", res, err)
	}
}

// TestRevokeAuthzAlreadyRevokedIdempotentSaveFailure 覆盖：已撤销授权再次
// 撤销的幂等分支也要以保存完成为准——请求结果落盘失败时返回保存错误与空
// 结果，请求号不被占用，已撤销终态与既有撤销记录保持原样（不新增记录）。
// 保存恢复后重提按幂等成功返回（仍不新增记录），再原样提交才回放。
func TestRevokeAuthzAlreadyRevokedIdempotentSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 首次撤销成功：新增一条撤销记录。
	first, err := r.RevokeAuthorization(revokeAuthzSaveReq("alice", "a1", "rv-1"))
	if err != nil || first.Status != AuthRevoked || first.Replayed {
		t.Fatalf("首次撤销应成功: %+v, %v", first, err)
	}

	// 已撤销终态下用新请求号再次撤销：幂等不新增记录，但请求结果需要落盘。
	req := revokeAuthzSaveReq("alice", "a1", "rv-2")
	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthRevoked {
		t.Fatalf("保存失败不应改变已撤销终态: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("幂等重撤保存失败不应新增记录: %+v", evs)
	}

	restoreBatchSave(t, r)
	res, err = r.RevokeAuthorization(req)
	if err != nil || res.Replayed || res.AuthID != "a1" ||
		res.Status != AuthRevoked {
		t.Fatalf("恢复后重提应幂等成功且首次保存: %+v, %v", res, err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("幂等重撤不应新增撤销记录: %+v", evs)
	}
	res, err = r.RevokeAuthorization(req)
	if err != nil || !res.Replayed || res.Status != AuthRevoked {
		t.Fatalf("再次提交应回放已保存的幂等结果: %+v, %v", res, err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("回放不应新增记录: %+v", evs)
	}
}
