package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为撤回拆分意向补充"保存失败"场景的回归保障，与创建、答复拆分意向
// （split_intent_save_failure_test.go、answer_split_intent_save_failure_
// test.go）及发行、转让、创建代转授权的同类用例行为对齐：发起人撤回仍有效
// 的待确认或已达成意向时，撤回是否生效始终以保存完成为准。撤回结果尚未写入
// 原登记册就发生保存错误时，必须返回实际保存错误（保留底层写入错误）——不
// 能返回成功，也不能用业务拒绝或随后读取数据的错误替代；结果为空（无意向
// 编号、无状态、业务错误为空、不标回放），请求号与意向历史序号都不被这次
// 未保存的撤回消耗。对已撤回意向的再次撤回，以及无权撤回、已拒绝、已失效、
// 已过期等需要保存拒绝结果的撤回同样如此。即使失败后原数据暂时无法读取、
// 状态未能按磁盘重建，同一个仍打开的登记册中原方案、各方此前的答复、创建
// 与结束时间保持操作前的内容，意向历史不出现这次撤回，意向状态继续按已
// 保存的内容、当前持有与账户状态及当前时间判断：仍有效的方案继续接受原本
// 允许的答复，并继续阻止另一份有效方案的创建；后续其他操作成功保存也不把
// 这次未保存的撤回、记录或请求结果带入登记册。保存条件恢复后用原请求号和
// 完全相同的内容重提，按当时的意向状态重新处理（意向仍有效则正常撤回，
// 期间已到期或失效则返回原有对应拒绝），首次真正保存成功或保存拒绝后，
// 相同请求才回放该结果。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// assertWithdrawSaveFailureEmpty 核对撤回"保存失败"的返回：error 是保存
// 错误而非成功或任何业务拒绝；结果为空——无意向编号、无状态，业务错误
// 为空，也不标记为重复返回。
func assertWithdrawSaveFailureEmpty(t *testing.T, res WithdrawSplitIntentResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.IntentID != "" || res.Status != "" || res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须返回空结果: %+v", res)
	}
}

// assertWithdrawRequestFree 核对请求号未被这次未保存的撤回占用、意向历史
// 序号未被消耗（在同一个已打开的登记册上检查）。
func assertWithdrawRequestFree(t *testing.T, r *Registry, req WithdrawSplitIntentRequest, wantNextIntentSeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的撤回不应占用请求号 %s", req.RequestID)
	}
	if r.state.NextIntentSeq != wantNextIntentSeq {
		t.Fatalf("NextIntentSeq = %d, want %d", r.state.NextIntentSeq, wantNextIntentSeq)
	}
}

// withdrawSaveFailureWorld 建立已保存的意向 it1：alice 持有 i1 版本 1，
// 方案为 alice 50%（创建即同意）、bob 30%、carol 20%，一小时后到期；
// bob 已保存同意，历史为 create + bob 答复，序号 2。
func withdrawSaveFailureWorld(t *testing.T, r *Registry) {
	t.Helper()
	answerSaveFailureWorld(t, r)
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob", true)); err != nil {
		t.Fatal(err)
	}
}

// TestWithdrawSplitIntentSuccessSaveFailure 覆盖：撤回的成功结果落盘失败时
// 返回保存错误、结果为空；同一个仍打开的登记册中意向不显示为已撤回——方案、
// 各方此前答复与创建时间保持原样、无结束时间，历史不增加撤回记录，请求号与
// 意向历史序号不被消耗；仍有效的方案继续接受原本允许的答复，并继续阻止另建
// 有效方案。保存恢复后其他操作成功保存不夹带这次撤回；原请求重提首次成功
// 不标回放，此后相同请求回放首次结果，重复撤回不新增历史。
func TestWithdrawSplitIntentSuccessSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	withdrawSaveFailureWorld(t, r)
	req := withdrawReq("alice", "it1", "wd-1")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 2)
	// 意向不显示为已撤回：方案与各方答复保持原样、无结束时间，历史保持
	// create + bob 答复两条。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": "",
	}, false)
	if in, _ := r.GetSplitIntent("it1"); in.CreatedAt.IsZero() {
		t.Fatalf("创建时间应保持操作前内容: %+v", in)
	}
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 2 || evs[1].Seq != 2 || evs[1].Kind != "answer" {
		t.Fatalf("失败后意向历史应保持此前 2 条: %+v, %v", evs, err)
	}
	// 撤回不改变藏品持有人或持有版本。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 保存条件未恢复时再次提交同一请求：仍重新尝试保存并失败，不能把上次
	// 未保存的撤回当成已保存回放。
	res, err = r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 2)

	// 保存条件恢复后，其他操作成功保存不能把这次未保存的撤回或撤回记录
	// 带入登记册。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertWithdrawRequestFree(t, r, req, 2)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"carol": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("其他操作保存后意向历史仍应是 2 条: %+v", evs)
	}

	// 方案仍有效：参与账户继续答复原本允许的份额。
	if res, err := r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol", true)); err != nil ||
		res.Status != SplitAgreed {
		t.Fatalf("方案仍有效，参与账户应能继续答复: %+v, %v", res, err)
	}
	// 并继续阻止另一份有效方案的创建。
	blockedNew := createIntentReq(r, "it2")
	blockedNew.RequestID = "create-it2-blocked"
	if _, err := r.CreateSplitIntent(blockedNew); !errors.Is(err, ErrConflict) {
		t.Fatalf("未保存的撤回不应终结方案，另建应仍冲突: %v", err)
	}

	// 用原请求号和完全相同的内容重提：按当前状态重新处理，正常撤回（此时
	// 意向已达成），首次重新成功不标回放。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常撤回: %v", err)
	}
	if res.Replayed || res.Err != nil || res.IntentID != "it1" || res.Status != SplitWithdrawn {
		t.Fatalf("重提结果异常: %+v", res)
	}
	assertIntentParties(t, r, "it1", SplitWithdrawn, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": SplitAnswerAgree,
	}, true)
	evs, _ = r.SplitIntentHistory("i1")
	if len(evs) != 4 || evs[3].Seq != 4 || evs[3].Kind != "withdraw" ||
		evs[3].RequestID != "wd-1" || evs[3].FromStatus != SplitAgreed ||
		evs[3].ToStatus != SplitWithdrawn {
		t.Fatalf("撤回历史异常: %+v", evs)
	}
	if r.state.NextIntentSeq != 4 {
		t.Fatalf("NextIntentSeq = %d, want 4", r.state.NextIntentSeq)
	}

	// 真正保存成功后，相同请求回放首次结果。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || !res.Replayed || res.IntentID != "it1" || res.Status != SplitWithdrawn {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
	// 已撤回后再次撤回（新请求号）成功但不新增历史。
	res, err = r.WithdrawSplitIntent(withdrawReq("alice", "it1", "wd-2"))
	if err != nil || res.Replayed || res.Status != SplitWithdrawn {
		t.Fatalf("重复撤回应成功: %+v, err %v", res, err)
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 4 {
		t.Fatalf("重复撤回不应新增历史: %+v", evs)
	}
	// 撤回始终不改变持有。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
}

// TestWithdrawSplitIntentSuccessSaveFailureUnreadableDisk 覆盖：保存失败且
// 原数据暂时无法读取（commit 无法按磁盘重建状态）时，未保存的撤回、终结
// 状态、撤回记录与请求号占用也不能留在当前登记册中；恢复正常读写后，其他
// 操作成功保存不带入这次未保存的内容，原请求重提正常撤回。
func TestWithdrawSplitIntentSuccessSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	withdrawSaveFailureWorld(t, r)
	req := withdrawReq("alice", "it1", "wd-1")

	// 让保存失败，同时让原数据暂时无法读取：commit 无法按磁盘重建状态。
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	// 状态未能重建时也必须显式撤销：同一个仍打开的登记册中意向不显示为
	// 已撤回，历史保持此前 2 条，请求号与序号未被消耗。
	assertWithdrawRequestFree(t, r, req, 2)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": "",
	}, false)
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 2 || evs[1].Seq != 2 || evs[1].Operator != "bob" {
		t.Fatalf("不可读磁盘失败后历史应保持此前 2 条且顺序不变: %+v, %v", evs, err)
	}

	// 恢复正常读写。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	// 其他操作成功保存不能把这次未保存的撤回带入登记册。
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertWithdrawRequestFree(t, r, req, 2)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"carol": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("其他操作保存后意向历史仍应是 2 条: %+v", evs)
	}

	// 原请求重提首次成功，不标回放。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || res.Replayed || res.Status != SplitWithdrawn {
		t.Fatalf("恢复后重提应正常撤回: %+v, err %v", res, err)
	}
	if r.state.NextIntentSeq != 3 {
		t.Fatalf("NextIntentSeq = %d, want 3", r.state.NextIntentSeq)
	}
}

// TestWithdrawSplitIntentForbiddenSaveFailure 覆盖：非发起人撤回的无权拒绝
// 落盘失败时返回实际保存错误、结果为空、不占用请求号；保存恢复后原请求重提
// 重新保存该拒绝并返回 ErrForbidden（首次不标回放），再次提交才回放。
func TestWithdrawSplitIntentForbiddenSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	withdrawSaveFailureWorld(t, r)
	req := withdrawReq("bob", "it1", "wd-bob")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 2)
	// 无权拒绝的保存失败不影响意向：仍待确认，历史保持 2 条。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"carol": ""}, false)

	restoreBatchSave(t, r)
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("重提应在保存成功后返回 ErrForbidden: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrForbidden) ||
		res.Status != "" {
		t.Fatalf("无权拒绝的重提结果异常: %+v", res)
	}
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; !ok {
		t.Fatal("拒绝保存成功后应登记请求结果")
	}
	// 拒绝不终结意向，也不消耗意向历史序号。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"carol": ""}, false)
	if r.state.NextIntentSeq != 2 {
		t.Fatalf("拒绝不应消耗意向历史序号: NextIntentSeq = %d", r.state.NextIntentSeq)
	}

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrForbidden) || !res.Replayed || !errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestWithdrawSplitIntentEndedRejectSaveFailure 覆盖：对已拒绝意向的撤回
// （ErrSplitIntentRejected）与已失效意向的撤回（ErrSplitIntentInvalid）在
// 拒绝结果落盘失败时同样返回实际保存错误、结果为空、不占用请求号；恢复后
// 重提重新保存对应拒绝（首次不标回放），再次提交才回放。
func TestWithdrawSplitIntentEndedRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	withdrawSaveFailureWorld(t, r)
	// carol 拒绝，方案终结为已拒绝（已保存）。
	if _, err := r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol", false)); err != nil {
		t.Fatal(err)
	}
	rejectedReq := withdrawReq("alice", "it1", "wd-rejected")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(rejectedReq)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, rejectedReq, 3)

	restoreBatchSave(t, r)
	res, err = r.WithdrawSplitIntent(rejectedReq)
	if !errors.Is(err, ErrSplitIntentRejected) {
		t.Fatalf("重提应返回 ErrSplitIntentRejected: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentRejected) {
		t.Fatalf("已拒绝意向撤回的重提结果异常: %+v", res)
	}
	res, err = r.WithdrawSplitIntent(rejectedReq)
	if !errors.Is(err, ErrSplitIntentRejected) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}

	// 另建一份意向后让持有版本变化使其失效，失效拒绝的保存失败同样处理。
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it2")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Transfer(xferReq("alice", "i1", "dave", 1, "rt-move")); err != nil {
		t.Fatal(err)
	}
	invalidReq := withdrawReq("alice", "it2", "wd-invalid")

	blockBatchSave(t, r)
	res, err = r.WithdrawSplitIntent(invalidReq)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, invalidReq, 4)

	restoreBatchSave(t, r)
	res, err = r.WithdrawSplitIntent(invalidReq)
	if !errors.Is(err, ErrSplitIntentInvalid) {
		t.Fatalf("重提应返回 ErrSplitIntentInvalid: %v", err)
	}
	if res.Replayed || res.IntentID != "it2" || !errors.Is(res.Err, ErrSplitIntentInvalid) {
		t.Fatalf("已失效意向撤回的重提结果异常: %+v", res)
	}
	res, err = r.WithdrawSplitIntent(invalidReq)
	if !errors.Is(err, ErrSplitIntentInvalid) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的失效拒绝: %+v, err %v", res, err)
	}
}

// TestWithdrawSplitIntentRepeatSaveFailure 覆盖：对已撤回意向的再次撤回只
// 登记请求结果、不新增变更记录；该请求结果保存失败时同样返回实际保存错误、
// 结果为空、不占用请求号，恢复后重提正常返回已撤回（不标回放），此后回放。
func TestWithdrawSplitIntentRepeatSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	withdrawSaveFailureWorld(t, r)
	if _, err := r.WithdrawSplitIntent(withdrawReq("alice", "it1", "wd-1")); err != nil {
		t.Fatal(err)
	}
	req := withdrawReq("alice", "it1", "wd-again")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 3)
	// 重复撤回本就不新增历史，失败后历史仍是 create + bob 答复 + 撤回。
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 3 {
		t.Fatalf("重复撤回保存失败后历史应仍是 3 条: %+v", evs)
	}

	restoreBatchSave(t, r)
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || res.Replayed || res.IntentID != "it1" || res.Status != SplitWithdrawn {
		t.Fatalf("恢复后重提应返回已撤回: %+v, err %v", res, err)
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 3 {
		t.Fatalf("重复撤回重提不应新增历史: %+v", evs)
	}
	// 保存成功后相同请求回放首次结果。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || !res.Replayed || res.Status != SplitWithdrawn {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
}

// TestWithdrawSplitIntentSaveFailureRetryExpired 覆盖：撤回保存失败后意向
// 到期的，用原请求号和相同内容重提应沿用现有的过期拒绝（首次不标回放），
// 不回放未保存的撤回，也不把方案显示为已撤回；该拒绝保存后相同请求才回放。
func TestWithdrawSplitIntentSaveFailureRetryExpired(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	base := time.Unix(2_100_000_000, 0)
	r.now = func() time.Time { return base }
	defer func() { r.now = time.Now }()
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	req := withdrawReq("alice", "it1", "wd-1")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": ""}, false)

	// 时间走到到期点：意向已过期，未被撤回。
	r.now = func() time.Time { return base.Add(time.Hour) }
	assertIntentParties(t, r, "it1", SplitExpired, map[string]string{"bob": ""}, false)

	// 保存条件恢复，用原请求号、相同内容重提：按当时状态重新处理，沿用
	// 现有过期拒绝，不能把方案显示为已撤回。
	restoreBatchSave(t, r)
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentExpired) {
		t.Fatalf("过期后重提应返回 ErrSplitIntentExpired: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentExpired) ||
		res.Status != "" {
		t.Fatalf("过期拒绝的重提结果异常: %+v", res)
	}
	assertIntentParties(t, r, "it1", SplitExpired, map[string]string{"bob": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("过期拒绝不应新增意向历史: %+v", evs)
	}

	// 该拒绝保存后，相同请求回放首次（过期）拒绝。
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentExpired) || !res.Replayed ||
		!errors.Is(res.Err, ErrSplitIntentExpired) {
		t.Fatalf("保存后的再次提交应回放过期拒绝: %+v, err %v", res, err)
	}
}

// TestWithdrawSplitIntentSaveFailureRetryInvalid 覆盖：撤回保存失败后持有
// 版本变化（藏品被合法转出）使意向失效的，原请求重提沿用现有的失效拒绝，
// 不回放未保存的撤回，也不把方案显示为已撤回。
func TestWithdrawSplitIntentSaveFailureRetryInvalid(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	withdrawSaveFailureWorld(t, r)
	req := withdrawReq("alice", "it1", "wd-1")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 2)

	// 保存恢复后，alice 把 i1 合法转给 dave：持有版本变为 2，意向失效。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(xferReq("alice", "i1", "dave", 1, "rt-move")); err != nil {
		t.Fatal(err)
	}
	assertIntentParties(t, r, "it1", SplitInvalid, map[string]string{"carol": ""}, false)

	// 原请求重提：按当时状态重新处理，沿用失效拒绝，首次不标回放。
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentInvalid) {
		t.Fatalf("失效后重提应返回 ErrSplitIntentInvalid: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentInvalid) {
		t.Fatalf("失效拒绝的重提结果异常: %+v", res)
	}
	// 方案没有被撤回：当前派生状态为已失效，无结束时间，历史无撤回记录。
	assertIntentParties(t, r, "it1", SplitInvalid, map[string]string{"carol": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("失效拒绝不应新增意向历史: %+v", evs)
	}

	// 该拒绝保存后，相同请求回放首次（失效）拒绝。
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentInvalid) || !res.Replayed ||
		!errors.Is(res.Err, ErrSplitIntentInvalid) {
		t.Fatalf("保存后的再次提交应回放失效拒绝: %+v, err %v", res, err)
	}
}

// TestWithdrawSplitIntentValidationErrorIgnoresSaveFailure 覆盖：必填内容
// 缺失与意向不存在的请求不占用请求号、不要求保存，即使数据位置暂时不可写
// 也仍返回原参数/引用错误，不能改报保存错误。
func TestWithdrawSplitIntentValidationErrorIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	withdrawSaveFailureWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 引用不存在的意向：返回 ErrNotFound，不是保存错误。
	notFoundReq := withdrawReq("alice", "it-nope", "wd-nf")
	res, err := r.WithdrawSplitIntent(notFoundReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.IntentID != "it-nope" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}
	assertWithdrawRequestFree(t, r, notFoundReq, 2)

	// 必填内容缺失：返回 ErrInvalidArgument，不是保存错误。
	missingReq := withdrawReq("alice", "", "wd-miss")
	if _, err := r.WithdrawSplitIntent(missingReq); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	assertWithdrawRequestFree(t, r, missingReq, 2)
}

// TestWithdrawSplitIntentSaveFailureAfterReopen 覆盖磁盘视角：原数据可读时
// 保存失败后关闭重开，看到的仍是撤回前状态——意向未撤回、请求号未占用；
// 恢复保存后用原请求重提正常撤回。
func TestWithdrawSplitIntentSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	withdrawSaveFailureWorld(t, r)
	req := withdrawReq("alice", "it1", "wd-1")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)

	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatalf("失败后原登记册必须仍能正常读取: %v", err)
	}
	t.Cleanup(func() { _ = r2.Close() })
	assertIntentParties(t, r2, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": "",
	}, false)
	if _, ok := r2.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("重开后未保存的撤回不应占用请求号")
	}
	if r2.state.NextIntentSeq != 2 {
		t.Fatalf("重开后 NextIntentSeq = %d, want 2", r2.state.NextIntentSeq)
	}
	if evs, _ := r2.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("重开后历史应仍是 2 条: %+v", evs)
	}

	restoreBatchSave(t, r2)
	res2, err := r2.WithdrawSplitIntent(req)
	if err != nil || res2.Replayed || res2.Status != SplitWithdrawn {
		t.Fatalf("重开后重提应正常撤回: %+v, err %v", res2, err)
	}
	res2, err = r2.WithdrawSplitIntent(req)
	if err != nil || !res2.Replayed || res2.Status != SplitWithdrawn {
		t.Fatalf("成功后重提应回放: %+v, err %v", res2, err)
	}
}
