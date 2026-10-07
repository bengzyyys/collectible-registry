package registry

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

// 本文件为限时一次性代转（ProxyTransfer）补充"成功代转本身保存失败"场景的
// 回归保障。既有的 proxy_transfer_save_failure_test.go 覆盖的是状态类业务
// 拒绝的落盘失败；这里覆盖的是受托人通过 ProxyTransfer 使用一份仍有效的
// 授权、账户与持有版本等业务条件全部满足，但本次代转的持有换人/版本/转让
// 历史/版税应付/授权使用状态/授权使用记录/请求结果尚未原子替换原数据就发生
// 写入失败的情形——这样的代转实际没有保存，绝不能成为登记册认可的交易，
// 即使原数据在失败后暂时无法读取或解析。
//
// 失败返回必须保留实际写入错误并给出空结果（不带授权编号、藏品编号、前后
// 持有人、版本、历史序号、价款、应付或余款，业务错误为空，也不标回放）。
// 同一个仍打开的登记册上：藏品继续属于提交前的持有人，版本及双方持有列表
// 不变，藏品历史没有本次转让；授权没有使用时间或关联转让序号，授权历史没有
// 本次使用记录；版税计算记录与收款账户的应付明细不多出这一笔；请求号、藏品
// 历史序号与授权历史序号都不被消耗；失败前已经保存的交易、授权及应付原样
// 保留。读写恢复后，即使用户先登记一个无关账户并成功保存，也不能把这笔未
// 保存的代转夹带落盘；受托人用原请求号、原因和授权编号重提时按当时的授权、
// 账户与持有状态重新处理——条件仍满足就真正完成一次代转（版本只增加一次、
// 新增记录接续原有序号、价款仍取授权约定、余款仍归授权人、首次保存成功不标
// 回放），之后原样重提才返回该次已保存结果；授权已经到期、撤销或持有版本
// 已经变化则沿用对应拒绝，不能回放先前那次未保存的成功，也不因恢复状态而
// 延长授权有效期。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件改写
// 为无法解析的内容，覆盖"原数据暂时无法读取、commit 无法按磁盘重建状态"。

// proxySuccessPrice 是各用例授权中约定的代转价款（分）。
const proxySuccessPrice int64 = 10000

// seedProxySuccessWorld 建立带版税规则的代转世界：alice（系列创建人/授权人/
// 初始持有人）、bob（受托人）、carol（固定接收人兼 10% 版税收款人）、dave
// （5% 版税收款人）；系列 s1 在首次发行前设定 carol 1000、dave 500（万分
// 之）的版税，随后向 alice 发行 i1（历史序号 1、持有版本 1）。因此一笔
// 10000 分的成功代转：carol 应付 1000、dave 应付 500，余款 8500 归转让前
// 持有人（授权人 alice），转让历史序号 2；授权创建占授权历史序号 1，代转
// 成功的使用记录占授权历史序号 2。
func seedProxySuccessWorld(t *testing.T, r *Registry) {
	t.Helper()
	setupWorld(t, r) // alice、bob 与系列 s1
	if err := r.RegisterAccount("carol", "接收人"); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAccount("dave", "版税收款人"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "设定版税", RequestID: "royalty-s1", SeriesID: "s1",
		Shares: []RoyaltyShare{{AccountID: "carol", Rate: 1000}, {AccountID: "dave", Rate: 500}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(issueReq("i1", "alice")); err != nil {
		t.Fatal(err)
	}
}

// proxySuccessAuthReq 构造一份约定了价款的代转授权：alice 持有的 i1 委托
// bob 代转给 carol，绑定 alice 版本 1。
func proxySuccessAuthReq(r *Registry, authID string, price int64) CreateAuthorizationRequest {
	req := createAuthReq(r, authID, time.Hour)
	req.Price = price
	return req
}

// proxySuccessPayables 是 10000 分代转按 s1 规则应得的应付明细（按收款
// 账户排序）。
func proxySuccessPayables() []RoyaltyPayable {
	return []RoyaltyPayable{
		{AccountID: "carol", Rate: 1000, Amount: 1000},
		{AccountID: "dave", Rate: 500, Amount: 500},
	}
}

// assertProxySuccessStillPending 在（成功代转保存失败后的）同一个已打开登记
// 册上核对：这笔代转如同从未发生，而失败前已有的发行、授权创建与序号完整
// 保留。i1 仍属 alice 版本 1、历史只有发行一条；授权 a1 仍有效、未使用；
// 授权变更记录只有创建；藏品历史序号停在 1、授权历史序号停在 1；请求号空闲；
// 没有序号 2 的版税计算记录，carol/dave 没有应付。
func assertProxySuccessStillPending(t *testing.T, r *Registry, req ProxyTransferRequest) {
	t.Helper()
	// 藏品仍属提交前持有人 alice，版本没有增加，历史只有发行一条。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
	for _, e := range mustHistory(t, r, "i1") {
		if e.RequestID == req.RequestID {
			t.Fatalf("保存失败后 i1 历史出现请求号 %s: %+v", req.RequestID, e)
		}
	}

	// 双方持有列表不变：i1 没有离开 alice，carol 没有收到。
	aliceHoldings, err := r.HoldingsOf("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceHoldings) != 1 || aliceHoldings[0].ItemID != "i1" || aliceHoldings[0].Version != 1 {
		t.Fatalf("保存失败后 alice 持有清单异常: %+v", aliceHoldings)
	}
	carolHoldings, err := r.HoldingsOf("carol")
	if err != nil {
		t.Fatal(err)
	}
	if len(carolHoldings) != 0 {
		t.Fatalf("保存失败后 carol 不应收到藏品: %+v", carolHoldings)
	}

	// 授权没有使用时间或关联转让序号，仍处于有效状态。
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != AuthActive || a.UsedTxSeq != 0 || a.UsedAt != (time.Time{}) {
		t.Fatalf("保存失败后授权应仍未使用: %+v", a)
	}
	// 授权历史没有本次使用记录，仍只有创建一条。
	evs, err := r.AuthorizationHistory("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "create" || evs[0].AuthID != "a1" {
		t.Fatalf("保存失败后授权变更记录应只有创建: %+v", evs)
	}

	// 请求号、藏品历史序号与授权历史序号都不被这次失败消耗。
	assertProxyRequestFree(t, r, req)
	if r.state.NextSeq != 1 {
		t.Fatalf("NextSeq = %d，未保存代转不应消耗藏品历史序号，仍应为 1", r.state.NextSeq)
	}
	if r.state.NextAuthSeq != 1 {
		t.Fatalf("NextAuthSeq = %d，未保存代转不应消耗授权历史序号，仍应为 1", r.state.NextAuthSeq)
	}

	// 版税计算记录与收款账户应付明细不多出这一笔（序号 2 不存在）。
	if _, err := r.TransferRoyalty(2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未保存代转不应留下序号 2 的版税计算记录: %v", err)
	}
	for _, acct := range []string{"alice", "bob", "carol", "dave"} {
		got, err := r.PayablesOf(acct)
		if err != nil {
			t.Fatalf("PayablesOf %s: %v", acct, err)
		}
		if len(got) != 0 {
			t.Fatalf("未保存代转不应新增应付，%s 却有 %+v", acct, got)
		}
	}
}

func mustHistory(t *testing.T, r *Registry, item string) []HistoryEntry {
	t.Helper()
	hist, err := r.History(item)
	if err != nil {
		t.Fatalf("History %s: %v", item, err)
	}
	return hist
}

// TestProxyTransferSuccessSaveFailureUnreadableSameRegistry 覆盖核心场景：
// 业务条件全部满足后的成功代转在写入阶段失败，且原数据同时被改写为无法解析
// （commit 无法按磁盘重建状态）。调用返回保存错误与空结果；同一登记册上代
// 转如同从未发生。保存仍失败时重提依旧失败、不留痕。读写恢复后先登记一个
// 无关账户成功保存，也不把未保存代转带入；随后受托人用原请求号、原因与授权
// 编号重提，按当前状态真正完成一次代转（非回放、藏品与授权历史序号都接续
// 原有、价款取授权约定、余款归授权人 alice），再原样重提才回放该已保存结果。
func TestProxyTransferSuccessSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	seedProxySuccessWorld(t, r)
	if _, err := r.CreateAuthorization(proxySuccessAuthReq(r, "a1", proxySuccessPrice)); err != nil {
		t.Fatal(err)
	}
	req := proxyReq("bob", "a1", "p-ok")

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	// 成功代转落盘失败：返回保存错误与空结果。
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxySuccessStillPending(t, r, req)

	// 保存条件未恢复、磁盘仍不可读时再次提交同一请求：仍失败在保存上，
	// 不能把上次未保存的成功当成已保存结果回放。
	res, err = r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxySuccessStillPending(t, r, req)

	// 恢复正常读写；先登记一个无关账户成功保存，不能夹带这笔未保存的代转。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("eve", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertProxySuccessStillPending(t, r, req)

	// 用原受托人、请求号、原因和授权编号重提：授权仍有效、条件仍满足，真正
	// 完成一次代转，不标回放，版本只加一次，两类历史序号都接续原有（2）。
	res, err = r.ProxyTransfer(req)
	if err != nil {
		t.Fatalf("恢复后重提应真正完成一次代转: %v", err)
	}
	wantPayables := proxySuccessPayables()
	if res.Replayed || res.Err != nil || res.AuthID != "a1" || res.ItemID != "i1" ||
		res.FromID != "alice" || res.ToID != "carol" || res.Version != 2 || res.TxSeq != 2 ||
		res.Price != proxySuccessPrice || res.Remainder != 8500 ||
		!reflect.DeepEqual(res.Payables, wantPayables) {
		t.Fatalf("重提结果异常: %+v", res)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("重提成功后 i1 持有 = %+v", h)
	}
	hist := mustHistory(t, r, "i1")
	if len(hist) != 2 || hist[1].Seq != 2 || hist[1].Kind != "transfer" ||
		hist[1].RequestID != req.RequestID || hist[1].AuthID != "a1" ||
		hist[1].Operator != "bob" || hist[1].FromID != "alice" || hist[1].ToID != "carol" {
		t.Fatalf("重提成功后 i1 历史异常: %+v", hist)
	}
	tr, _ := r.TransferRoyalty(2)
	if tr.ItemID != "i1" || tr.Price != proxySuccessPrice || tr.Remainder != 8500 ||
		tr.OwnerID != "alice" || !reflect.DeepEqual(tr.Payables, wantPayables) {
		t.Fatalf("本次代转版税记录异常（余款应归授权人）: %+v", tr)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthUsed || a.UsedTxSeq != 2 || a.UsedAt != base {
		t.Fatalf("重提成功后授权应记为本次已使用: %+v", a)
	}
	evs, _ := r.AuthorizationHistory("i1")
	if len(evs) != 2 || evs[1].Kind != "use" || evs[1].AuthID != "a1" ||
		evs[1].Seq != 2 || evs[1].TxSeq != 2 {
		t.Fatalf("重提成功后授权使用记录异常: %+v", evs)
	}
	if r.state.NextSeq != 2 || r.state.NextAuthSeq != 2 {
		t.Fatalf("序号异常: NextSeq=%d NextAuthSeq=%d，都应为 2", r.state.NextSeq, r.state.NextAuthSeq)
	}

	// 只有此次成功保存后再原样重提，才返回这一笔已保存结果（回放），不再
	// 第二次换人、不重复计入应付。
	replay, err := r.ProxyTransfer(req)
	if err != nil || !replay.Replayed || replay.AuthID != "a1" || replay.Version != 2 ||
		replay.TxSeq != 2 || replay.Price != proxySuccessPrice || replay.Remainder != 8500 ||
		!reflect.DeepEqual(replay.Payables, wantPayables) {
		t.Fatalf("成功保存后再提应回放已保存结果: %+v, err %v", replay, err)
	}
	h, _ = r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("回放不应再次改变持有: %+v", h)
	}
	if hist := mustHistory(t, r, "i1"); len(hist) != 2 {
		t.Fatalf("回放不应新增历史: %+v", hist)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("回放不应新增授权使用记录: %+v", evs)
	}
	if carolPay, _ := r.PayablesOf("carol"); len(carolPay) != 1 || carolPay[0].TxSeq != 2 {
		t.Fatalf("回放不应重复计入应付: %+v", carolPay)
	}
}

// TestProxyTransferSuccessSaveFailureReadableSameRegistry 覆盖：写入失败但原
// 数据仍可正常读取（commit 据磁盘内容重建状态）时，得到相同的失败结果与
// 状态保障——同一登记册上代转从未发生；恢复后原请求重提真正完成一次。
func TestProxyTransferSuccessSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	seedProxySuccessWorld(t, r)
	if _, err := r.CreateAuthorization(proxySuccessAuthReq(r, "a1", proxySuccessPrice)); err != nil {
		t.Fatal(err)
	}
	req := proxyReq("bob", "a1", "p-ok-r")

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxySuccessStillPending(t, r, req)

	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(req)
	if err != nil || res.Replayed || res.AuthID != "a1" || res.ItemID != "i1" ||
		res.FromID != "alice" || res.ToID != "carol" || res.Version != 2 || res.TxSeq != 2 ||
		res.Price != proxySuccessPrice || res.Remainder != 8500 {
		t.Fatalf("可读磁盘失败恢复后重提应真正完成一次: %+v, err %v", res, err)
	}
	// 成功保存后再提才回放。
	replay, err := r.ProxyTransfer(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 2 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestProxyTransferSuccessSaveFailureAfterReopen 覆盖磁盘视角：成功代转保存
// 失败（原数据可读）后关闭重开，看到的仍是代转前状态；恢复保存后受托人用
// 原请求重提真正完成一次，而不是回放一个从未保存的成功。
func TestProxyTransferSuccessSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	seedProxySuccessWorld(t, r)
	if _, err := r.CreateAuthorization(proxySuccessAuthReq(r, "a1", proxySuccessPrice)); err != nil {
		t.Fatal(err)
	}
	req := proxyReq("bob", "a1", "p-ok-reopen")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)

	// 数据文件从未被替换：正常关闭并重新打开仍读到代转前状态。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	r2.now = func() time.Time { return base }
	assertProxySuccessStillPending(t, r2, req)

	restoreBatchSave(t, r2)
	res2, err := r2.ProxyTransfer(req)
	if err != nil || res2.Replayed || res2.TxSeq != 2 || res2.Version != 2 ||
		res2.AuthID != "a1" || res2.FromID != "alice" || res2.ToID != "carol" {
		t.Fatalf("重开后重提应真正完成一次: %+v, err %v", res2, err)
	}
	replay, err := r2.ProxyTransfer(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 2 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestProxyTransferSuccessSaveFailureRetryStateChanged 覆盖：保存恢复时授权、
// 账户或持有状态已经变化的，原请求重提沿用对应业务拒绝，不能回放那次未保存
// 的成功；这些拒绝是首次保存、不标回放，保存后再提才回放，且都不换人、不
// 消耗授权或序号。到期情形同时覆盖"不因恢复状态而延长授权有效期"。
func TestProxyTransferSuccessSaveFailureRetryStateChanged(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		base := time.Unix(1_700_000_000, 0)
		r.now = func() time.Time { return base }
		seedProxySuccessWorld(t, r)
		if _, err := r.CreateAuthorization(proxySuccessAuthReq(r, "a1", proxySuccessPrice)); err != nil {
			t.Fatal(err)
		}
		req := proxyReq("bob", "a1", "p-exp")

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
		assertProxySuccessStillPending(t, r, req)

		// 恢复读写，并越过到期点：授权到期是实时判断，恢复状态不会延长有效期。
		if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
			t.Fatal(err)
		}
		restoreBatchSave(t, r)
		r.now = func() time.Time { return base.Add(2 * time.Hour) }

		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationExpired) {
			t.Fatalf("授权到期后重提应返回 ErrAuthorizationExpired: %v", err)
		}
		if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationExpired) ||
			res.ItemID != "" || res.TxSeq != 0 {
			t.Fatalf("到期拒绝的重提结果异常: %+v", res)
		}
		assertProxyRequestSavedRejected(t, r, req, ErrAuthorizationExpired)
		// 到期拒绝不换人、不把授权用掉、不消耗序号。
		assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
		if a, _ := r.GetAuthorization("a1"); a.UsedTxSeq != 0 {
			t.Fatalf("到期拒绝不应使用授权: %+v", a)
		}
		if r.state.NextSeq != 1 || r.state.NextAuthSeq != 1 {
			t.Fatalf("到期拒绝不应消耗序号: NextSeq=%d NextAuthSeq=%d", r.state.NextSeq, r.state.NextAuthSeq)
		}
		// 此后原样重提回放已保存的到期拒绝，而不是执行代转。
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationExpired) || !res.Replayed ||
			!errors.Is(res.Err, ErrAuthorizationExpired) {
			t.Fatalf("保存后的重提应回放到期拒绝: %+v, err %v", res, err)
		}
		assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
	})

	t.Run("revoked", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		base := time.Unix(1_700_000_000, 0)
		r.now = func() time.Time { return base }
		seedProxySuccessWorld(t, r)
		if _, err := r.CreateAuthorization(proxySuccessAuthReq(r, "a1", proxySuccessPrice)); err != nil {
			t.Fatal(err)
		}
		req := proxyReq("bob", "a1", "p-rev")

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
		assertProxySuccessStillPending(t, r, req)

		if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
			t.Fatal(err)
		}
		restoreBatchSave(t, r)
		// 授权人在恢复期间撤销授权（新增撤销记录，占授权历史序号 2）。
		if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
			Operator: "alice", Reason: "改主意", RequestID: "rv-1", AuthID: "a1",
		}); err != nil {
			t.Fatal(err)
		}

		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationRevoked) {
			t.Fatalf("授权撤销后重提应返回 ErrAuthorizationRevoked: %v", err)
		}
		if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationRevoked) ||
			res.ItemID != "" || res.TxSeq != 0 {
			t.Fatalf("已撤销拒绝的重提结果异常: %+v", res)
		}
		assertProxyRequestSavedRejected(t, r, req, ErrAuthorizationRevoked)
		assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
		if a, _ := r.GetAuthorization("a1"); a.Status != AuthRevoked || a.UsedTxSeq != 0 {
			t.Fatalf("已撤销拒绝不应把授权用掉: %+v", a)
		}
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrAuthorizationRevoked) || !res.Replayed {
			t.Fatalf("保存后的重提应回放已撤销拒绝: %+v, err %v", res, err)
		}
	})

	t.Run("version_changed", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		base := time.Unix(1_700_000_000, 0)
		r.now = func() time.Time { return base }
		seedProxySuccessWorld(t, r)
		if _, err := r.CreateAuthorization(proxySuccessAuthReq(r, "a1", proxySuccessPrice)); err != nil {
			t.Fatal(err)
		}
		req := proxyReq("bob", "a1", "p-conflict")

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
		assertProxySuccessStillPending(t, r, req)

		// 恢复读写后，alice 先把 i1 直接合法转让给 bob：i1 变为 bob 版本 2，
		// 占用接续的藏品历史序号 2（证明未保存代转没有消耗序号）。
		if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
			t.Fatal(err)
		}
		restoreBatchSave(t, r)
		if _, err := r.Transfer(TransferRequest{
			Operator: "alice", Reason: "恢复期间直接转让", RequestID: "t-direct", ItemID: "i1",
			ExpectedOwner: "alice", ExpectedVer: 1, ToID: "bob", Price: 5000,
		}); err != nil {
			t.Fatal(err)
		}

		// 授权绑定 alice 版本 1，当前 bob 版本 2：版本冲突，不是回放未保存成功。
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("持有版本变化后重提应返回 ErrConflict: %v", err)
		}
		if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrConflict) ||
			res.ItemID != "" || res.TxSeq != 0 {
			t.Fatalf("版本冲突拒绝的重提结果异常: %+v", res)
		}
		assertProxyRequestSavedRejected(t, r, req, ErrConflict)
		h, _ := r.GetHolding("i1")
		if h.OwnerID != "bob" || h.Version != 2 {
			t.Fatalf("冲突拒绝不应改变持有: %+v", h)
		}
		if a, _ := r.GetAuthorization("a1"); a.Status != AuthActive || a.UsedTxSeq != 0 {
			t.Fatalf("冲突拒绝不应使用授权: %+v", a)
		}
		// 直接转让占序号 2，冲突拒绝不再消耗；授权历史仍只有创建一条。
		if r.state.NextSeq != 2 || r.state.NextAuthSeq != 1 {
			t.Fatalf("序号异常: NextSeq=%d（应 2） NextAuthSeq=%d（应 1）",
				r.state.NextSeq, r.state.NextAuthSeq)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
			t.Fatalf("冲突拒绝不应新增授权使用记录: %+v", evs)
		}
		res, err = r.ProxyTransfer(req)
		if !errors.Is(err, ErrConflict) || !res.Replayed {
			t.Fatalf("保存后的重提应回放版本冲突拒绝: %+v, err %v", res, err)
		}
	})
}

// TestProxyTransferSuccessSaveFailureZeroPrice 覆盖授权约定零价款的代转在保存
// 失败时同样整体不留痕；恢复后重提成功一次：价款 0、零金额应付明细仍保留、
// 余款 0 归授权人。
func TestProxyTransferSuccessSaveFailureZeroPrice(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	seedProxySuccessWorld(t, r)
	if _, err := r.CreateAuthorization(proxySuccessAuthReq(r, "a1", 0)); err != nil {
		t.Fatal(err)
	}
	req := proxyReq("bob", "a1", "p-zero")

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
	assertProxySuccessStillPending(t, r, req)

	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(req)
	if err != nil || res.Replayed || res.AuthID != "a1" || res.ItemID != "i1" ||
		res.Version != 2 || res.TxSeq != 2 || res.Price != 0 || res.Remainder != 0 {
		t.Fatalf("零价款代转重提结果异常: %+v, err %v", res, err)
	}
	wantPayables := []RoyaltyPayable{
		{AccountID: "carol", Rate: 1000, Amount: 0},
		{AccountID: "dave", Rate: 500, Amount: 0},
	}
	if !reflect.DeepEqual(res.Payables, wantPayables) {
		t.Fatalf("零价款代转应付 = %+v, want %+v", res.Payables, wantPayables)
	}
	tr, _ := r.TransferRoyalty(2)
	if tr.Price != 0 || tr.Remainder != 0 || tr.OwnerID != "alice" ||
		!reflect.DeepEqual(tr.Payables, wantPayables) {
		t.Fatalf("零价款代转版税记录异常: %+v", tr)
	}

	// 再提回放，不第二次执行。
	replay, err := r.ProxyTransfer(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 2 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
	if hist := mustHistory(t, r, "i1"); len(hist) != 2 {
		t.Fatalf("回放不应新增历史: %+v", hist)
	}
}
