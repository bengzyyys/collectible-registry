package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为撤回拆分意向补充"保存失败"场景的回归保障，与创建、答复拆分意向
// （split_intent_save_failure_test.go、answer_split_intent_save_failure_test.go）
// 及发行、转让、创建代转授权的同类用例行为对齐：发起人撤回待确认或已达成
// 的意向、对已撤回意向的幂等再次撤回，以及无权撤回、意向已拒绝、已失效、
// 已过期等需要保存拒绝结果的情形，是否生效都以结果成功保存为准。撤回结果
// 尚未写入原登记册就发生保存错误时，必须返回实际保存错误——不能返回撤回
// 成功，也不能只返回业务拒绝或用随后读取数据的错误替代它；结果为空
// （无意向编号、无状态、业务错误为空、不标回放），请求号与意向历史序号
// 都不被这次未保存的撤回消耗。即使失败后原数据暂时无法读取、状态未能按
// 磁盘重建，同一个仍打开的登记册中原方案、各方此前的答复、创建时间与
// 结束时间仍保持操作前内容：仍有效的方案继续按当前持有、账户状态与时间
// 显示并接受原本允许的答复，继续阻止另一份有效方案的创建，意向历史不
// 出现这次撤回；后续其他操作成功保存也不把这次未保存的撤回状态、记录或
// 请求结果带入登记册。
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

// assertIntentBlocksCreate 核对仍有效的意向继续阻止同一藏品另建方案。
func assertIntentBlocksCreate(t *testing.T, r *Registry) {
	t.Helper()
	blockedNew := createIntentReq(r, "it2")
	blockedNew.RequestID = "create-it2-blocked"
	if _, err := r.CreateSplitIntent(blockedNew); !errors.Is(err, ErrConflict) {
		t.Fatalf("方案仍有效时另建应冲突: %v", err)
	}
}

// TestWithdrawSplitIntentPendingSaveFailure 覆盖题述核心场景：待确认意向的
// 撤回保存失败后，方案仍按操作前内容保持有效——参与账户仍可答复、另建
// 方案仍被阻止、藏品持有不变；失败不占用请求号、不消耗意向历史序号。
// 保存恢复后其他操作成功保存不夹带这次撤回；原请求重提首次成功撤回（不标
// 回放），此后相同请求回放，撤回后才拒绝答复并允许另建新方案。
func TestWithdrawSplitIntentPendingSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	req := withdrawReq("alice", "it1", "wd-1")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 1)
	// 方案没有被撤回：仍待确认、无结束时间，各方答复保持创建时内容。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": "", "carol": "",
	}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("失败后意向历史应只有创建记录: %+v", evs)
	}
	// 撤回不改变藏品持有人或持有版本。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 仍有效的方案继续接受原本允许的答复：保存仍被阻断时，bob 的同意在通过
	// 业务检查后只失败在保存上（返回保存错误而非 ErrSplitIntentWithdrawn 等
	// 终结拒绝），且失败同样不留痕迹。
	ansReq := answerReq("bob", "it1", "ans-bob", true)
	ares, aerr := r.AnswerSplitIntent(ansReq)
	assertAnswerSaveFailureEmpty(t, ares, aerr)
	assertAnswerRequestFree(t, r, ansReq, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": ""}, false)

	// 保存条件未恢复时再次提交同一撤回请求：仍重新尝试保存并失败，不能把
	// 上次未保存的撤回当成已保存回放。
	res, err = r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"carol": ""}, false)

	// 保存条件恢复后，参与账户此前被允许的答复可以真正保存（历史序号 2）；
	// 其他操作成功保存不能把这次未保存的撤回带入登记册。
	restoreBatchSave(t, r)
	if _, err := r.AnswerSplitIntent(ansReq); err != nil {
		t.Fatalf("失败撤回后参与账户应仍能答复: %v", err)
	}
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertWithdrawRequestFree(t, r, req, 2)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": "",
	}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("其他操作保存后意向历史仍应是 2 条: %+v", evs)
	}
	// 方案仍同时有效：持有人不能另建新方案。
	assertIntentBlocksCreate(t, r)

	// 用原请求号和相同内容重提：按当前状态重新处理，正常撤回，首次重新
	// 成功不标记为回放。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil {
		t.Fatalf("恢复后重提应正常撤回: %v", err)
	}
	if res.Replayed || res.Err != nil || res.IntentID != "it1" || res.Status != SplitWithdrawn {
		t.Fatalf("重提结果异常: %+v", res)
	}
	assertIntentParties(t, r, "it1", SplitWithdrawn, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": "",
	}, true)
	evs, _ := r.SplitIntentHistory("i1")
	if len(evs) != 3 || evs[2].Seq != 3 || evs[2].Operator != "alice" ||
		evs[2].Kind != "withdraw" || evs[2].FromStatus != SplitPending ||
		evs[2].ToStatus != SplitWithdrawn {
		t.Fatalf("撤回历史异常: %+v", evs)
	}
	if r.state.NextIntentSeq != 3 {
		t.Fatalf("NextIntentSeq = %d, want 3", r.state.NextIntentSeq)
	}
	// 持有始终不变。
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 真正保存成功后，相同请求回放首次结果。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || !res.Replayed || res.IntentID != "it1" || res.Status != SplitWithdrawn {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
	// 撤回后不再接受答复。
	if _, err := r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol", true)); !errors.Is(err, ErrSplitIntentWithdrawn) {
		t.Fatalf("撤回后答复应为 ErrSplitIntentWithdrawn: %v", err)
	}
	// 撤回后持有人才可用新编号另建方案。
	if _, err := r.CreateSplitIntent(createIntentReq(r, "it2")); err != nil {
		t.Fatalf("撤回后应允许另建新方案: %v", err)
	}
}

// TestWithdrawSplitIntentSaveFailureUnreadableDisk 覆盖：撤回保存失败且原
// 数据暂时无法读取（commit 无法按磁盘重建状态）时，未保存的撤回状态、
// 结束时间、意向历史与请求号占用也不能留在当前登记册中；恢复正常读写后
// 其他操作成功保存不带入这次撤回，方案仍有效并阻止另建，原请求重提正常
// 撤回。
func TestWithdrawSplitIntentSaveFailureUnreadableDisk(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
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
	// 状态未能重建时也必须显式撤销：同一个仍打开的登记册中方案仍待确认、
	// 无结束时间，历史保持创建 1 条，请求号与序号未被消耗。
	assertWithdrawRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": "", "carol": "",
	}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("不可读磁盘失败后历史应只有创建: %+v", evs)
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
	assertWithdrawRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("其他操作保存后历史仍应只有创建: %+v", evs)
	}
	// 方案仍同时有效：不能另建新方案。
	assertIntentBlocksCreate(t, r)

	// 原请求重提首次成功撤回，不标回放。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || res.Replayed || res.Status != SplitWithdrawn {
		t.Fatalf("恢复后重提应正常撤回: %+v, err %v", res, err)
	}
	assertIntentParties(t, r, "it1", SplitWithdrawn, map[string]string{"bob": ""}, true)
	evs, _ := r.SplitIntentHistory("i1")
	if len(evs) != 2 || evs[1].Seq != 2 || evs[1].Kind != "withdraw" {
		t.Fatalf("撤回历史异常: %+v", evs)
	}
	if r.state.NextIntentSeq != 2 {
		t.Fatalf("NextIntentSeq = %d, want 2", r.state.NextIntentSeq)
	}
}

// TestWithdrawSplitIntentAgreedSaveFailure 覆盖：已达成意向的撤回保存失败
// 时，方案仍显示已达成、无结束时间，历史保持各方答复记录，仍阻止另建；
// 恢复后原请求重提首次成功才撤回（自已达成状态），此后回放。
func TestWithdrawSplitIntentAgreedSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	if _, err := r.AnswerSplitIntent(answerReq("bob", "it1", "ans-bob", true)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol", true)); err != nil {
		t.Fatal(err)
	}
	req := withdrawReq("alice", "it1", "wd-1")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 3)
	assertIntentParties(t, r, "it1", SplitAgreed, map[string]string{
		"alice": SplitAnswerAgree, "bob": SplitAnswerAgree, "carol": SplitAnswerAgree,
	}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 3 {
		t.Fatalf("失败后意向历史应保持 3 条: %+v", evs)
	}

	// 保存恢复后其他操作不夹带撤回，已达成方案仍阻止另建。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("erin", "路人"); err != nil {
		t.Fatal(err)
	}
	assertWithdrawRequestFree(t, r, req, 3)
	assertIntentParties(t, r, "it1", SplitAgreed, map[string]string{
		"bob": SplitAnswerAgree, "carol": SplitAnswerAgree,
	}, false)
	assertIntentBlocksCreate(t, r)

	// 原请求重提：自已达成状态正常撤回，首次不标回放。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || res.Replayed || res.Err != nil || res.Status != SplitWithdrawn {
		t.Fatalf("恢复后重提应正常撤回: %+v, err %v", res, err)
	}
	evs, _ := r.SplitIntentHistory("i1")
	if len(evs) != 4 || evs[3].Seq != 4 || evs[3].Kind != "withdraw" ||
		evs[3].FromStatus != SplitAgreed || evs[3].ToStatus != SplitWithdrawn {
		t.Fatalf("已达成撤回历史异常: %+v", evs)
	}
	// 成功保存后相同请求回放。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || !res.Replayed || res.Status != SplitWithdrawn {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
}

// TestWithdrawSplitIntentForbiddenSaveFailure 覆盖：无权撤回的状态类业务
// 拒绝在拒绝结果自身落盘失败时，也必须返回实际保存错误、结果为空、不占用
// 请求号，原意向保持不变；保存恢复后原请求重提重新保存该拒绝（首次不标
// 回放），再次提交才回放。
func TestWithdrawSplitIntentForbiddenSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	req := withdrawReq("bob", "it1", "wd-bob")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 1)
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{
		"alice": SplitAnswerAgree, "bob": "", "carol": "",
	}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("无权拒绝保存失败不应新增历史: %+v", evs)
	}

	restoreBatchSave(t, r)
	// 原请求重提：重新保存无权拒绝，首次不标回放。
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("重提应返回 ErrForbidden: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrForbidden) ||
		res.Status != "" {
		t.Fatalf("无权拒绝的重提结果异常: %+v", res)
	}
	// 再次提交相同请求才回放已保存的拒绝。
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrForbidden) || !res.Replayed || !errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("无权拒绝应被回放: %+v, err %v", res, err)
	}
	// 业务拒绝不改变意向、不新增历史；发起人仍可正常撤回。
	assertIntentParties(t, r, "it1", SplitPending, map[string]string{"bob": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("业务拒绝后意向历史仍应只有创建: %+v", evs)
	}
	if _, err := r.WithdrawSplitIntent(withdrawReq("alice", "it1", "wd-ok")); err != nil {
		t.Fatalf("发起人仍可正常撤回: %v", err)
	}
}

// TestWithdrawSplitIntentEndedStatusSaveFailure 覆盖：对已拒绝、已失效、
// 已过期意向的撤回本应返回对应哨兵错误，但拒绝结果保存失败时返回实际保存
// 错误、结果为空、不占用请求号；意向原有的落盘/派生状态与结束时间保持
// 不变。恢复后原请求重提重新保存对应拒绝（首次不标回放），再次提交才回放。
func TestWithdrawSplitIntentEndedStatusSaveFailure(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		answerSaveFailureWorld(t, r)
		// carol 拒绝，方案终结为已拒绝（已保存，结束时间已写入）。
		if _, err := r.AnswerSplitIntent(answerReq("carol", "it1", "ans-carol", false)); err != nil {
			t.Fatal(err)
		}
		before, _ := r.GetSplitIntent("it1")
		req := withdrawReq("alice", "it1", "wd-after-reject")

		blockBatchSave(t, r)
		res, err := r.WithdrawSplitIntent(req)
		assertWithdrawSaveFailureEmpty(t, res, err)
		assertWithdrawRequestFree(t, r, req, 2)
		// 原已拒绝状态与结束时间保持不变。
		assertIntentParties(t, r, "it1", SplitRejected, map[string]string{
			"carol": SplitAnswerReject,
		}, true)

		restoreBatchSave(t, r)
		res, err = r.WithdrawSplitIntent(req)
		if !errors.Is(err, ErrSplitIntentRejected) {
			t.Fatalf("重提应返回 ErrSplitIntentRejected: %v", err)
		}
		if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentRejected) {
			t.Fatalf("已拒绝意向撤回的重提结果异常: %+v", res)
		}
		res, err = r.WithdrawSplitIntent(req)
		if !errors.Is(err, ErrSplitIntentRejected) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的拒绝: %+v, err %v", res, err)
		}
		after, _ := r.GetSplitIntent("it1")
		if !after.EndedAt.Equal(before.EndedAt) {
			t.Fatalf("结束时间不应被失败请求改变: before %v after %v", before.EndedAt, after.EndedAt)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		answerSaveFailureWorld(t, r)
		// alice 把 i1 合法转给 dave：持有版本变为 2，意向失效。
		if _, err := r.Transfer(xferReq("alice", "i1", "dave", 1, "rt-move")); err != nil {
			t.Fatal(err)
		}
		req := withdrawReq("alice", "it1", "wd-invalid")

		blockBatchSave(t, r)
		res, err := r.WithdrawSplitIntent(req)
		assertWithdrawSaveFailureEmpty(t, res, err)
		assertWithdrawRequestFree(t, r, req, 1)
		assertIntentParties(t, r, "it1", SplitInvalid, map[string]string{"bob": ""}, false)

		restoreBatchSave(t, r)
		res, err = r.WithdrawSplitIntent(req)
		if !errors.Is(err, ErrSplitIntentInvalid) {
			t.Fatalf("重提应返回 ErrSplitIntentInvalid: %v", err)
		}
		if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentInvalid) {
			t.Fatalf("已失效意向撤回的重提结果异常: %+v", res)
		}
		// 失效拒绝不写入结束时间、不新增历史。
		assertIntentParties(t, r, "it1", SplitInvalid, map[string]string{"bob": ""}, false)
		if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
			t.Fatalf("失效拒绝不应新增意向历史: %+v", evs)
		}
		res, err = r.WithdrawSplitIntent(req)
		if !errors.Is(err, ErrSplitIntentInvalid) || !res.Replayed ||
			!errors.Is(res.Err, ErrSplitIntentInvalid) {
			t.Fatalf("再次提交应回放已保存的失效拒绝: %+v, err %v", res, err)
		}
	})

	t.Run("expired", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		intentWorld(t, r)
		base := time.Unix(2_100_000_000, 0)
		r.now = func() time.Time { return base }
		defer func() { r.now = time.Now }()
		if _, err := r.CreateSplitIntent(createIntentReq(r, "it1")); err != nil {
			t.Fatal(err)
		}
		req := withdrawReq("alice", "it1", "wd-expired")

		blockBatchSave(t, r)
		// 时间走到到期点：意向已过期。
		r.now = func() time.Time { return base.Add(time.Hour) }
		res, err := r.WithdrawSplitIntent(req)
		assertWithdrawSaveFailureEmpty(t, res, err)
		assertWithdrawRequestFree(t, r, req, 1)
		assertIntentParties(t, r, "it1", SplitExpired, map[string]string{"bob": ""}, false)

		restoreBatchSave(t, r)
		res, err = r.WithdrawSplitIntent(req)
		if !errors.Is(err, ErrSplitIntentExpired) {
			t.Fatalf("重提应返回 ErrSplitIntentExpired: %v", err)
		}
		if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentExpired) {
			t.Fatalf("已过期意向撤回的重提结果异常: %+v", res)
		}
		if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
			t.Fatalf("过期拒绝不应新增意向历史: %+v", evs)
		}
		res, err = r.WithdrawSplitIntent(req)
		if !errors.Is(err, ErrSplitIntentExpired) || !res.Replayed ||
			!errors.Is(res.Err, ErrSplitIntentExpired) {
			t.Fatalf("再次提交应回放已保存的过期拒绝: %+v, err %v", res, err)
		}
	})
}

// TestWithdrawSplitIntentRepeatWithdrawSaveFailure 覆盖：对已撤回意向再次
// 撤回（幂等路径）只登记请求结果、不新增变更记录；该请求结果保存失败时
// 同样返回实际保存错误、结果为空、不占用请求号，原撤回状态与历史保持
// 不变；恢复后重提首次正常返回已撤回（不标回放、不新增历史），此后回放。
func TestWithdrawSplitIntentRepeatWithdrawSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	// 首次撤回已保存：历史为 create + withdraw，序号 2。
	if _, err := r.WithdrawSplitIntent(withdrawReq("alice", "it1", "wd-1")); err != nil {
		t.Fatal(err)
	}
	req := withdrawReq("alice", "it1", "wd-2")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 2)
	assertIntentParties(t, r, "it1", SplitWithdrawn, map[string]string{"bob": ""}, true)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("幂等撤回保存失败后历史应仍是 2 条: %+v", evs)
	}

	restoreBatchSave(t, r)
	// 重提：首次为该请求号真正保存成功结果，不标回放，也不新增历史。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || res.Replayed || res.Err != nil ||
		res.IntentID != "it1" || res.Status != SplitWithdrawn {
		t.Fatalf("恢复后重提应返回已撤回: %+v, err %v", res, err)
	}
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 2 {
		t.Fatalf("幂等撤回重提不应新增历史: %+v", evs)
	}
	if r.state.NextIntentSeq != 2 {
		t.Fatalf("NextIntentSeq = %d, want 2", r.state.NextIntentSeq)
	}
	// 保存成功后相同请求回放首次结果。
	res, err = r.WithdrawSplitIntent(req)
	if err != nil || !res.Replayed || res.Status != SplitWithdrawn {
		t.Fatalf("成功后重提应回放: %+v, err %v", res, err)
	}
}

// TestWithdrawSplitIntentSaveFailureRetryExpired 覆盖：撤回保存失败后意向
// 到期的，用原请求号和相同内容重提应沿用现有的过期拒绝，而不是回放未保存
// 的撤回或强行把方案撤回；该拒绝保存后再次提交才回放。
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

	// 时间走到到期点：意向已过期，撤回未生效。
	r.now = func() time.Time { return base.Add(time.Hour) }
	assertIntentParties(t, r, "it1", SplitExpired, map[string]string{"bob": ""}, false)

	// 保存条件恢复，用原请求号、相同内容重提：按当时状态重新处理，沿用
	// 现有过期拒绝，不回放未保存的撤回。
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
// 版本发生变化（藏品被合法转出）使意向失效的，原请求重提沿用现有的失效
// 拒绝，不回放未保存的撤回；失效后当前持有人可用新编号另建方案。
func TestWithdrawSplitIntentSaveFailureRetryInvalid(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)
	req := withdrawReq("alice", "it1", "wd-1")

	blockBatchSave(t, r)
	res, err := r.WithdrawSplitIntent(req)
	assertWithdrawSaveFailureEmpty(t, res, err)
	assertWithdrawRequestFree(t, r, req, 1)

	// 保存恢复后，alice 把 i1 合法转给 dave：持有版本变为 2，意向失效。
	restoreBatchSave(t, r)
	if _, err := r.Transfer(xferReq("alice", "i1", "dave", 1, "rt-move")); err != nil {
		t.Fatal(err)
	}
	assertIntentParties(t, r, "it1", SplitInvalid, map[string]string{"bob": ""}, false)

	// 原请求重提：按当时状态重新处理，沿用失效拒绝，首次不标回放。
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentInvalid) {
		t.Fatalf("失效后重提应返回 ErrSplitIntentInvalid: %v", err)
	}
	if res.Replayed || res.IntentID != "it1" || !errors.Is(res.Err, ErrSplitIntentInvalid) {
		t.Fatalf("失效拒绝的重提结果异常: %+v", res)
	}
	// 方案没有被撤回：当前派生状态为已失效，无结束时间，历史无撤回。
	assertIntentParties(t, r, "it1", SplitInvalid, map[string]string{"bob": ""}, false)
	if evs, _ := r.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("失效拒绝不应新增意向历史: %+v", evs)
	}
	// 该拒绝保存后，相同请求回放首次（失效）拒绝。
	res, err = r.WithdrawSplitIntent(req)
	if !errors.Is(err, ErrSplitIntentInvalid) || !res.Replayed ||
		!errors.Is(res.Err, ErrSplitIntentInvalid) {
		t.Fatalf("保存后的再次提交应回放失效拒绝: %+v, err %v", res, err)
	}
}

// TestWithdrawSplitIntentSaveFailureAfterReopen 覆盖磁盘视角：原数据可读时
// 保存失败后关闭重开，看到的仍是撤回前状态——未保存的撤回不存在、请求号
// 未占用；恢复保存后用原请求重提正常撤回。
func TestWithdrawSplitIntentSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	answerSaveFailureWorld(t, r)
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
		"alice": SplitAnswerAgree, "bob": "", "carol": "",
	}, false)
	if _, ok := r2.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatal("重开后未保存的撤回不应占用请求号")
	}
	if r2.state.NextIntentSeq != 1 {
		t.Fatalf("重开后 NextIntentSeq = %d, want 1", r2.state.NextIntentSeq)
	}
	if evs, _ := r2.SplitIntentHistory("i1"); len(evs) != 1 {
		t.Fatalf("重开后历史应只有创建: %+v", evs)
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

// TestWithdrawSplitIntentValidationIgnoresSaveFailure 覆盖：必填内容缺失与
// 意向不存在的请求不占用请求号、不要求保存，即使数据位置暂时不可写也仍
// 返回原参数/引用错误，不能改报保存错误。
func TestWithdrawSplitIntentValidationIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	answerSaveFailureWorld(t, r)

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 必填内容缺失：返回 ErrInvalidArgument，不是保存错误。
	if _, err := r.WithdrawSplitIntent(WithdrawSplitIntentRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	missing := withdrawReq("alice", "it1", "wd-miss")
	missing.IntentID = ""
	if _, err := r.WithdrawSplitIntent(missing); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument: %v", err)
	}

	// 引用不存在的意向：返回 ErrNotFound，不是保存错误。
	notFoundReq := withdrawReq("alice", "it-nf", "wd-nf")
	res, err := r.WithdrawSplitIntent(notFoundReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("引用不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.IntentID != "it-nf" || !errors.Is(res.Err, ErrNotFound) {
		t.Fatalf("引用不存在的结果异常: %+v", res)
	}

	// 两类请求都不占用请求号，意向历史序号也不消耗。
	if _, ok := r.state.Requests[requestKey("alice", "wd-nf")]; ok {
		t.Fatal("引用不存在不应占用请求号")
	}
	if r.state.NextIntentSeq != 1 {
		t.Fatalf("NextIntentSeq = %d, want 1", r.state.NextIntentSeq)
	}
}
