package registry

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

// 本文件为限时一次性代转（ProxyTransfer）补充“成功代转本身保存失败”场景的
// 回归保障。既有的 proxy_transfer_save_failure_test.go 覆盖的是状态类业务
// 拒绝的落盘失败；这里覆盖的是受托人通过 ProxyTransfer 使用仍有效的授权、
// 账户与持有版本等业务条件全部满足，但本次代转的持有换人/版本/历史/版税
// 应付/授权使用状态/请求结果尚未原子替换原数据就发生写入失败的情形——这样的
// 代转实际没有保存，绝不能成为登记册认可的交易，即使原数据在失败后暂时无法
// 读取或解析。
//
// 失败返回必须保留实际写入错误并给出空结果（不带授权或藏品编号、前后持有人、
// 版本、历史序号、价款、应付或余款，业务错误为空，不标回放）。同一个仍打开
// 的登记册上：藏品继续属于提交前持有人、版本及双方持有列表不变、藏品历史没有
// 本次转让，授权没有使用时间或关联转让序号、授权历史没有本次使用记录，版税
// 计算记录与收款账户应付也不多出这一笔；请求号、藏品历史序号与授权历史序号
// 都不被这次失败消耗，失败前已保存的交易、授权及应付保持原样。读写恢复后，
// 即使用户先登记一个无关账户并成功保存，也不能把这笔未保存代转夹带落盘；用
// 原请求号、原因和授权编号重提按当时状态重新处理——条件仍满足就真正完成一次
// （版本只加一次、记录接续原有序号、价款取授权约定、余款归授权人、首次保存
// 成功不标回放），此后原样重提才回放；授权已到期、撤销或持有版本已变化则沿用
// 对应拒绝，不能回放先前未保存的成功，也不因恢复状态而延长授权有效期。
//
// 失败注入与其他场景共用：在 .registry.json.tmp 上预建目录，save 在
// OpenFile 阶段即以 EISDIR 失败（rename 之前）；部分用例同时把数据文件改写
// 为无法解析的内容，覆盖“原数据暂时无法读取、commit 无法按磁盘重建状态”。

// seedProxySuccessWorld 建立一个带版税规则的代转世界：alice 是 s1 的创建
// 账户与 i1 的初始持有人（版本 1，发行历史序号 1），s1 设 carol 10% 版税；
// alice 为 i1 创建授权 a1，受托人为 bob、固定接收人为 carol、价款 10000 分、
// 到期点 base+1h，绑定 alice 版本 1。因此成功代转将占用藏品历史序号 2 与授权
// 历史序号 2：i1 转给 carol、版本 2，carol 应付 1000、余款 9000 归授权人
// alice。时间固定在 base，授权处于 active。
func seedProxySuccessWorld(t *testing.T, r *Registry) time.Time {
	t.Helper()
	for _, id := range []string{"alice", "bob", "carol"} {
		if err := r.RegisterAccount(id, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.CreateSeries("s1", "alice", "系列"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SetRoyalty(SetRoyaltyRequest{
		Operator: "alice", Reason: "设置版税", RequestID: "rr-s1", SeriesID: "s1",
		Shares: []RoyaltyShare{{AccountID: "carol", Rate: 1000}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Issue(IssueRequest{
		Operator: "alice", Reason: "首发", RequestID: "ri-i1",
		ItemID: "i1", SeriesID: "s1", BatchNo: "b1", Metadata: "元-i1", HolderID: "alice",
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 0)
	r.now = func() time.Time { return base }
	if _, err := r.CreateAuthorization(CreateAuthorizationRequest{
		Operator: "alice", Reason: "委托代转", RequestID: "create-a1",
		AuthID: "a1", ItemID: "i1", TrusteeID: "bob", ToID: "carol",
		ExpectedOwner: "alice", ExpectedVer: 1, ExpiresAt: base.Add(time.Hour), Price: 10000,
	}); err != nil {
		t.Fatal(err)
	}
	return base
}

// assertProxySuccessStillPending 在（成功代转保存失败后的）同一个已打开登记册
// 上核对：这笔代转如同从未发生，而失败前已有的内容完整保留。rid 为本次未保存
// 代转的请求号（操作者为 bob）。
func assertProxySuccessStillPending(t *testing.T, r *Registry, rid string) {
	t.Helper()
	// 藏品仍属提交前持有人 alice、版本没有增加，历史只有发行一条，且不出现
	// 本次请求号。
	h, err := r.GetHolding("i1")
	if err != nil {
		t.Fatalf("GetHolding i1: %v", err)
	}
	if h.OwnerID != "alice" || h.Version != 1 {
		t.Fatalf("保存失败后 i1 持有被改变: %+v", h)
	}
	hist, err := r.History("i1")
	if err != nil {
		t.Fatalf("History i1: %v", err)
	}
	if len(hist) != 1 || hist[0].Kind != "issue" {
		t.Fatalf("保存失败后 i1 历史多出本次代转: %+v", hist)
	}
	for _, e := range hist {
		if e.RequestID == rid {
			t.Fatalf("保存失败后 i1 历史出现请求号 %s: %+v", rid, e)
		}
	}

	// 双方持有列表不变：alice 仍持有 i1，受托人 bob 与接收人 carol 都没有持有。
	aliceHoldings, err := r.HoldingsOf("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceHoldings) != 1 || aliceHoldings[0].ItemID != "i1" || aliceHoldings[0].Version != 1 {
		t.Fatalf("保存失败后 alice 持有清单异常: %+v", aliceHoldings)
	}
	if bobHoldings, err := r.HoldingsOf("bob"); err != nil || len(bobHoldings) != 0 {
		t.Fatalf("保存失败后 bob 不应持有任何藏品: %+v, err %v", bobHoldings, err)
	}
	if carolHoldings, err := r.HoldingsOf("carol"); err != nil || len(carolHoldings) != 0 {
		t.Fatalf("保存失败后 carol 不应因未保存代转而持有: %+v, err %v", carolHoldings, err)
	}

	// 授权没有使用时间或关联转让序号，状态仍为 active；授权历史只有创建一条。
	a, err := r.GetAuthorization("a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != AuthActive || a.UsedTxSeq != 0 || a.UsedAt != (time.Time{}) {
		t.Fatalf("保存失败后授权不应被消耗: %+v", a)
	}
	evs, err := r.AuthorizationHistory("i1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("保存失败后授权历史不应出现本次使用记录: %+v", evs)
	}

	// 两类历史序号都不被消耗；请求号空闲；版税计算记录与收款账户应付不多出。
	if r.state.NextSeq != 1 {
		t.Fatalf("NextSeq = %d，未保存代转不应消耗藏品历史序号，仍应为 1", r.state.NextSeq)
	}
	if r.state.NextAuthSeq != 1 {
		t.Fatalf("NextAuthSeq = %d，未保存代转不应消耗授权历史序号，仍应为 1", r.state.NextAuthSeq)
	}
	if _, ok := r.state.Requests[requestKey("bob", rid)]; ok {
		t.Fatalf("未保存代转不应占用请求号 %s", rid)
	}
	if _, err := r.TransferRoyalty(2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("未保存代转不应留下序号 2 的版税计算记录: %v", err)
	}
	if pay, err := r.PayablesOf("carol"); err != nil || len(pay) != 0 {
		t.Fatalf("未保存代转不应新增收款账户应付: %+v, err %v", pay, err)
	}
}

// wantProxySuccessPayables 是成功代转 i1（价款 10000、carol 10%）的应付。
var wantProxySuccessPayables = []RoyaltyPayable{{AccountID: "carol", Rate: 1000, Amount: 1000}}

// TestProxyTransferSuccessSaveFailureUnreadableSameRegistry 覆盖核心场景：
// 业务条件全部满足后的成功代转在写入阶段失败，且原数据同时被改写为无法解析
// （commit 无法按磁盘重建状态）。调用返回保存错误与空结果；同一登记册上代转
// 如同从未发生。保存仍失败时重提依旧失败、不留痕。读写恢复后先登记一个无关
// 账户成功保存也不夹带；随后原请求重提按当前状态真正完成一次（非回放、序号 2、
// 价款取授权约定、余款归授权人），再提才回放且不第二次换人。
func TestProxyTransferSuccessSaveFailureUnreadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedProxySuccessWorld(t, r)
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
	assertProxySuccessStillPending(t, r, req.RequestID)

	// 保存条件未恢复、磁盘仍不可读时再次提交同一请求：仍失败在保存上，不能把
	// 上次未保存的成功当成已保存结果回放。
	res, err = r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxySuccessStillPending(t, r, req.RequestID)

	// 恢复正常读写；先让一次无关操作（登记新账户）成功保存，不能夹带这笔未
	// 保存的代转。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatalf("无关操作应能成功保存: %v", err)
	}
	assertProxySuccessStillPending(t, r, req.RequestID)

	// 受托人用原请求号、原因和授权编号重提：条件仍满足，真正完成一次，不标
	// 回放，版本只加一次，记录接续原有序号（2），价款取授权约定、余款归授权人。
	res, err = r.ProxyTransfer(req)
	if err != nil {
		t.Fatalf("恢复后重提应真正完成一次代转: %v", err)
	}
	if res.Replayed || res.Err != nil || res.AuthID != "a1" || res.ItemID != "i1" ||
		res.FromID != "alice" || res.ToID != "carol" || res.Version != 2 || res.TxSeq != 2 ||
		res.Price != 10000 || res.Remainder != 9000 ||
		!reflect.DeepEqual(res.Payables, wantProxySuccessPayables) {
		t.Fatalf("重提结果异常: %+v", res)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("重提成功后 i1 持有 = %+v", h)
	}
	if hist, _ := r.History("i1"); len(hist) != 2 || hist[1].Seq != 2 ||
		hist[1].Kind != "transfer" || hist[1].AuthID != "a1" ||
		hist[1].RequestID != req.RequestID || hist[1].FromID != "alice" || hist[1].ToID != "carol" {
		t.Fatalf("重提成功后 i1 历史异常: %+v", hist)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthUsed || a.UsedTxSeq != 2 || a.UsedAt == (time.Time{}) {
		t.Fatalf("重提成功后授权应记为本次已使用: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 ||
		evs[1].Kind != "use" || evs[1].AuthID != "a1" || evs[1].Seq != 2 || evs[1].TxSeq != 2 {
		t.Fatalf("重提成功后授权使用记录异常: %+v", evs)
	}
	if r.state.NextSeq != 2 || r.state.NextAuthSeq != 2 {
		t.Fatalf("序号应只增加一次: NextSeq=%d NextAuthSeq=%d", r.state.NextSeq, r.state.NextAuthSeq)
	}
	tr, _ := r.TransferRoyalty(2)
	if tr.ItemID != "i1" || tr.Price != 10000 || tr.Remainder != 9000 || tr.OwnerID != "alice" ||
		!reflect.DeepEqual(tr.Payables, wantProxySuccessPayables) {
		t.Fatalf("本次代转版税记录异常（余款应归授权人 alice）: %+v", tr)
	}
	if pay, _ := r.PayablesOf("carol"); len(pay) != 1 || pay[0].TxSeq != 2 || pay[0].Amount != 1000 {
		t.Fatalf("收款账户应只多出本次一笔应付: %+v", pay)
	}

	// 只有此次成功保存后再原样重提，才回放这一笔已保存结果，不再第二次换人、
	// 不重复计金额、不新增历史或授权使用记录。
	replay, err := r.ProxyTransfer(req)
	if err != nil || !replay.Replayed || replay.Version != 2 || replay.TxSeq != 2 ||
		replay.Price != 10000 || replay.Remainder != 9000 ||
		!reflect.DeepEqual(replay.Payables, wantProxySuccessPayables) {
		t.Fatalf("成功保存后再提应回放已保存结果: %+v, err %v", replay, err)
	}
	h, _ = r.GetHolding("i1")
	if h.OwnerID != "carol" || h.Version != 2 {
		t.Fatalf("回放不应再次改变持有: %+v", h)
	}
	if hist, _ := r.History("i1"); len(hist) != 2 {
		t.Fatalf("回放不应新增藏品历史: %+v", hist)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 {
		t.Fatalf("回放不应新增授权使用记录: %+v", evs)
	}
	if pay, _ := r.PayablesOf("carol"); len(pay) != 1 {
		t.Fatalf("回放不应重复计入应付: %+v", pay)
	}
}

// TestProxyTransferSuccessSaveFailureReadableSameRegistry 覆盖：写入失败但原
// 数据仍可正常读取（commit 据磁盘内容重建状态）时，得到相同的失败结果与状态
// 保障——同一登记册上代转从未发生；恢复后原请求重提真正完成一次，再提才回放。
func TestProxyTransferSuccessSaveFailureReadableSameRegistry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedProxySuccessWorld(t, r)
	req := proxyReq("bob", "a1", "p-ok-r")

	// 只阻断写入，不破坏数据文件：rename 之前失败，原快照完好可读。
	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)
	assertProxySuccessStillPending(t, r, req.RequestID)

	restoreBatchSave(t, r)
	res, err = r.ProxyTransfer(req)
	if err != nil || res.Replayed || res.AuthID != "a1" ||
		res.FromID != "alice" || res.ToID != "carol" || res.Version != 2 || res.TxSeq != 2 ||
		res.Price != 10000 || res.Remainder != 9000 {
		t.Fatalf("可读磁盘失败恢复后重提应真正完成一次: %+v, err %v", res, err)
	}
	a, _ := r.GetAuthorization("a1")
	if a.Status != AuthUsed || a.UsedTxSeq != 2 {
		t.Fatalf("重提后授权应已使用: %+v", a)
	}
	// 成功保存后再提才回放。
	replay, err := r.ProxyTransfer(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 2 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestProxyTransferSuccessSaveFailureAfterReopen 覆盖磁盘视角：成功代转保存
// 失败（原数据可读）后关闭重开，看到的仍是代转前状态；恢复保存后用原请求重提
// 真正完成一次，而不是回放一个从未保存的成功。
func TestProxyTransferSuccessSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	base := seedProxySuccessWorld(t, r)
	req := proxyReq("bob", "a1", "p-ok-reopen")

	blockBatchSave(t, r)
	res, err := r.ProxyTransfer(req)
	assertProxySaveFailureEmpty(t, res, err)

	// 数据文件从未被替换：正常关闭并重新打开仍读到代转前状态。重开后沿用固定
	// 时钟，避免真实时间越过授权到期点影响“授权未使用”的核对。
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	r2.now = func() time.Time { return base }
	assertProxySuccessStillPending(t, r2, req.RequestID)

	restoreBatchSave(t, r2)
	res2, err := r2.ProxyTransfer(req)
	if err != nil || res2.Replayed || res2.TxSeq != 2 || res2.Version != 2 ||
		res2.FromID != "alice" || res2.ToID != "carol" {
		t.Fatalf("重开后重提应真正完成一次: %+v, err %v", res2, err)
	}
	replay, err := r2.ProxyTransfer(req)
	if err != nil || !replay.Replayed || replay.TxSeq != 2 {
		t.Fatalf("成功后再提应回放: %+v, err %v", replay, err)
	}
}

// TestProxyTransferSuccessSaveFailureRetryExpired 覆盖：恢复期间授权已到期后，
// 原请求重提必须返回到期拒绝，不能回放那次未保存的成功，也不因状态恢复而延长
// 授权有效期；该到期拒绝首次保存不标回放，再提才回放。
func TestProxyTransferSuccessSaveFailureRetryExpired(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	base := seedProxySuccessWorld(t, r)
	req := proxyReq("bob", "a1", "p-ok-exp")

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
	assertProxySuccessStillPending(t, r, req.RequestID)

	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	// 越过到期点：授权从到期时间点起不可用。
	r.now = func() time.Time { return base.Add(2 * time.Hour) }

	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) || res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationExpired) ||
		res.ItemID != "" || res.TxSeq != 0 {
		t.Fatalf("授权到期后重提应返回未回放的到期拒绝，不能回放未保存成功: %+v, err %v", res, err)
	}
	// 到期拒绝不换人、不消耗授权与序号。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthExpired || a.UsedTxSeq != 0 {
		t.Fatalf("到期拒绝不应消耗授权: %+v", a)
	}
	if r.state.NextSeq != 1 || r.state.NextAuthSeq != 1 {
		t.Fatalf("到期拒绝不应消耗序号: NextSeq=%d NextAuthSeq=%d", r.state.NextSeq, r.state.NextAuthSeq)
	}
	// 此后原样重提回放已保存的到期拒绝。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationExpired) || !res.Replayed ||
		!errors.Is(res.Err, ErrAuthorizationExpired) {
		t.Fatalf("到期拒绝保存后再提应回放: %+v, err %v", res, err)
	}
}

// TestProxyTransferSuccessSaveFailureRetryRevoked 覆盖：恢复期间授权人撤销授权
// 后，原请求重提返回已撤销拒绝，不能回放未保存的成功；首次保存不标回放，再提
// 才回放。
func TestProxyTransferSuccessSaveFailureRetryRevoked(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedProxySuccessWorld(t, r)
	req := proxyReq("bob", "a1", "p-ok-rv")

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
	assertProxySuccessStillPending(t, r, req.RequestID)

	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	// 授权人在恢复后撤销仍未使用的授权。
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{
		Operator: "alice", Reason: "恢复后撤销", RequestID: "rv-1", AuthID: "a1",
	}); err != nil {
		t.Fatal(err)
	}

	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) || res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrAuthorizationRevoked) ||
		res.ItemID != "" || res.TxSeq != 0 {
		t.Fatalf("授权撤销后重提应返回未回放的已撤销拒绝: %+v, err %v", res, err)
	}
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthRevoked || a.UsedTxSeq != 0 {
		t.Fatalf("已撤销拒绝不应消耗授权: %+v", a)
	}
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrAuthorizationRevoked) || !res.Replayed {
		t.Fatalf("已撤销拒绝保存后再提应回放: %+v, err %v", res, err)
	}
}

// TestProxyTransferSuccessSaveFailureRetryHoldingChanged 覆盖：恢复期间藏品被
// 持有人另一笔合法直接转让转出后，原代转请求必须按持有版本冲突规则拒绝，不能
// 回放那次未保存的成功；冲突拒绝不换人、不消耗授权或序号，保存后再提回放。
func TestProxyTransferSuccessSaveFailureRetryHoldingChanged(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	seedProxySuccessWorld(t, r)
	req := proxyReq("bob", "a1", "p-ok-cf")

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
	assertProxySuccessStillPending(t, r, req.RequestID)

	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	// 恢复期间 alice 把 i1 合法直接转给 dave：i1 变为 dave 版本 2（序号 2），
	// 授权绑定的 alice 版本 1 已不匹配。
	if _, err := r.Transfer(TransferRequest{
		Operator: "alice", Reason: "恢复期间的合法转让", RequestID: "rt-intervene", ItemID: "i1",
		ExpectedOwner: "alice", ExpectedVer: 1, ToID: "dave", Price: 5000,
	}); err != nil {
		t.Fatal(err)
	}

	// 原代转请求按当前持有状态应被版本冲突拒绝，而不是回放未保存的成功。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrConflict) || res.Replayed ||
		res.AuthID != "a1" || !errors.Is(res.Err, ErrConflict) ||
		res.ItemID != "" || res.TxSeq != 0 {
		t.Fatalf("持有版本已变化后重提应返回未回放的冲突拒绝: %+v, err %v", res, err)
	}
	h, _ := r.GetHolding("i1")
	if h.OwnerID != "dave" || h.Version != 2 {
		t.Fatalf("冲突拒绝不应改变持有: %+v", h)
	}
	if a, _ := r.GetAuthorization("a1"); a.Status != AuthActive || a.UsedTxSeq != 0 {
		t.Fatalf("冲突拒绝不应消耗授权: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
		t.Fatalf("冲突拒绝不应新增授权使用记录: %+v", evs)
	}
	// 插入的直接转让占用序号 2；冲突拒绝不再消耗任何序号。
	if r.state.NextSeq != 2 || r.state.NextAuthSeq != 1 {
		t.Fatalf("冲突拒绝不应消耗序号: NextSeq=%d NextAuthSeq=%d", r.state.NextSeq, r.state.NextAuthSeq)
	}
	// 此后原样重提回放已保存的冲突拒绝，仍不执行代转。
	res, err = r.ProxyTransfer(req)
	if !errors.Is(err, ErrConflict) || !res.Replayed || !errors.Is(res.Err, ErrConflict) {
		t.Fatalf("冲突拒绝保存后再提应回放: %+v, err %v", res, err)
	}
	h, _ = r.GetHolding("i1")
	if h.OwnerID != "dave" || h.Version != 2 {
		t.Fatalf("回放冲突拒绝不应改变持有: %+v", h)
	}
}
