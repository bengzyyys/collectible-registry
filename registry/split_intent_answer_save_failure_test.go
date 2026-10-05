package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为答复拆分意向补充"保存失败"场景的回归保障，与创建拆分意向、
// 单件/整批发行、单件/整批转让、创建代转授权（split_intent/issue/transfer/
// batch/batch_transfer/authorization_save_failure_test.go）行为对齐：
// 参与账户首次提交同意或拒绝时，答复是否生效以保存完成为准。新答复尚未
// 写入原登记册而保存失败时，必须返回实际保存错误——不能报告答复成功，
// 也不能用随后读取数据的错误替代；结果为空（意向编号与状态为空、不标
// 回放、业务错误为空）。同一个仍打开的登记册中，各参与账户此前已保存的
// 答复、份额与意向结束时间保持原样，意向状态继续按原有答复、当前持有与
// 账户状态以及当前时间判断，不提前变为已达成或已拒绝；意向历史不出现
// 这次未保存的答复，请求号与意向历史序号都不被消耗，即使失败后原数据
// 暂时无法读取、状态未能按磁盘重建也一样。保存条件恢复后，后续其他操作
// 成功保存不能夹带这次未保存的答复、终结状态或历史；用完全相同的请求
// 重提按当时的意向状态重新处理（首次重提不标回放），意向已过期或已失效
// 时沿用对应的拒绝。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// assertAnswerSaveFailureEmpty 核对"保存失败"的返回：error 是保存错误而
// 非答复成功或任何业务拒绝；结果为空——不携带意向编号或状态，业务错误
// 为空，也不标记为重复返回。
func assertAnswerSaveFailureEmpty(t *testing.T, res AnswerSplitIntentResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	if res.IntentID != "" || res.Status != "" || res.Replayed || res.Err != nil {
		t.Fatalf("保存失败必须返回空结果: %+v", res)
	}
}

// assertAnswerRequestFree 核对答复请求号未被这次未保存的操作占用、意向
// 历史序号未被消耗（在同一个已打开的登记册上检查）。
func assertAnswerRequestFree(t *testing.T, r *Registry, req AnswerSplitIntentRequest, wantNextIntentSeq int64) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的答复不应占用请求号 %s", req.RequestID)
	}
	if r.state.NextIntentSeq != wantNextIntentSeq {
		t.Fatalf("NextIntentSeq = %d, want %d", r.state.NextIntentSeq, wantNextIntentSeq)
	}
}

// assertIntentPending 核对意向仍为待确认：bob 未答复、无结束时间，意向
// 历史只有创建一条记录。
func assertIntentPending(t *testing.T, r *Registry, intentID string) {
	t.Helper()
	it, err := r.GetSplitIntent(intentID)
	if err != nil {
		t.Fatalf("GetSplitIntent %s: %v", intentID, err)
	}
	if it.Status != SplitPending {
		t.Fatalf("未保存的答复不应改变意向状态: %+v", it)
	}
	if !it.EndedAt.IsZero() {
		t.Fatalf("未保存的答复不应产生结束时间: %+v", it)
	}
	for _, p := range it.Shares {
		want := ""
		if p.AccountID == "alice" {
			want = SplitAnswerAgree // 发起人列入方案，创建即视为已同意
		}
		if p.Answer != want {
			t.Fatalf("参与账户 %s 的答复应为 %q，实际 %q", p.AccountID, want, p.Answer)
		}
	}
	evs, err := r.SplitIntentHistory(it.ItemID)
	if err != nil {
		t.Fatalf("SplitIntentHistory %s: %v", it.ItemID, err)
	}
	if len(evs) != 1 || evs[0].Seq != 1 || evs[0].Kind != "create" {
		t.Fatalf("意向历史应只有创建一条: %+v", evs)
	}
}

// TestAnswerSplitIntentAgreeSaveFailure 覆盖：首次同意落盘失败时返回保存
// 错误、结果为空；同一个仍打开的登记册中意向仍为待确认、bob 未答复、
// 历史只有创建记录、请求号与意向历史序号都未被消耗。保存条件恢复后，其他
// 操作成功保存不会把这次未保存的答复带入登记册；用完全相同的请求重提正常
// 记录答复（不标回放），此后相同请求回放首次结果，重复相同答复不新增历史，
// 已答复后不能改答。
func TestAnswerSplitIntentAgreeSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	req := answerReq("bob", "it1", "ans-bob-1", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentPending(t, r, "it1")

	// 保存条件未恢复时再次提交同一请求：仍重新尝试保存并失败，不能把上次
	// 未保存的答复当成已保存回放。
	res, err = r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)

	// 保存条件恢复后，其他操作成功保存也不能把这次未保存的答复或答复记录
	// 带入登记册。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentPending(t, r, "it1")

	// 用完全相同的请求重提：正常记录答复，不标回放；carol 未答复，仍为
	// 待确认。
	res, err = r.AnswerSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常记录答复: %v", err)
	}
	if res.Replayed || res.Err != nil || res.IntentID != "it1" || res.Status != SplitPending {
		t.Fatalf("重提结果异常: %+v", res)
	}
	it, err := r.GetSplitIntent("it1")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != SplitPending {
		t.Fatalf("carol 未答复，意向应为待确认: %+v", it)
	}
	for _, p := range it.Shares {
		if p.AccountID == "bob" && p.Answer != SplitAnswerAgree {
			t.Fatalf("bob 的答复未保存: %+v", it)
		}
	}
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 2 || evs[1].Seq != 2 || evs[1].Kind != "answer" ||
		evs[1].Operator != "bob" || evs[1].Answer != SplitAnswerAgree {
		t.Fatalf("意向历史异常: %+v, %v", evs, err)
	}

	// 答复保存后，相同请求回放首次结果。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || !res.Replayed || res.IntentID != "it1" || res.Status != SplitPending {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}

	// 重复相同答复（新请求号）成功但不新增历史；改答拒绝。
	res, err = r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob-2", true))
	if err != nil || res.Replayed || res.Status != SplitPending {
		t.Fatalf("重复相同答复异常: %+v, err %v", res, err)
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("重复相同答复不应新增历史: %+v", evs)
	}
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob-3", false)); !errors.Is(err, ErrSplitAnswered) {
		t.Fatalf("已答复后改答应拒绝: %v", err)
	}
}

// TestAnswerSplitIntentAgreeSaveFailureUnreadableDisk 覆盖：保存失败且原
// 数据暂时无法读取（状态未能按磁盘重建）时，未保存的答复、答复记录与请求
// 号占用也不能留在当前登记册中；恢复正常读写后，其他操作成功保存不带入
// 这次未保存的内容，原请求重提正常记录答复。
func TestAnswerSplitIntentAgreeSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	req := answerReq("bob", "it1", "ans-bob-1", true)

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
	// 状态未能重建时也必须显式撤销：同一个仍打开的登记册中意向仍为待
	// 确认、bob 未答复，请求号与意向历史序号都未被消耗。
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentPending(t, r, "it1")

	// 恢复正常读写。
	if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
		t.Fatal(err)
	}
	restoreBatchSave(t, r)

	// 其他操作成功保存不能把这次未保存的答复或请求号占用带入登记册。
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentPending(t, r, "it1")

	// 用完全相同的请求重提：正常记录答复，不标回放。
	res, err = r.AnswerSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常记录答复: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || res.Status != SplitPending {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if r.state.NextIntentSeq != 2 {
		t.Fatalf("意向历史序号应连续: NextIntentSeq = %d", r.state.NextIntentSeq)
	}
}

// TestAnswerSplitIntentRejectSaveFailure 覆盖：首次拒绝落盘失败时返回保存
// 错误、结果为空；意向不能因此被终结为已拒绝（无结束时间、仍为待确认），
// 持有人也不能因此另建一份同时有效的方案。保存条件恢复后用完全相同的请求
// 重提，正常终结为已拒绝（不标回放）；此后相同请求回放首次结果。
func TestAnswerSplitIntentRejectSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	req := answerReq("bob", "it1", "ans-bob-1", false)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)
	assertIntentPending(t, r, "it1")

	// 保存条件恢复后，其他操作成功保存不能把意向夹带为已拒绝；未保存的
	// 拒绝也不能终结意向——持有人另建方案仍按状态冲突拒绝。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertIntentPending(t, r, "it1")
	newReq := createIntentReq(r, "it2")
	if _, err := r.CreateSplitIntent(newReq); !errors.Is(err, ErrConflict) {
		t.Fatalf("原意向仍有效，另建方案应冲突拒绝: %v", err)
	}

	// 用完全相同的请求重提：正常记录拒绝，意向终结为已拒绝，不标回放。
	res, err = r.AnswerSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常记录拒绝: %v", err)
	}
	if res.Replayed || res.Err != nil || res.IntentID != "it1" || res.Status != SplitRejected {
		t.Fatalf("重提结果异常: %+v", res)
	}
	it, err := r.GetSplitIntent("it1")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != SplitRejected || it.EndedAt.IsZero() {
		t.Fatalf("意向应已拒绝并有结束时间: %+v", it)
	}
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 2 || evs[1].Kind != "answer" ||
		evs[1].Answer != SplitAnswerReject || evs[1].ToStatus != SplitRejected {
		t.Fatalf("意向历史异常: %+v, %v", evs, err)
	}

	// 拒绝保存后，相同请求回放首次结果；已拒绝的意向不再接受新答复。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || !res.Replayed || res.Status != SplitRejected {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
	if _, err := r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol-1", true)); !errors.Is(err, ErrSplitIntentRejected) {
		t.Fatalf("已拒绝的意向不应再接受答复: %v", err)
	}
	// 已拒绝是终态，持有人可用新编号另建方案。
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it3")); err != nil {
		t.Fatalf("意向终结后应可另建方案: %v", err)
	}
}

// TestAnswerSplitIntentLastAgreeSaveFailure 覆盖：只剩一名参与者尚未答复
// 时，其同意落盘失败不能把方案显示为已达成——意向仍为待确认、等待该参与
// 者确认；保存条件恢复后重提才变为已达成。
func TestAnswerSplitIntentLastAgreeSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	// bob 先同意并保存成功，只剩 carol 未答复。
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob-1", true)); err != nil {
		t.Fatal(err)
	}
	req := answerReq("carol", "it1", "ans-carol-1", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 2)

	// 方案仍应等待 carol 确认：状态为待确认，bob 已保存的同意保持原样，
	// carol 未答复，历史只有创建与 bob 的答复两条。
	it, err := r.GetSplitIntent("it1")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != SplitPending {
		t.Fatalf("carol 的同意未保存，意向不应变为已达成: %+v", it)
	}
	for _, p := range it.Shares {
		switch p.AccountID {
		case "alice", "bob":
			if p.Answer != SplitAnswerAgree {
				t.Fatalf("%s 已保存的同意被改变: %+v", p.AccountID, it)
			}
		case "carol":
			if p.Answer != "" {
				t.Fatalf("carol 未保存的同意不应留下: %+v", it)
			}
		}
	}
	evs, err := r.SplitIntentHistory("i1")
	if err != nil || len(evs) != 2 || evs[1].Operator != "bob" {
		t.Fatalf("意向历史不应出现 carol 未保存的答复: %+v, %v", evs, err)
	}

	// 保存条件恢复后重提：正常记录，全部同意，意向变为已达成，不标回放。
	restoreBatchSave(t, r)
	res, err = r.AnswerSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常记录答复: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || res.Status != SplitAgreed {
		t.Fatalf("重提结果异常: %+v", res)
	}
	if it, _ := r.GetSplitIntent("it1"); it.Status != SplitAgreed {
		t.Fatalf("全部同意后意向应为已达成: %+v", it)
	}
	if r.state.NextIntentSeq != 3 {
		t.Fatalf("意向历史序号应连续: NextIntentSeq = %d", r.state.NextIntentSeq)
	}
}

// TestAnswerSplitIntentSaveFailureRetryAfterExpiry 覆盖：答复落盘失败后
// 意向已过期时，用完全相同的请求重提沿用现有的过期拒绝（保存成功后返回
// 业务错误，不标回放）；此后相同请求回放该拒绝。
func TestAnswerSplitIntentSaveFailureRetryAfterExpiry(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	base := r.now()
	r.now = func() time.Time { return base }
	defer func() { r.now = time.Now }()
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	req := answerReq("bob", "it1", "ans-bob-1", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 1)

	// 保存条件恢复，但意向已过期：重提按当时状态判断，沿用过期拒绝。
	restoreBatchSave(t, r)
	r.now = func() time.Time { return base.Add(2 * time.Hour) }
	res, err = r.AnswerSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentExpired) {
		t.Fatalf("意向已过期，重提应返回 ErrSplitIntentExpired: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentExpired) ||
		res.Status != "" {
		t.Fatalf("过期拒绝的重提结果异常: %+v", res)
	}
	// 拒绝不记录答复，意向仍为待确认的过期状态，历史只有创建一条。
	it, err := r.GetSplitIntent("it1")
	if err != nil {
		t.Fatal(err)
	}
	if it.Status != SplitExpired {
		t.Fatalf("意向应显示已过期: %+v", it)
	}
	for _, p := range it.Shares {
		if p.AccountID == "bob" && p.Answer != "" {
			t.Fatalf("过期拒绝不应记录答复: %+v", it)
		}
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("过期拒绝不应新增意向历史: %+v", evs)
	}

	// 拒绝已保存：相同请求重提回放该拒绝。
	res, err = r.AnswerSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentExpired) || !res.Replayed {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestAnswerSplitIntentDuplicateSaveFailure 覆盖：重复相同答复（成功但不
// 新增记录）落盘失败时同样返回保存错误、结果为空、请求号不被占用；保存
// 条件恢复后重提正常成功（不标回放），仍不新增意向历史。
func TestAnswerSplitIntentDuplicateSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	intentWorld(t, r)
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob-1", true)); err != nil {
		t.Fatal(err)
	}
	// bob 用新请求号重复相同的同意。
	req := answerReq("bob", "it1", "ans-bob-2", true)

	blockBatchSave(t, r)
	res, err := r.AnswerSplitIntent(req)
	assertAnswerSaveFailureEmpty(t, res, err)
	assertAnswerRequestFree(t, r, req, 2)

	restoreBatchSave(t, r)
	res, err = r.AnswerSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常成功: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || res.Status != SplitPending {
		t.Fatalf("重复答复的重提结果异常: %+v", res)
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("重复相同答复不应新增历史: %+v", evs)
	}
	// 请求结果保存后，相同请求回放。
	res, err = r.AnswerSplitIntent(req)
	if err != nil || !res.Replayed || res.Status != SplitPending {
		t.Fatalf("保存后的重提应回放: %+v, err %v", res, err)
	}
}
