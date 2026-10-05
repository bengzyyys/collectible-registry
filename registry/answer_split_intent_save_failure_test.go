package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为答复拆分意向补充"保存失败"场景的回归保障，与创建拆分意向
// （split_intent_save_failure_test.go）及发行、转让、创建代转授权的同类
// 用例行为对齐：参与账户对一个已保存、尚未结束且仍有效的意向首次提交
// 同意或拒绝时，这次答复是否生效始终以保存完成为准。新答复尚未写入原
// 登记册就发生保存错误时，必须返回实际保存错误——不能返回成功，也不能
// 用随后读取数据的错误替代它；结果为空（无意向编号、无状态、业务错误
// 为空、不标回放），请求号与意向历史序号都不被这次未保存的答复消耗。
// 即使失败后原数据暂时无法读取、状态未能按磁盘重建，同一个仍打开的
// 登记册中各方此前已保存的答复、份额与意向结束时间仍保持原样，意向状态
// 继续按原有答复、当前持有与账户状态及当前时间判断（不能因这次失败提前
// 变为已达成或已拒绝），意向历史不出现这次答复，后续其他操作成功保存也
// 不把这次未保存的答复或终结状态带入登记册。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// assertAnswerSaveFailureEmpty 核对答复"保存失败"的返回：error 是保存
// 错误而非成功或任何业务拒绝；结果为空——无意向编号、无状态，业务错误
// 为空，也不标记为重复返回。
func assertAnswerSaveFailureEmpty(t *testing.T, res AnswerSplitIntentResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.IntentID != "" || res.Status != "" || res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须返回空结果: %+v", res)
	}
}

// assertAnswerRequestFree 核对请求号未被这次未保存的答复占用、意向历史
// 序号未被消耗（在同一个已打开的登记册上检查）。
func assertAnswerRequestFree(t *testing.T, r *Registry, req AnswerSplitIntentRequest, wantNextIntentSeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的答复不应占用请求号 %s", req.RequestID)
	}
	if r.state.NextIntentSeq != wantNextIntentSeq {
		t.Fatalf("NextIntentSeq = %d, want %d", r.state.NextIntentSeq, wantNextIntentSeq)
	}
}

// assertIntentParties 按账户核对意向各方的当前答复，并核对当前状态与
// 结束时间。
func assertIntentParties(t *testing.T, r *Registry, intentID, status string,
	wantAnswers map[string]string, wantEnded bool) {
	t.Helper()
	in, err := r.GetSplitIntent(intentID)
	if err != nil {
		t.Fatalf("GetSplitIntent %s: %v", intentID, err)
	}
	if in.Status != status {
		t.Fatalf("意向 %s 状态 = %s, want %s: %+v", intentID, in.Status, status, in)
	}
	if in.EndedAt.IsZero() == wantEnded {
		t.Fatalf("意向 %s 结束时间异常: %+v", intentID, in)
	}
	got := make(map[string]string, len(in.Shares))
	for _, p := range in.Shares {
		got[p.AccountID] = p.Answer
	}
	for acct, want := range wantAnswers {
		if got[acct] != want {
			t.Fatalf("账户 %s 的答复 = %q, want %q（各方答复 %v）", acct, got[acct], want, got)
		}
	}
}

// answerSaveFailureWorld 建立已保存的意向 it1：alice 持有 i1 版本 1，
// 方案为 alice 50%（创建即同意）、bob 30%、carol 20%，一小时后到期。
func answerSaveFailureWorld(t *testing.T, r *Registry) {
	t.Helper()
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
}

// TestAnswerSplitIntentLastAgreeSaveFailure 覆盖题述核心场景之一：只剩一名
// 参与者尚未答复时，其同意保存失败，方案仍应等待该参与者确认——不提前
// 变为已达成；失败不占用请求号、不消耗意向历史序号。保存恢复后其他操作
// 成功保存不夹带这次答复；原请求重提首次成功不标回放，此后相同请求回放
// 首次结果，重复相同答复不新增历史，已答复后不能改答。
func TestAnswerSplitIntentLastAgreeSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	// bob 先同意（已保存）：意向仍待确认，历史为 create + bob 答复，序号 2。
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob", true)); err != nil {
		t.Fatal(err)
	}
	req := answerReq("carol", "it1", "ans-carol", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 2)
	// 方案仍等待 carol 确认：carol 未答复，状态仍为待确认；bob 的已保存
	// 答复与历史记录保持原样。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": "",
	}, false)
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 2 || evs[1].Seq != 2 || evs[1].Operator != "bob" {
		t.Fatalf("失败后意向历史应保持此前 2 条: %+v, %v", evs, err)
	}
	// 答复不改变藏品持有人或持有版本。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 保存条件未恢复时再次提交同一请求：仍重新尝试保存并失败，不能把上次
	// 未保存的答复当成已保存回放。
	res, err = r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 2)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"carol": ""}, false)

	// 保存条件恢复后，其他操作成功保存也不能把这次未保存的答复带入登记册。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertAnswerRequestFree(t, r, req, 2)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"carol": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("其他操作保存后意向历史仍应是 2 条: %+v", evs)
	}

	// 用原请求号和相同内容重提：按当前状态重新处理，carol 同意后全部同意，
	// 首次重新成功不标记为回放。
	res, err = r.AnswerSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常生效: %v", err)
	}
	if res.Replayed || res.Err != nil || res.IntentID != "it1" || res.Status != SplitAgreed {
		t.Fatalf("重提结果异常: %+v", res)
	}
	assertIntentParties(t, r, "it1", SplitAgreed, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": SplitAnswerAgree,
	}, false)
	evs, _ = r.SplitIntentHistory("i1")
	if len(evs) != 3 || evs[2].Seq != 3 || evs[2].Operator != "carol" ||
		evs[2].FromStatus != SplitPending || evs[2].ToStatus != SplitAgreed {
		t.Fatalf("carol 答复历史异常: %+v", evs)
	}
	if r.state.NextIntentSeq != 3 {
		t.Fatalf("NextIntentSeq = %d, want 3", r.state.NextIntentSeq)
	}

	// 真正保存成功后，相同请求回放首次结果。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || !res.Replayed || res.IntentID != "it1" || res.Status != SplitAgreed {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
	// 重复相同答复（新请求号）成功但不新增历史。
	res, err = r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol-again", true))
	if err != nil || res.Replayed || res.Status != SplitAgreed {
		t.Fatalf("重复相同答复应成功: %+v, err %v", res, err)
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 3 {
		t.Fatalf("重复相同答复不应新增历史: %+v", evs)
	}
	// 已答复后不能改答。
	if _, err := r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol-change", false)); !errors.Is(err, ErrSplitAnswered) {
		t.Fatalf("改答应为 ErrSplitAnswered: %v", err)
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 3 {
		t.Fatalf("改答拒绝不应新增历史: %+v", evs)
	}
	// 答复始终不改变持有。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)
}

// TestAnswerSplitIntentLastAgreeSaveFailureUnreadableDisk 覆盖：保存失败且
// 原数据暂时无法读取（commit 无法按磁盘重建状态）时，未保存的答复、终结
// 状态、意向历史与请求号占用也不能留在当前登记册中；恢复正常读写后，
// 其他操作成功保存不带入这次未保存的内容，原请求重提正常生效。
func TestAnswerSplitIntentLastAgreeSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob", true)); err != nil {
		t.Fatal(err)
	}
	req := answerReq("carol", "it1", "ans-carol", true)

	// 让保存失败，同时让原数据暂时无法读取：commit 无法按磁盘重建状态。
	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	// 状态未能重建时也必须显式撤销：同一个仍打开的登记册中 carol 仍未
	// 答复，意向仍待确认，历史保持此前 2 条，请求号与序号未被消耗。
	assertAnswerRequestFree(t, r, req, 2)
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

	// 其他操作成功保存不能把这次未保存的答复带入登记册。
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertAnswerRequestFree(t, r, req, 2)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"carol": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("其他操作保存后意向历史仍应是 2 条: %+v", evs)
	}

	// 原请求重提首次成功，不标回放。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || res.Replayed || res.Status != SplitAgreed {
		t.Fatalf("恢复后重提应正常生效: %+v, err %v", res, err)
	}
	if r.state.NextIntentSeq != 3 {
		t.Fatalf("NextIntentSeq = %d, want 3", r.state.NextIntentSeq)
	}
}

// TestAnswerSplitIntentRejectSaveFailure 覆盖题述核心场景之二：拒绝保存
// 失败不能把方案终结为已拒绝，也不能因此允许持有人另建一份同时有效的
// 方案。保存恢复后其他操作不夹带终结状态；原请求重提首次成功才把方案
// 终结为已拒绝（不标回放），此后相同请求回放，已拒绝后才能另建新方案。
func TestAnswerSplitIntentRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	req := answerReq("bob", "it1", "ans-bob", false)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)
	// 方案没有被终结：bob 未答复，状态仍待确认、无结束时间，历史只有创建。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": "", "carol": "",
	}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("失败后意向历史应只有创建记录: %+v", evs)
	}

	// 保存条件恢复后，其他操作成功保存不能夹带这次未保存的拒绝或终结状态。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": ""}, false)
	// 失败没有终结方案：它仍是一份同时有效的待确认方案，持有人不能另建。
	blockedNew := createIntentReq(r, "it2")
	blockedNew.RequestID = "create-it2-blocked"
	if _, err := r.CreateSplitIntent(blockedNew); !errors.Is(err, ErrConflict) {
		t.Fatalf("未保存的拒绝不应终结方案，另建应仍冲突: %v", err)
	}

	// 原请求重提：拒绝这次真正生效，首次重新成功不标回放。
	res, err = r.AnswerSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常生效: %v", err)
	}
	if res.Replayed || res.Err != nil || res.IntentID != "it1" || res.Status != SplitRejected {
		t.Fatalf("重提结果异常: %+v", res)
	}
	assertIntentParties(t, r, "it1", SplitRejected, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerReject, "carol": "",
	}, true)
	evs, _ := r.SplitIntentHistory("i1")
	if len(evs) != 2 || evs[1].Seq != 2 || evs[1].Operator != "bob" ||
		evs[1].Answer != SplitAnswerReject || evs[1].ToStatus != SplitRejected {
		t.Fatalf("bob 拒绝历史异常: %+v", evs)
	}

	// 真正保存成功后，相同请求回放首次结果：原拒绝答复是一次成功操作，回放
	// 返回其首次结果（状态已拒绝、不附业务错误），而不是对已拒绝意向再答复
	// 的哨兵错误。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || !res.Replayed || res.IntentID != "it1" ||
		res.Status != SplitRejected || res.Err != nil {
		t.Fatalf("成功后重提应回放首次结果: %+v, err %v", res, err)
	}
	// 已拒绝是终态，此后持有人才能用新编号另建方案。
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it2")); err != nil {
		t.Fatalf("已拒绝后应允许另建新方案: %v", err)
	}
}

// TestAnswerSplitIntentRejectSaveFailureUnreadableDisk 覆盖题述拒绝示例的
// 最严情形：拒绝保存失败且原数据暂时无法读取（状态未能按磁盘重建）时，
// 方案不能被终结为已拒绝——无结束时间、历史无答复、请求号与序号未消耗；
// 恢复正常读写后其他操作成功保存不夹带终结状态，且因方案仍有效而不能另建，
// 原请求重提首次成功才把方案终结为已拒绝。
func TestAnswerSplitIntentRejectSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	req := answerReq("bob", "it1", "ans-bob", false)

	orig, err := os.ReadFile(dataFile(r.dir))
	if err != nil {
		t.Fatal(err)
	}
	blockBatchSave(t, r)
	if err := os.WriteFile(dataFile(r.dir), []byte("{corrupt"), fileMode); err != nil {
		t.Fatal(err)
	}

	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)
	// 没有被终结为已拒绝：bob 未答复、无结束时间、派生状态仍待确认。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": "", "carol": "",
	}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("不可读磁盘拒绝失败后历史应只有创建: %+v", evs)
	}

	// 恢复正常读写，另一笔正常操作成功保存。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	// 未保存的拒绝、终结状态与历史都不能被夹带。
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("其他操作保存后历史仍应只有创建: %+v", evs)
	}
	// 方案仍同时有效：持有人不能另建新方案。
	blockedNew := createIntentReq(r, "it2")
	blockedNew.RequestID = "create-it2-blocked"
	if _, err := r.CreateSplitIntent(blockedNew); !errors.Is(err, ErrConflict) {
		t.Fatalf("未保存的拒绝不应终结方案，另建应仍冲突: %v", err)
	}

	// 原请求重提首次成功才终结为已拒绝，不标回放。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || res.Replayed || res.Status != SplitRejected {
		t.Fatalf("恢复后重提应正常终结方案: %+v, err %v", res, err)
	}
	assertIntentParties(t, r, "it1", SplitRejected, map[string]string{
		"bob": SplitAnswerReject,
	}, true)
}

// TestAnswerSplitIntentAgreeSaveFailureRetryExpired 覆盖：同意保存失败后
// 意向到期的，用原请求号和相同内容重提应沿用现有的过期拒绝，而不是回放
// 未保存的同意或强行把方案变为已达成。
func TestAnswerSplitIntentAgreeSaveFailureRetryExpired(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	base := time.Unix(2_100_000_000, 0)
	r.now = func() time.Time { return base }
	defer func() { r.now = time.Now }()
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	req := answerReq("bob", "it1", "ans-bob", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": ""}, false)

	// 时间走到到期点：意向已过期，bob 仍未答复。
	r.now = func() time.Time { return base.Add(time.Hour) }
	assertIntentParties(t, r, "it1", SplitExpired, map[string]string{"bob": ""}, false)

	// 保存条件恢复，用原请求号、相同内容重提：按当时状态重新处理，沿用
	// 现有过期拒绝，不能把方案变为已达成。
	restoreBatchSave(t, r)
	res, err = r.AnswerSplitIntent(req)
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
}

// TestAnswerSplitIntentRejectSaveFailureRetryExpired 覆盖：拒绝保存失败后
// 意向到期的，用原请求号和相同内容重提应沿用现有的过期拒绝（首次重提
// 不标回放），此后相同请求回放该过期拒绝。
func TestAnswerSplitIntentRejectSaveFailureRetryExpired(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	base := time.Unix(2_100_000_000, 0)
	r.now = func() time.Time { return base }
	defer func() { r.now = time.Now }()
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	req := answerReq("bob", "it1", "ans-bob", false)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": ""}, false)

	// 时间走到到期点：意向已过期。
	r.now = func() time.Time { return base.Add(time.Hour) }
	assertIntentParties(t, r, "it1", SplitExpired, map[string]string{"bob": ""}, false)

	// 保存条件恢复，用原请求号、相同内容重提：按当时状态重新处理，沿用
	// 现有过期拒绝，不回放未保存的拒绝。
	restoreBatchSave(t, r)
	res, err = r.AnswerSplitIntent(req)
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
	res, err = r.AnswerSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentExpired) || !res.Replayed ||
		!errors.Is(res.Err, ErrSplitIntentExpired) {
		t.Fatalf("保存后的再次提交应回放过期拒绝: %+v, err %v", res, err)
	}
}

// TestAnswerSplitIntentRejectSaveFailureRetryInvalid 覆盖：拒绝保存失败后
// 持有版本发生变化（藏品被合法转出）使意向失效的，原请求重提沿用现有的
// 失效拒绝，不回放未保存的拒绝，也不把方案终结为已拒绝。
func TestAnswerSplitIntentRejectSaveFailureRetryInvalid(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	req := answerReq("bob", "it1", "ans-bob", false)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)

	// 保存恢复后，alice 把 i1 合法转给 dave：持有版本变为 2，意向失效。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(xferReq("alice", "i1", "dave", 1, "rt-move")); err != nil {
		t.Fatal(err)
	}
	assertIntentParties(t, r, "it1", SplitInvalid, map[string]string{"bob": ""}, false)

	// 原请求重提：按当时状态重新处理，沿用失效拒绝，首次不标回放。
	res, err = r.AnswerSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentInvalid) {
		t.Fatalf("失效后重提应返回 ErrSplitIntentInvalid: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentInvalid) {
		t.Fatalf("失效拒绝的重提结果异常: %+v", res)
	}
	// 方案没有被终结为已拒绝：当前派生状态为已失效，无结束时间，历史无答复。
	assertIntentParties(t, r, "it1", SplitInvalid, map[string]string{"bob": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("失效拒绝不应新增意向历史: %+v", evs)
	}

	// 该拒绝保存后，相同请求回放首次（失效）拒绝。
	res, err = r.AnswerSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentInvalid) || !res.Replayed ||
		!errors.Is(res.Err, ErrSplitIntentInvalid) {
		t.Fatalf("保存后的再次提交应回放失效拒绝: %+v, err %v", res, err)
	}
}

// TestAnswerSplitIntentStatusRejectSaveFailure 覆盖：名单外账户无权答复与
// 已答复后改答这两类状态类业务拒绝，在拒绝结果自身落盘失败时也必须返回
// 实际保存错误、结果为空、不占用请求号；保存恢复后原请求重提重新保存
// 对应业务拒绝（首次不标回放），再次提交才回放。
func TestAnswerSplitIntentStatusRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	// bob 已保存同意。
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob", true)); err != nil {
		t.Fatal(err)
	}
	outsideReq := answerReq("dave", "it1", "ans-dave-outside", true)
	changeReq := answerReq("bob", "it1", "ans-bob-change", false)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(outsideReq)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, outsideReq, 2)
	res, err = r.AnswerSplitIntent(changeReq)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, changeReq, 2)
	// bob 此前已保存的同意保持原样，意向仍待确认。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": "",
	}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("拒绝保存失败不应新增历史: %+v", evs)
	}

	restoreBatchSave(t, r)
	// 原请求重提：重新保存对应业务拒绝，首次不标回放。
	res, err = r.AnswerSplitIntent(outsideReq)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("重提应返回 ErrForbidden: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("无权拒绝的重提结果异常: %+v", res)
	}
	res, err = r.AnswerSplitIntent(changeReq)
	if !errors.Is(err, ErrSplitAnswered) {
		t.Fatalf("重提应返回 ErrSplitAnswered: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitAnswered) {
		t.Fatalf("改答拒绝的重提结果异常: %+v", res)
	}
	// 再次提交相同请求才回放已保存的拒绝。
	res, err = r.AnswerSplitIntent(outsideReq)
	if !errors.Is(err, ErrForbidden) || !res.Replayed || !errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("无权拒绝应被回放: %+v, err %v", res, err)
	}
	res, err = r.AnswerSplitIntent(changeReq)
	if !errors.Is(err, ErrSplitAnswered) || !res.Replayed || !errors.Is(res.Err, ErrSplitAnswered) {
		t.Fatalf("改答拒绝应被回放: %+v, err %v", res, err)
	}
	// 业务拒绝不新增意向历史，bob 的答复也未被改变。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": SplitAnswerAgree}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("业务拒绝后意向历史仍应是 2 条: %+v", evs)
	}
}

// TestAnswerSplitIntentEndedRejectSaveFailure 覆盖：对已拒绝意向的答复本应
// 返回 ErrSplitIntentRejected，但这次拒绝结果保存失败时返回实际保存错误、
// 结果为空、不占用请求号；恢复后重提重新保存该拒绝。
func TestAnswerSplitIntentEndedRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	// carol 拒绝，方案终结为已拒绝（已保存）。
	if _, err := r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol", false)); err != nil {
		t.Fatal(err)
	}
	req := answerReq("bob", "it1", "ans-bob-after-reject", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 2)

	restoreBatchSave(t, r)
	res, err = r.AnswerSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentRejected) {
		t.Fatalf("重提应返回 ErrSplitIntentRejected: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentRejected) {
		t.Fatalf("已拒绝意向答复的重提结果异常: %+v", res)
	}
	res, err = r.AnswerSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentRejected) || !res.Replayed {
		t.Fatalf("再次提交应回放已保存的拒绝: %+v, err %v", res, err)
	}
}

// TestAnswerSplitIntentRepeatSaveFailure 覆盖：重复相同答复只登记请求结果、
// 不新增变更记录；该请求结果保存失败时同样返回实际保存错误、结果为空、
// 不占用请求号，恢复后重提正常返回当前状态（不标回放），此后回放。
func TestAnswerSplitIntentRepeatSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob", true)); err != nil {
		t.Fatal(err)
	}
	req := answerReq("bob", "it1", "ans-bob-repeat", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 2)
	// 重复答复本就不新增历史，失败后历史仍是 create + bob 首次答复。
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("重复答复保存失败后历史应仍是 2 条: %+v", evs)
	}

	restoreBatchSave(t, r)
	// carol 尚未答复：重提重复答复返回待确认，首次不标回放，不新增历史。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || res.Replayed || res.IntentID != "it1" || res.Status != SplitPending {
		t.Fatalf("恢复后重提应返回当前状态: %+v, err %v", res, err)
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("重复答复重提不应新增历史: %+v", evs)
	}
	// 保存成功后相同请求回放首次结果。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || !res.Replayed || res.Status != SplitPending {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
}

// TestAnswerSplitIntentSaveFailureAfterReopen 覆盖磁盘视角：原数据可读时
// 保存失败后关闭重开，看到的仍是答复前状态——未保存的答复不存在、请求号
// 未占用；恢复保存后用原请求重提正常生效。
func TestAnswerSplitIntentSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	answerSaveFailureWorld(t, r)
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob", true)); err != nil {
		t.Fatal(err)
	}
	req := answerReq("carol", "it1", "ans-carol", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)

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
		t.Fatal("重开后未保存的答复不应占用请求号")
	}
	if r2.state.NextIntentSeq != 2 {
		t.Fatalf("重开后 NextIntentSeq = %d, want 2", r2.state.NextIntentSeq)
	}
	if evs, _ := r2.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("重开后历史应仍是 2 条: %+v", evs)
	}

	restoreBatchSave(t, r2)
	res2, err := r2.AnswerSplitIntent(req)
	if err != nil || res2.Replayed || res2.Status != SplitAgreed {
		t.Fatalf("重开后重提应正常生效: %+v, err %v", res2, err)
	}
	res2, err = r2.AnswerSplitIntent(req)
	if err != nil || !res2.Replayed || res2.Status != SplitAgreed {
		t.Fatalf("成功后重提应回放: %+v, err %v", res2, err)
	}
}
