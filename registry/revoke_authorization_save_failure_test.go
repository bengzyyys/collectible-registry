package registry

import (
	"errors"
	"os"
	"testing"
	"time"
)

// 本文件为撤销代转授权补充"拒绝结果保存失败"场景的回归保障，与创建授权
// （authorization_save_failure_test.go）及发行、转让、拆分意向的同类用例
// 行为对齐：非授权人请求撤销得到的 ErrForbidden、授权人撤销已使用授权得到
// 的 ErrAuthorizationUsed 都属于需要落盘记忆的状态类业务拒绝，登记册应保存
// 这两种拒绝，让同一请求重提时返回首次结果。若本次拒绝结果尚未写入原登记册
// 就发生写入失败，RevokeAuthorization 必须返回实际保存错误，不能只返回上述
// 业务错误，让调用者误以为拒绝结果已经记住。
//
// 保存失败时返回的结果必须为空：不带授权编号与状态，Replayed 为 false，
// 结果内的 Err 为空，单独返回的 error 保留保存失败的原因。授权内容、授权
// 变更记录及藏品当前持有人和版本保持操作前的状态；该操作者的本次请求号没有
// 被占用——即使写入失败后原登记册数据暂时无法读取，也不能让尚未保存的拒绝
// 留在当前已打开的登记册中，或随之后另一次正常操作被保存下来。保存条件恢复
// 后用同一操作者、请求号、授权编号和原因重新提交，按当时的授权情况重新
// 处理：拒绝条件仍存在时先成功保存此次拒绝再返回相应业务错误（首次重提不标
// 回放），此后完全相同的提交才回放它。失败保存的请求也不能使该操作者使用
// 同号撤销另一份自己有权撤销的授权时遭遇请求号冲突；只有实际保存过的结果
// 才继续适用原有的同号不同参数冲突规则。
//
// 必填内容缺失及授权不存在仍直接按原错误拒绝、不占用请求号，也不因当前无法
// 保存而变成保存错误。
//
// 失败注入方式与其他场景共用：在临时文件路径 .registry.json.tmp 上预先
// 建一个目录，save 在 OpenFile 阶段即以 EISDIR 失败，原快照完好可读；
// 另有用例同时破坏数据文件，覆盖"原数据暂时无法读取"的情形。

// revokeAuthzSaveReq 构造一条撤销授权请求。
func revokeAuthzSaveReq(op, authID, rid string) RevokeAuthorizationRequest {
	return RevokeAuthorizationRequest{Operator: op, Reason: "撤销授权", RequestID: rid, AuthID: authID}
}

// setupUsedAuthzWorld 建立两份授权：a1 仍有效、未使用；a2 已由受托人 bob
// 代转给 carol（已使用）。代转后藏品 i1 在 carol 手中、版本 2；授权变更
// 记录依次为 create a1（序号 1）、create a2（序号 2）、use a2（序号 3）。
// a1 虽因藏品易手无法再用于代转，但尚未使用，授权人 alice 仍可撤销。
func setupUsedAuthzWorld(t *testing.T, r *Registry) {
	t.Helper()
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateAuthorization(createAuthReq(r, "a2", time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ProxyTransfer(proxyReq("bob", "a2", "p-a2")); err != nil {
		t.Fatal(err)
	}
}

// assertRevokeAuthzSaveFailureEmpty 核对撤销"拒绝结果保存失败"的返回：
// error 是保存错误而非任何业务拒绝（含授权类的已使用、已撤销、已过期
// 哨兵）；结果为空——不携带授权编号或状态，业务错误为空，也不标记为重复
// 返回。
func assertRevokeAuthzSaveFailureEmpty(t *testing.T, res RevokeAuthorizationResult, err error) {
	t.Helper()
	assertSaveFailureError(t, err)
	for _, s := range []error{ErrAuthorizationUsed, ErrAuthorizationRevoked, ErrAuthorizationExpired} {
		if errors.Is(err, s) {
			t.Fatalf("返回的应是保存错误，不能是业务拒绝 %v: %v", s, err)
		}
	}
	if res.AuthID != "" || res.Status != "" || res.Replayed || res.Err != nil {
		t.Fatalf("拒绝结果保存失败必须返回空结果: %+v", res)
	}
}

// assertRevokeRequestFree 核对请求号未被这次未保存的拒绝占用（在同一个已
// 打开的登记册上检查）。
func assertRevokeRequestFree(t *testing.T, r *Registry, req RevokeAuthorizationRequest) {
	t.Helper()
	if _, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]; ok {
		t.Fatalf("未保存的拒绝不应占用请求号 %s", req.RequestID)
	}
}

// assertRevokeRequestSaved 核对拒绝结果已登记落盘。
func assertRevokeRequestSaved(t *testing.T, r *Registry, req RevokeAuthorizationRequest) {
	t.Helper()
	prev, ok := r.state.Requests[requestKey(req.Operator, req.RequestID)]
	if !ok {
		t.Fatalf("拒绝保存成功后应登记请求结果，请求号 %s 却空闲", req.RequestID)
	}
	if !prev.Rejected || prev.AuthID != req.AuthID {
		t.Fatalf("登记的请求结果异常: %+v", prev)
	}
}

// TestRevokeAuthzForbiddenSaveFailure 覆盖：非授权人撤销的 ErrForbidden
// 拒绝在拒绝结果自身落盘失败时，返回实际保存错误与空结果，业务错误被掩盖的
// 问题得到修正；请求号不被占用，授权内容、授权变更记录与藏品持有保持操作前
// 状态。保存恢复且拒绝条件仍在时，原请求重提先成功保存此次拒绝再返回
// ErrForbidden（首次重提不标回放、结果带原授权编号与业务错误），此后完全
// 相同的提交才回放该拒绝；授权人仍可正常撤销该授权。
func TestRevokeAuthzForbiddenSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("bob", "a1", "rv-bob")

	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)
	// 授权内容与授权变更记录保持操作前状态：仍有效，只有创建记录一条。
	a, err := r.GetAuthorization("a1")
	if err != nil || a.Status != AuthActive || a.RevokedAt != (time.Time{}) {
		t.Fatalf("未保存的无权拒绝不应改变授权: %+v, %v", a, err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 || evs[0].Kind != "create" {
		t.Fatalf("未保存的无权拒绝不应新增授权变更记录: %+v", evs)
	}
	assertHoldingUnchanged(t, r, "i1", "alice", 1, 1)

	// 保存条件未恢复时再次提交同一请求：仍重新尝试保存并失败，不能把上次
	// 未保存的拒绝当成已保存拒绝回放。
	res, err = r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)

	// 保存条件恢复后，其他操作成功保存也不能把这次未保存的拒绝带入登记册。
	restoreBatchSave(t, r)
	if err := r.RegisterAccount("dave", "路人"); err != nil {
		t.Fatal(err)
	}
	assertRevokeRequestFree(t, r, req)
	a, _ = r.GetAuthorization("a1")
	if a.Status != AuthActive {
		t.Fatalf("其他操作保存后授权仍应有效: %+v", a)
	}

	// 原请求重提：拒绝条件仍在，先成功保存此次拒绝，再返回业务错误本身；
	// 结果带原授权编号与业务错误，这次重新处理不标记为回放。
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("重提应在保存成功后返回 ErrForbidden: %v", err)
	}
	if res.Replayed || res.AuthID != "a1" || !errors.Is(res.Err, ErrForbidden) ||
		res.Status != "" {
		t.Fatalf("无权拒绝的重提结果异常: %+v", res)
	}
	assertRevokeRequestSaved(t, r, req)
	a, _ = r.GetAuthorization("a1")
	if a.Status != AuthActive {
		t.Fatalf("无权拒绝不应改变授权状态: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
		t.Fatalf("无权拒绝不应新增授权变更记录: %+v", evs)
	}

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrForbidden) || !res.Replayed || res.AuthID != "a1" ||
		!errors.Is(res.Err, ErrForbidden) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}

	// 授权人仍可正常撤销该授权（业务含义不变），撤销成功后新增一条记录。
	ok, err := r.RevokeAuthorization(revokeAuthzSaveReq("alice", "a1", "rv-ok"))
	if err != nil || ok.Status != AuthRevoked || ok.Replayed {
		t.Fatalf("授权人应仍可正常撤销: %+v, %v", ok, err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 2 ||
		evs[1].Kind != "revoke" || evs[1].ToStatus != AuthRevoked {
		t.Fatalf("正常撤销应新增一条撤销记录: %+v", evs)
	}
}

// TestRevokeAuthzUsedSaveFailure 覆盖：授权人撤销已使用授权的
// ErrAuthorizationUsed 拒绝在拒绝结果自身落盘失败时，同样返回实际保存错误
// 与空结果；已使用终态、使用关联与授权变更记录保持操作前内容，藏品持有
// 不变。保存恢复后原请求重提重新保存该拒绝（首次不标回放），再次提交才
// 回放。
func TestRevokeAuthzUsedSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupUsedAuthzWorld(t, r)
	req := revokeAuthzSaveReq("alice", "a2", "rv-used")

	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)
	// 已使用终态及其使用关联保持操作前内容。
	a, err := r.GetAuthorization("a2")
	if err != nil || a.Status != AuthUsed || a.UsedTxSeq != 2 {
		t.Fatalf("未保存的拒绝不应改变已使用授权: %+v, %v", a, err)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 3 ||
		evs[2].Kind != "use" || evs[2].AuthID != "a2" {
		t.Fatalf("授权变更记录应保持操作前的 3 条: %+v", evs)
	}
	assertHoldingUnchanged(t, r, "i1", "carol", 2, 2)

	// 保存仍被阻断时再次提交：仍失败在保存上，不能回放未保存的拒绝。
	res, err = r.RevokeAuthorization(req)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, req)

	restoreBatchSave(t, r)
	// 重提：先成功保存此次拒绝，再返回 ErrAuthorizationUsed，首次不标回放，
	// 结果带原授权编号与业务错误、状态为空。
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrAuthorizationUsed) {
		t.Fatalf("重提应在保存成功后返回 ErrAuthorizationUsed: %v", err)
	}
	if res.Replayed || res.AuthID != "a2" || !errors.Is(res.Err, ErrAuthorizationUsed) ||
		res.Status != "" {
		t.Fatalf("已使用拒绝的重提结果异常: %+v", res)
	}
	assertRevokeRequestSaved(t, r, req)
	a, _ = r.GetAuthorization("a2")
	if a.Status != AuthUsed || a.UsedTxSeq != 2 {
		t.Fatalf("已使用拒绝不应改变授权: %+v", a)
	}
	if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 3 {
		t.Fatalf("已使用拒绝不应新增授权变更记录: %+v", evs)
	}

	// 拒绝已保存：相同内容重提回放原拒绝。
	res, err = r.RevokeAuthorization(req)
	if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed ||
		res.AuthID != "a2" || !errors.Is(res.Err, ErrAuthorizationUsed) {
		t.Fatalf("保存后的重提应回放原拒绝: %+v, err %v", res, err)
	}
}

// TestRevokeAuthzRejectSaveFailureUnreadableDisk 覆盖：拒绝结果保存失败且
// 原数据暂时无法读取（commit 无法按磁盘重建状态）时，未保存的拒绝也不能
// 留在当前已打开的登记册中；恢复正常读写后其他操作成功保存不带入这次拒绝，
// 原请求重提才首次保存对应业务拒绝，再次提交才回放。无权与已使用两种拒绝
// 分别覆盖。
func TestRevokeAuthzRejectSaveFailureUnreadableDisk(t *testing.T) {
	t.Run("forbidden", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		authzWorld(t, r)
		if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
			t.Fatal(err)
		}
		req := revokeAuthzSaveReq("bob", "a1", "rv-bob")

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
		// 状态未能重建时也必须显式撤销：请求号空闲，授权仍有效，历史只有
		// 创建记录。
		assertRevokeRequestFree(t, r, req)
		a, _ := r.GetAuthorization("a1")
		if a.Status != AuthActive {
			t.Fatalf("不可读磁盘失败后授权不应改变: %+v", a)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 1 {
			t.Fatalf("不可读磁盘失败后历史应只有创建: %+v", evs)
		}

		if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
			t.Fatal(err)
		}
		restoreBatchSave(t, r)
		if err := r.RegisterAccount("dave", "路人"); err != nil {
			t.Fatal(err)
		}
		assertRevokeRequestFree(t, r, req)

		res, err = r.RevokeAuthorization(req)
		if !errors.Is(err, ErrForbidden) || res.Replayed ||
			!errors.Is(res.Err, ErrForbidden) {
			t.Fatalf("恢复后重提应首次保存无权拒绝: %+v, err %v", res, err)
		}
		res, err = r.RevokeAuthorization(req)
		if !errors.Is(err, ErrForbidden) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的无权拒绝: %+v, err %v", res, err)
		}
	})

	t.Run("used", func(t *testing.T) {
		r := mustCreate(t, tempDir(t))
		setupUsedAuthzWorld(t, r)
		req := revokeAuthzSaveReq("alice", "a2", "rv-used")

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
		assertRevokeRequestFree(t, r, req)
		a, _ := r.GetAuthorization("a2")
		if a.Status != AuthUsed || a.UsedTxSeq != 2 {
			t.Fatalf("不可读磁盘失败后已使用授权不应改变: %+v", a)
		}
		if evs, _ := r.AuthorizationHistory("i1"); len(evs) != 3 {
			t.Fatalf("不可读磁盘失败后授权变更记录应保持 3 条: %+v", evs)
		}

		if err := os.WriteFile(dataFile(r.dir), orig, fileMode); err != nil {
			t.Fatal(err)
		}
		restoreBatchSave(t, r)
		if err := r.RegisterAccount("dave", "路人"); err != nil {
			t.Fatal(err)
		}
		assertRevokeRequestFree(t, r, req)

		res, err = r.RevokeAuthorization(req)
		if !errors.Is(err, ErrAuthorizationUsed) || res.Replayed ||
			!errors.Is(res.Err, ErrAuthorizationUsed) {
			t.Fatalf("恢复后重提应首次保存已使用拒绝: %+v, err %v", res, err)
		}
		res, err = r.RevokeAuthorization(req)
		if !errors.Is(err, ErrAuthorizationUsed) || !res.Replayed {
			t.Fatalf("再次提交应回放已保存的已使用拒绝: %+v, err %v", res, err)
		}
	})
}

// TestRevokeAuthzFailedSaveRequestIDReusable 覆盖：保存失败的拒绝不占用请求
// 号——该操作者用同一请求号撤销另一份自己有权撤销的授权时不能遭遇请求号
// 冲突；只有实际保存过的结果才继续适用原有的同号不同参数冲突规则。
func TestRevokeAuthzFailedSaveRequestIDReusable(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	setupUsedAuthzWorld(t, r)
	// alice 撤销已使用的 a2 将被拒绝；a1 未使用，alice 有权撤销。
	failedReq := revokeAuthzSaveReq("alice", "a2", "rv-x")
	otherReq := revokeAuthzSaveReq("alice", "a1", "rv-x")

	blockBatchSave(t, r)
	res, err := r.RevokeAuthorization(failedReq)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, failedReq)

	// 保存条件未恢复时重放原失败请求：仍只失败在保存上。
	res, err = r.RevokeAuthorization(failedReq)
	assertRevokeAuthzSaveFailureEmpty(t, res, err)
	assertRevokeRequestFree(t, r, failedReq)

	// 保存恢复后，同一操作者用同一请求号撤销另一份自己有权撤销的授权：
	// 未保存的拒绝不适用冲突规则，请求正常成功且不标回放。
	restoreBatchSave(t, r)
	res, err = r.RevokeAuthorization(otherReq)
	if err != nil {
		t.Fatalf("未保存的拒绝不应阻碍同号撤销另一份有权撤销的授权: %v", err)
	}
	if res.Replayed || res.Err != nil || res.AuthID != "a1" || res.Status != AuthRevoked {
		t.Fatalf("同号撤销另一份授权的结果异常: %+v", res)
	}
	a1, _ := r.GetAuthorization("a1")
	if a1.Status != AuthRevoked {
		t.Fatalf("a1 应已撤销: %+v", a1)
	}
	// 已使用的 a2 不受影响。
	a2, _ := r.GetAuthorization("a2")
	if a2.Status != AuthUsed {
		t.Fatalf("a2 应仍为已使用: %+v", a2)
	}

	// 请求号此刻已被实际保存的 a1 成功结果占用：同号不同参数（改撤 a2）
	// 按原有规则返回 ErrRequestConflict。
	if _, err := r.RevokeAuthorization(failedReq); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("实际保存过的结果才应触发同号不同参数冲突: %v", err)
	}
	// 完全相同的已保存请求（撤 a1）回放首次成功结果。
	res, err = r.RevokeAuthorization(otherReq)
	if err != nil || !res.Replayed || res.AuthID != "a1" || res.Status != AuthRevoked {
		t.Fatalf("已保存的同号成功结果应被回放: %+v, err %v", res, err)
	}
}

// TestRevokeAuthzRejectSaveFailureAfterReopen 覆盖磁盘视角：原数据可读时
// 拒绝结果保存失败后关闭重开，看到的仍是拒绝前状态——授权未改变、请求号
// 未占用；恢复保存后用原请求重提才首次保存该拒绝，再次提交回放。
func TestRevokeAuthzRejectSaveFailureAfterReopen(t *testing.T) {
	dir := tempDir(t)
	r := mustCreate(t, dir)
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}
	req := revokeAuthzSaveReq("bob", "a1", "rv-bob")

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
		t.Fatal("重开后未保存的拒绝不应占用请求号")
	}
	if a, _ := r2.GetAuthorization("a1"); a.Status != AuthActive {
		t.Fatalf("重开后授权应仍有效: %+v", a)
	}
	if evs, _ := r2.AuthorizationHistory("i1"); len(evs) != 1 {
		t.Fatalf("重开后授权变更记录应只有创建: %+v", evs)
	}

	restoreBatchSave(t, r2)
	res2, err := r2.RevokeAuthorization(req)
	if !errors.Is(err, ErrForbidden) || res2.Replayed ||
		!errors.Is(res2.Err, ErrForbidden) || res2.AuthID != "a1" {
		t.Fatalf("重开后重提应首次保存无权拒绝: %+v, err %v", res2, err)
	}
	res2, err = r2.RevokeAuthorization(req)
	if !errors.Is(err, ErrForbidden) || !res2.Replayed {
		t.Fatalf("再次提交应回放已保存的无权拒绝: %+v, err %v", res2, err)
	}
}

// TestRevokeAuthzValidationIgnoresSaveFailure 覆盖：必填内容缺失与授权不
// 存在的请求直接按原错误拒绝、不占用请求号、不要求保存，即使数据位置暂时
// 不可写也仍返回原参数/引用错误，不能改报保存错误。
func TestRevokeAuthzValidationIgnoresSaveFailure(t *testing.T) {
	r := mustCreate(t, tempDir(t))
	authzWorld(t, r)
	if _, err := r.CreateAuthorization(createAuthReq(r, "a1", time.Hour)); err != nil {
		t.Fatal(err)
	}

	blockBatchSave(t, r)
	defer restoreBatchSave(t, r)

	// 必填内容缺失：返回 ErrInvalidArgument，不是保存错误。
	if _, err := r.RevokeAuthorization(RevokeAuthorizationRequest{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument，不能改报保存错误: %v", err)
	}
	missing := revokeAuthzSaveReq("alice", "a1", "rv-miss")
	missing.AuthID = ""
	if _, err := r.RevokeAuthorization(missing); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("必填缺失应返回 ErrInvalidArgument: %v", err)
	}

	// 授权不存在：返回 ErrNotFound（结果带授权编号与业务错误），不是保存
	// 错误，也不占用请求号。
	notFoundReq := revokeAuthzSaveReq("alice", "ghost", "rv-nf")
	res, err := r.RevokeAuthorization(notFoundReq)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("授权不存在应返回 ErrNotFound，不能改报保存错误: %v", err)
	}
	if res.AuthID != "ghost" || !errors.Is(res.Err, ErrNotFound) || res.Replayed {
		t.Fatalf("授权不存在的结果异常: %+v", res)
	}
	assertRevokeRequestFree(t, r, notFoundReq)

	// 保存条件恢复后，同一操作者用同一请求号撤销真实存在的授权应正常成功：
	// 引用不存在没有占用请求号。
	restoreBatchSave(t, r)
	res, err = r.RevokeAuthorization(revokeAuthzSaveReq("alice", "a1", "rv-nf"))
	if err != nil || res.Replayed || res.AuthID != "a1" || res.Status != AuthRevoked {
		t.Fatalf("引用不存在不应占用请求号，同号撤销真实授权应成功: %+v, err %v", res, err)
	}
}
