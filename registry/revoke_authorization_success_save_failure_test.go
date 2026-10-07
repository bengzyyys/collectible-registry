package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为撤销代转授权补充"撤销成功本身保存失败"场景的回归保障。既有的
// revoke_authorization_save_failure_test.go 覆盖的是无权（ErrForbidden）
// 与已使用（ErrAuthorizationUsed）两类状态类拒绝的落盘失败；这里覆盖的是
// 请求符合全部业务条件、授权人撤销一份尚未使用的授权，但授权状态与撤销时间、
// 撤销历史、授权历史序号与请求结果尚未原子替换原登记册就发生写入失败的
// 情形——这样的撤销实际没有保存，绝不能让同一个仍打开的登记册提前把授权
// 显示为已撤销、阻止受托人使用原授权，即使原数据在失败后暂时无法读取或解析。
//
// 失败返回必须保留实际写入错误并给出空结果（不携带授权编号或状态、业务错误
// 为空、不标回放）。同一个仍打开的登记册上必须保留撤销前的授权内容与撤销
// 时间：不增加撤销历史、不占用本次请求号、不消耗授权历史序号，藏品持有人与
// 版本以及此前已保存的其他授权和历史原样保留；原数据仍可读取的普通保存失败
// 遵守同样规则。保存条件未恢复时重提仍实际尝试保存并返回当次保存错误，不能
// 回放幻影成功；随后另一次正常操作成功保存也不会把这次未保存的撤销夹带
// 落盘。保存条件恢复后用同一操作者、授权编号、原因和请求号再次撤销，按此时
// 的授权情况重新处理：授权仍未使用且允许撤销时本次才完成撤销并新增一条记录
// 本次生效时间、操作者、原因与前后状态的历史（不标回放），此后原样提交才
// 回放这个已保存结果；恢复期间授权已被受托人使用的，重提按
// ErrAuthorizationUsed 重新处理。失败后的授权可否代转仍按原规则判断：持有
// 版本未变、相关账户可用且未到期时受托人仍能正常使用；已到期或藏品已易手时
// 代转继续返回相应业务拒绝，不能为了恢复撤销前状态让本已失效的授权重新
// 可用。已经保存的终态（已使用、已撤销）不能被另一次撤销的失败恢复。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件改写
// 为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建状态"。

// fixedClock 在登记册上安装固定时钟并返回指向当前时间的指针，测试通过给
// *tick 赋新值推进时间。
func fixedClock(r *Registry, start time.Time) (tick *time.Time, restore func()) {
	t := start
	r.now = func() time.Time { return t }
	return &t, func() { r.now = time.Now }
}

// assertRevokeAuthStillActive 在（撤销成功保存失败后的）同一个已打开登记册
// 上核对：授权仍是撤销前的内容——状态有效、撤销时间为空；该藏品的授权变更
// 记录保持既有条数且没有本次撤销，授权历史序号未被消耗；请求号空闲；藏品
// 仍由原持有人以原版本持有。
func assertRevokeAuthStillActive(t *testing.T, r *Registry, req RevokeAuthorizationRequest,
	item string, wantEvents int, wantNextAuthSeq int64) {
	t.Helper()
	a, err := r.GetAuthorization(req.AuthID)
	if err != nil || a.Status != AuthActive || a.RevokedAt != (time.Time{}) {
		t.Fatalf("未保存的撤销不应改变授权内容与撤销时间: %+v, %v", a, err)
	}
	evs, err := r.AuthorizationHistory(item)
	if err != nil {
		t.Fatalf("AuthorizationHistory %s: %v", item, err)
	}
	if len(evs) != wantEvents {
		t.Fatalf("未保存的撤销不应新增授权变更记录，仍应 %d 条: %+v", wantEvents, evs)
	}
	for _, e := range evs {
		if e.Kind == "revoke" && e.AuthID == req.AuthID {
			t.Fatalf("未保存的撤销不应留下撤销记录: %+v", e)
		}
		if e.RequestID == req.RequestID {
			t.Fatalf("未保存的撤销不应在变更记录中出现请求号 %s: %+v", req.RequestID, e)
		}
	}
	if r.state.NextAuthSeq != wantNextAuthSeq {
		t.Fatalf("NextAuthSeq = %d，未保存的撤销不应消耗授权历史序号，仍应为 %d",
			r.state.NextAuthSeq, wantNextAuthSeq)
	}
	assertRevokeRequestFree(t, r, req)
	assertHoldingUnchanged(t, r, item, "alice", 1, 1)
}

// TestRevokeAuthzSuccessSaveFailureUnreadableThenProxyStillWorks 覆盖核心
// 场景：授权人撤销一份尚未使用的授权时写入失败且原数据同时被改写为无法
// 解析（commit 无法按磁盘重建状态）。返回保存错误与空结果；同一登记册保留
// 撤销前的授权内容、撤销时间、授权变更记录、授权历史序号、请求号与藏品
// 持有。保存仍失败时重提依旧失败、不留痕；读写恢复后先做一次无关操作成功
// 保存也不夹带这次撤销，受托人仍可正常凭授权代转——撤销失败没有提前阻止
// 受托人使用原授权。代转后再用原撤销请求重提，按授权已使用重新处理（首次
// 保存 ErrAuthorizationUsed 拒绝，非回放），再提才回放。
func TestRevokeAuthzSuccessSaveFailureUnreadableThenProxyStillWorks(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	base := time.Unix(2_000_000_000, 0)
	tick, restoreClock := fixedClock(r, base)
	defer restoreClock()
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", 24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("alice", "a1", "rv-ok")

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	// 撤销落盘失败：返回保存错误与空结果，授权仍是撤销前状态。
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeAuthStillActive(t, r, req, "i1", 1, 1)

	// 保存条件未恢复、磁盘仍不可读时再次提交：仍失败在保存上，不能把上次
	// 未保存的撤销当成已保存结果回放。
	res, err = r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeAuthStillActive(t, r, req, "i1", 1, 1)

	// 恢复正常读写；先让一次无关登记操作成功保存，不能把未保存的撤销一并
	// 写入（状态、撤销历史、请求结果都不带入）。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertRevokeAuthStillActive(t, r, req, "i1", 1, 1)

	// 受托人仍能正常使用原授权：持有版本未变、账户可用且未到期，代转成功，
	// 不返回"已撤销"。
	pres, perr := r.ProxyTransfer(proxyReq("bob", "a1", "p-after-failed-revoke"))
	if perr != nil || pres.Replayed || pres.AuthID != "a1" ||
		pres.ItemID != "i1" || pres.Version != 2 || pres.TxSeq != 2 {
		t.Fatalf("撤销失败不应阻止受托人使用原授权: %+v, %v", pres, perr)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthUsed || a.UsedTxSeq != 2 {
		t.Fatalf("代转后授权应为已使用: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 ||
		evs[1].Kind != "use" || evs[1].AuthID != "a1" || evs[1].TxSeq != 2 {
		t.Fatalf("代转应新增使用记录而非撤销记录: %+v", evs)
	}

	// 授权已使用后用原撤销请求重提：按此时的授权重新处理，首次保存
	// ErrAuthorizationUsed 拒绝（不标回放，结果带授权编号与业务错误），
	// 而不是回放那次未保存的成功；再原样提交才回放该拒绝。
	*tick = tick.Add(time.Minute)
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("授权已使用后重提应按 ErrAuthorizationUsed 重新处理: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationUsed) ||
		res.Status != "" {
		t.Fatalf("已使用拒绝的重提结果异常: %+v", res)
	}
	assertRevokeRequestSaved(t, r, req)
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("已使用拒绝不应新增撤销历史: %+v", evs)
	}
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的已使用拒绝: %+v, err %v", res, err)
	}
}

// TestRevokeAuthzSuccessSaveFailureRetryCompletesRevoke 覆盖：保存恢复后用
// 同一操作者、授权编号、原因和请求号再次撤销，授权仍未使用且允许撤销时
// 本次才完成撤销——记录本次生效（而非上次失败）的时间、操作者、原因与前后
// 状态，授权历史序号才被消耗；此后原样提交才回放这个已保存结果。重开后
// 看到的同样是本次保存的撤销。
func TestRevokeAuthzSuccessSaveFailureRetryCompletesRevoke(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	base := time.Unix(2_000_000_000, 0)
	tick, restoreClock := fixedClock(r, base)
	defer restoreClock()
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", 24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("alice", "a1", "rv-ok")

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeAuthStillActive(t, r, req, "i1", 1, 1)

	// 恢复后推进时间再重提：撤销记录的必须是本次生效的时间，不是失败时刻。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	effectiveAt := base.Add(30 * time.Minute)
	*tick = effectiveAt

	res, err = r.RevokeAuthorization(req)
	if err != nil || res.Replayed || res.Err != nil ||
		res.AuthID != "a1" || res.Status != AuthRevoked {
		t.Fatalf("恢复后重提应在本次完成撤销: %+v, %v", res, err)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthRevoked || !a.RevokedAt.Equal(effectiveAt) {
		t.Fatalf("撤销时间应为本次生效时间 %s: %+v", effectiveAt, a)
	}
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 2 {
		t.Fatalf("本次撤销应新增一条历史: %+v", evs)
	}
	if e := evs[1]; e.Seq != 2 || e.AuthID != "a1" || e.Kind != "revoke" ||
		e.Operator != "alice" || e.Reason != "撤销授权" || e.RequestID != "rv-ok" ||
		e.FromStatus != AuthActive || e.ToStatus != AuthRevoked ||
		!e.OccurredAt.Equal(effectiveAt) {
		t.Fatalf("撤销历史应记录本次生效的时间、操作者、原因与前后状态: %+v", e)
	}
	if r.state.NextAuthSeq != 2 {
		t.Fatalf("NextAuthSeq = %d, want 2", r.state.NextAuthSeq)
	}

	// 原样再提交才回放已保存结果：不第二次撤销、不新增历史、撤销时间不变。
	*tick = tick.Add(time.Hour)
	res, err = r.RevokeAuthorization(req)
	if err != nil || !res.Replayed || res.AuthID != "a1" || res.Status != AuthRevoked {
		t.Fatalf("成功保存后再提应回放已保存结果: %+v, err %v", res, err)
	}
	a, _ = r.GetAuthorization("a1")
	if !a.RevokedAt.Equal(effectiveAt) {
		t.Fatalf("回放不应改变撤销时间: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("回放不应新增历史: %+v", evs)
	}
	if _, perr := r.ProxyTransfer(proxyReq("bob", "a1", "p-late")); !errors.Is(perr, ErrAuthorizationRevoked) {
		t.Fatalf("撤销已保存后代转应被拒绝: %v", perr)
	}

	// 磁盘视角：重开后仍是本次保存的撤销与撤销时间，原失败无影可寻。
	dir := r.dir
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	r2.now = func() time.Time { return *tick }
	a2, _ := r2.GetAuthorization("a1")
	if a2.Status != AuthRevoked || !a2.RevokedAt.Equal(effectiveAt) {
		t.Fatalf("重开后撤销内容异常: %+v", a2)
	}
	if evs, _ := r2.AuthorizationHistory("i1"); len(evs) != 2 ||
		evs[1].RequestID != "rv-ok" || !evs[1].OccurredAt.Equal(effectiveAt) {
		t.Fatalf("重开后撤销历史异常: %+v", evs)
	}
	res2, err := r2.RevokeAuthorization(req)
	if err != nil || !res2.Replayed || res2.Status != AuthRevoked {
		t.Fatalf("重开后再提应回放已保存撤销: %+v, err %v", res2, err)
	}
}

// TestRevokeAuthzSuccessSaveFailureReadableDisk 覆盖：写入失败但原数据仍可
// 正常读取（commit 据磁盘内容重建状态）时遵守同样规则——返回保存错误与空
// 结果，授权从未被撤销；恢复后原请求重提本次才完成撤销，再提才回放。
func TestRevokeAuthzSuccessSaveFailureReadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("alice", "a1", "rv-ok-r")

	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeAuthStillActive(t, r, req, "i1", 1, 1)

	// 保存仍被阻断时重提仍失败在保存上。
	res, err = r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeAuthStillActive(t, r, req, "i1", 1, 1)

	restoreBatchSave(t, r)
	res, err = r.RevokeAuthorization(req)
	if err != nil || res.Replayed || res.Err != nil ||
		res.AuthID != "a1" || res.Status != AuthRevoked {
		t.Fatalf("可读磁盘失败恢复后重提应本次完成撤销: %+v, err %v", res, err)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthRevoked || a.RevokedAt == (time.Time{}) {
		t.Fatalf("重提后授权应已记录撤销时间: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 || evs[1].Kind != "revoke" {
		t.Fatalf("重提应新增一条撤销历史: %+v", evs)
	}
	replay, err := r.RevokeAuthorization(req)
	if err != nil || !replay.Replayed || replay.Status != AuthRevoked {
		t.Fatalf("成功保存后再提应回放: %+v, err %v", replay, err)
	}
}

// TestRevokeAuthzSuccessSaveFailureAfterReopen 覆盖磁盘视角：撤销成功保存
// 失败（原数据可读）后关闭重开，看到的仍是撤销前状态——授权有效、撤销时间
// 为空、只有创建记录、请求号未被占用；恢复保存后用原请求重提本次才完成
// 撤销（非回放），再次提交才回放。
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
	a, _ := r2.GetAuthorization("a1")
	if a.Status != AuthActive || a.RevokedAt != (time.Time{}) {
		t.Fatalf("重开后授权应仍是撤销前状态: %+v", a)
	}
	if evs, _ := r2.AuthorizationHistory("i1"); len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("重开后授权变更记录应只有创建: %+v", evs)
	}
	if r2.state.NextAuthSeq != 1 {
		t.Fatalf("重开后 NextAuthSeq = %d, want 1", r2.state.NextAuthSeq)
	}
	assertRevokeRequestFree(t, r2, req)

	restoreBatchSave(t, r2)
	res2, err := r2.RevokeAuthorization(req)
	if err != nil || res2.Replayed || res2.Err != nil ||
		res2.AuthID != "a1" || res2.Status != AuthRevoked {
		t.Fatalf("重开后重提应本次完成撤销: %+v, err %v", res2, err)
	}
	if evs, _ := r2.AuthorizationHistory("i1"); len(evs) != 2 || evs[1].Kind != "revoke" {
		t.Fatalf("重提应新增一条撤销历史: %+v", evs)
	}
	res2, err = r2.RevokeAuthorization(req)
	if err != nil || !res2.Replayed || res2.Status != AuthRevoked {
		t.Fatalf("再次提交应回放已保存撤销: %+v, err %v", res2, err)
	}
}

// TestRevokeAuthzSuccessSaveFailureProxyRulesUnchanged 覆盖失败后的授权可否
// 代转仍按原规则判断：到期后代转继续返回 ErrAuthorizationExpired，藏品易手
// 后代转继续返回 ErrConflict，不能为了恢复撤销前状态让本已失效的授权重新
// 可用；两种情况下撤销请求重提都按当时的授权处理（到期未使用仍可撤销，
// 变更记录从 expired 记为 revoked；易手后未使用同样由授权人完成撤销）。
func TestRevokeAuthzSuccessSaveFailureProxyRulesUnchanged(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		base := time.Unix(2_000_000_000, 0)
		tick, restoreClock := fixedClock(r, base)
		defer restoreClock()
		// 授权在 base+1h 到期。
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		req := revokeAuthzSaveReq("alice", "a1", "rv-exp")

		blockBatchSave(t, r)
		res, err := r.RevokeAuthorization(req)
		assertRevokeAuthzSaveFailureEmpty(t, res, err)
		assertRevokeAuthStillActive(t, r, req, "i1", 1, 1)

		// 恢复保存条件并越过到期点：授权实时显示已过期，代转继续按到期
		// 拒绝（这次拒绝正常落盘），不会因撤销失败被提前阻止，也不会让旧
		// 授权重新可用。
		restoreBatchSave(t, r)
		*tick = base.Add(2 * time.Hour)
		a, _ := r.GetAuthorization("a1")
		if a.Status != AuthExpired {
			t.Fatalf("到期后授权应显示已过期: %+v", a)
		}
		if _, perr := r.ProxyTransfer(proxyReq("bob", "a1", "p-late")); !errors.Is(perr, ErrAuthorizationExpired) {
			t.Fatalf("到期后代转应继续返回 ErrAuthorizationExpired: %v", perr)
		}

		// 到期但未使用的授权仍可撤销：重提本次完成撤销，历史从 expired
		// 记为 revoked，记录本次生效时间。
		res, err = r.RevokeAuthorization(req)
		if err != nil || res.Replayed || res.Status != AuthRevoked {
			t.Fatalf("到期未使用授权重提应本次完成撤销: %+v, err %v", res, err)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 ||
			evs[1].FromStatus != AuthExpired || evs[1].ToStatus != AuthRevoked ||
			!evs[1].OccurredAt.Equal(*tick) {
			t.Fatalf("撤销历史应从 expired 记为 revoked 并记录本次时间: %+v", evs)
		}
		replay, err := r.RevokeAuthorization(req)
		if err != nil || !replay.Replayed || replay.Status != AuthRevoked {
			t.Fatalf("再提应回放已保存撤销: %+v, err %v", replay, err)
		}
	})

	t.Run("item_changed_hands", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		req := revokeAuthzSaveReq("alice", "a1", "rv-hand")

		blockBatchSave(t, r)
		res, err := r.RevokeAuthorization(req)
		assertRevokeAuthzSaveFailureEmpty(t, res, err)
		assertRevokeAuthStillActive(t, r, req, "i1", 1, 1)

		restoreBatchSave(t, r)
		// 恢复后藏品先被授权人直接转给接收人，持有版本变为 2。
		if _, err := r.Transfer(xferReq("alice", "i1", "carol", 1, "direct-hand")); err != nil {
			t.Fatal(err)
		}
		// 受托人代转继续按持有版本冲突拒绝——失败撤销的回滚不让旧授权
		// 重新可用。
		if _, perr := r.ProxyTransfer(proxyReq("bob", "a1", "p-hand")); !errors.Is(perr, ErrConflict) {
			t.Fatalf("藏品易手后代转应继续返回 ErrConflict: %v", perr)
		}

		// 授权仍未使用，授权人重提撤销按当时授权处理：本次完成撤销，不回放
		// 那次未保存的成功。
		res, err = r.RevokeAuthorization(req)
		if err != nil || res.Replayed || res.Status != AuthRevoked {
			t.Fatalf("易手后未使用授权重提应本次完成撤销: %+v, err %v", res, err)
		}
		a, _ := r.GetAuthorization("a1")
		if a.Status != AuthRevoked || a.RevokedAt == (time.Time{}) {
			t.Fatalf("重提后授权应已记录撤销: %+v", a)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 ||
			evs[1].Kind != "revoke" || evs[1].AuthID != "a1" ||
			evs[1].FromStatus != AuthActive || evs[1].ToStatus != AuthRevoked {
			t.Fatalf("本次撤销历史异常: %+v", evs)
		}
		replay, err := r.RevokeAuthorization(req)
		if err != nil || !replay.Replayed || replay.Status != AuthRevoked {
			t.Fatalf("再提应回放已保存撤销: %+v, err %v", replay, err)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
			t.Fatalf("回放不应新增历史: %+v", evs)
		}
	})
}

// TestRevokeAuthzSuccessFailedSaveRequestIDReusable 覆盖：撤销成功保存失败
// 不占用请求号——同一操作者用同一请求号撤销另一份自己有权撤销的授权时不
// 遭遇请求号冲突，该撤销正常完成；只有实际保存过的结果才继续适用原有的同号
// 不同参数冲突规则。未保存的撤销不影响受托人随后使用第一份授权。
func TestRevokeAuthzSuccessFailedSaveRequestIDReusable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuthorization(createAuthReq(r, "a2", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 创建记录依次为 create a1（序号 1）、create a2（序号 2）。
	failedReq := revokeAuthzSaveReq("alice", "a1", "rv-x")
	otherReq := revokeAuthzSaveReq("alice", "a2", "rv-x")

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	res, err := r.RevokeAuthorization(failedReq)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeAuthStillActive(t, r, failedReq, "i1", 2, 2)
	if a2, _ := r.GetAuthorization("a2"); a2.Status != AuthActive {
		t.Fatalf("另一份授权不应受影响: %+v", a2)
	}

	// 保存仍阻断时重提仍失败在保存上。
	res, err = r.RevokeAuthorization(failedReq)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeAuthStillActive(t, r, failedReq, "i1", 2, 2)

	// 保存恢复后，同一操作者用同一请求号撤销另一份授权：未保存的撤销不适用
	// 冲突规则，请求正常成功且不标回放，新增一条 a2 的撤销历史（序号 3）。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	res, err = r.RevokeAuthorization(otherReq)
	if err != nil || res.Replayed || res.Err != nil ||
		res.AuthID != "a2" || res.Status != AuthRevoked {
		t.Fatalf("未保存的撤销不应阻碍同号撤销另一份授权: %+v, err %v", res, err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 3 ||
		evs[2].Seq != 3 || evs[2].AuthID != "a2" || evs[2].Kind != "revoke" {
		t.Fatalf("撤销 a2 应新增序号 3 的撤销记录: %+v", evs)
	}
	if r.state.NextAuthSeq != 3 {
		t.Fatalf("NextAuthSeq = %d, want 3", r.state.NextAuthSeq)
	}

	// a1 仍未撤销：受托人可正常使用，使用记录序号 4。
	if pres, perr := r.ProxyTransfer(proxyReq("bob", "a1", "p-a1")); perr != nil ||
		pres.Replayed || pres.TxSeq != 2 {
		t.Fatalf("a1 应仍可代转: %+v, %v", pres, perr)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 4 ||
		evs[3].Seq != 4 || evs[3].Kind != "use" || evs[3].AuthID != "a1" {
		t.Fatalf("代转 a1 应新增序号 4 的使用记录: %+v", evs)
	}

	// 请求号此刻已被实际保存的 a2 撤销结果占用：同号不同参数（改撤 a1）
	// 按原有规则返回 ErrRequestConflict，即使 a1 现已使用。
	if _, err := r.RevokeAuthorization(failedReq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("实际保存过的结果才应触发同号不同参数冲突: %v", err)
	}
	// 完全相同的已保存请求（撤 a2）回放首次成功结果。
	res, err = r.RevokeAuthorization(otherReq)
	if err != nil || !res.Replayed || res.AuthID != "a2" || res.Status != AuthRevoked {
		t.Fatalf("已保存的同号撤销结果应被回放: %+v, err %v", res, err)
	}
	// a2 已撤销终态不变：受托人代转仍被拒绝。
	if _, perr := r.ProxyTransfer(proxyReq("bob", "a2", "p-a2")); !errors.Is(perr, ErrAuthorizationRevoked) {
		t.Fatalf("a2 应仍为已撤销: %v", perr)
	}
}

// TestRevokeAuthzSuccessSaveFailureKeepsSavedTerminalStates 覆盖：另一份授权
// 已经保存的终态不能被本次失败恢复——先成功撤销 a2，再让撤销 a1 的保存
// 失败，a2 必须仍是已撤销（受托人继续被拒、撤销历史不增加），a1 则保留
// 撤销前内容并仍可由受托人使用；a1 被使用后原撤销请求按已使用重新处理。
func TestRevokeAuthzSuccessSaveFailureKeepsSavedTerminalStates(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuthorization(createAuthReq(r, "a2", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 先成功撤销 a2：create a1（1）、create a2（2）、revoke a2（3）。
	if _, err := r.RevokeAuthorization(revokeAuthzSaveReq("alice", "a2", "rv-a2")); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("alice", "a1", "rv-a1")

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)

	// a1 保留撤销前状态；a2 已保存的已撤销终态不被本次失败恢复。
	a1, _ := r.GetAuthorization("a1")
	if a1.Status != AuthActive || a1.RevokedAt != (time.Time{}) {
		t.Fatalf("a1 应保留撤销前状态: %+v", a1)
	}
	a2, _ := r.GetAuthorization("a2")
	if a2.Status != AuthRevoked || a2.RevokedAt == (time.Time{}) {
		t.Fatalf("a2 已保存的撤销终态不能被失败恢复: %+v", a2)
	}
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 3 || evs[2].Kind != "revoke" || evs[2].AuthID != "a2" {
		t.Fatalf("授权变更记录应保持 create a1、create a2、revoke a2: %+v", evs)
	}
	if r.state.NextAuthSeq != 3 {
		t.Fatalf("NextAuthSeq = %d, want 3", r.state.NextAuthSeq)
	}
	assertRevokeRequestFree(t, r, req)

	// 恢复后：a1 仍可代转（新增使用记录序号 4），a2 继续按已撤销拒绝。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if pres, perr := r.ProxyTransfer(proxyReq("bob", "a1", "p-a1")); perr != nil ||
		pres.TxSeq != 2 || pres.Version != 2 {
		t.Fatalf("a1 应仍可代转: %+v, %v", pres, perr)
	}
	if _, perr := r.ProxyTransfer(proxyReq("bob", "a2", "p-a2")); !errors.Is(perr, ErrAuthorizationRevoked) {
		t.Fatalf("a2 已保存的撤销终态应继续有效: %v", perr)
	}

	// 原撤销请求按 a1 已使用重新处理，首次保存该拒绝（非回放），再提回放。
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrAuthorizationUsed) || res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationUsed) {
		t.Fatalf("a1 已使用后重提应首次保存已使用拒绝: %+v, err %v", res, err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 4 {
		t.Fatalf("已使用拒绝不应新增撤销历史: %+v", evs)
	}
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed {
		t.Fatalf("再提应回放已使用拒绝: %+v, err %v", res, err)
	}
	// 已保存的 a2 撤销请求照常回放，不新增历史。
	r2res, r2err := r.RevokeAuthorization(revokeAuthzSaveReq("alice", "a2", "rv-a2"))
	if r2err != nil || !r2res.Replayed || r2res.Status != AuthRevoked {
		t.Fatalf("a2 的已保存撤销应照常回放: %+v, err %v", r2res, r2err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 4 {
		t.Fatalf("回放 a2 撤销不应新增历史: %+v", evs)
	}
}

// TestRevokeAuthzAlreadyRevokedSaveFailure 覆盖已撤销幂等分支自身的保存
// 失败：授权已撤销是已保存终态，再次撤销只登记请求结果、不新增历史；该
// 请求结果落盘失败时同样返回保存错误与空结果并回滚请求号占用，终态本身
// 保持不变。恢复后重提首次登记该成功（非回放、不新增历史），再提才回放。
func TestRevokeAuthzAlreadyRevokedSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 先成功撤销：create a1（1）、revoke a1（2）。
	first, err := r.RevokeAuthorization(revokeAuthzSaveReq("alice", "a1", "rv1"))
	if err != nil || first.Status != AuthRevoked || first.Replayed {
		t.Fatalf("首次撤销应成功: %+v, %v", first, err)
	}
	savedRevokedAt := r.state.Authzs["a1"].RevokedAt
	req := revokeAuthzSaveReq("alice", "a1", "rv2")

	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	// 已撤销终态保持，撤销时间不变；新请求号未占用，历史不增加，序号不消耗。
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthRevoked || !a.RevokedAt.Equal(savedRevokedAt) {
		t.Fatalf("幂等撤销保存失败不应改变已撤销终态: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("幂等撤销保存失败不应新增历史: %+v", evs)
	}
	if r.state.NextAuthSeq != 2 {
		t.Fatalf("NextAuthSeq = %d, want 2", r.state.NextAuthSeq)
	}
	assertRevokeRequestFree(t, r, req)

	// 保存仍阻断时重提仍失败，不能回放幻影成功。
	res, err = r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)

	restoreBatchSave(t, r)
	res, err = r.RevokeAuthorization(req)
	if err != nil || res.Replayed || res.Err != nil ||
		res.AuthID != "a1" || res.Status != AuthRevoked {
		t.Fatalf("恢复后重提应首次登记幂等成功而非回放: %+v, err %v", res, err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("幂等成功不应新增历史: %+v", evs)
	}
	res, err = r.RevokeAuthorization(req)
	if err != nil || !res.Replayed || res.Status != AuthRevoked {
		t.Fatalf("再提应回放已保存结果: %+v, err %v", res, err)
	}
}
