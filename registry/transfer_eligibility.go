package registry

import "fmt"

// 直接转让（Transfer 与 TransferBatch）共用的资格规则。
//
// 一件藏品只有在以下条件全部成立时才能直接转让：
//   - 藏品及其持有记录存在；
//   - 操作者（当前持有人）账户已登记且可用；
//   - 接收账户已登记、可用，且与当前持有人不同；
//   - 期望持有人、期望版本与当前持有一致，且操作者就是当前持有人。
//
// 系列封存不妨碍已发行藏品转让，因此这里不检查封存状态。规则的每一步
// 都由两个入口按各自的顺序编排：单件转让按"藏品 → 操作者 → 接收人 →
// 收发同人 → 持有信息"的顺序；整批先查操作者，再逐件按"藏品 → 接收人
// → 收发同人 → 持有信息"的顺序。参数合法性（必填内容、正期望版本、
// 非负价款）仍由两个入口各自的 validatePresent 在进入资格检查前完成；
// 代转授权不使用这里的规则，沿用 ProxyTransfer 原有行为。

// directTransferEligibility 汇集一件藏品直接转让资格判断所需的全部输入，
// 与具体入口无关。
type directTransferEligibility struct {
	itemID        string // 藏品编号
	operator      string // 操作者，必须是当前持有人
	toID          string // 接收账户
	expectedOwner string // 期望当前持有人
	expectedVer   int64  // 期望当前持有版本
}

// itemTransferEligibility 判断藏品与持有记录是否存在。
// 单件与整批都在账户问题之前先判断各自引用的藏品；持有记录缺失同样按
// 对象不存在处理。
func (r *Registry) itemTransferEligibility(itemID string) error {
	if _, ok := r.state.Holdings[itemID]; !ok {
		if _, itemExists := r.state.Items[itemID]; !itemExists {
			return fmt.Errorf("%w: 藏品 %s", ErrNotFound, itemID)
		}
		return fmt.Errorf("%w: 藏品 %s 没有持有记录", ErrNotFound, itemID)
	}
	return nil
}

// operatorTransferEligibility 判断操作者账户是否已登记且可用。
func (r *Registry) operatorTransferEligibility(operator string) error {
	op, ok := r.state.Accounts[operator]
	if !ok {
		return fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, operator)
	}
	return nil
}

// recipientTransferEligibility 判断接收账户是否已登记且可用。两个入口的
// 拒绝信息相同；整批由编排层另行在结果中关联藏品编号。
func (r *Registry) recipientTransferEligibility(toID string) error {
	to, ok := r.state.Accounts[toID]
	if !ok {
		return fmt.Errorf("%w: 接收账户 %s", ErrNotFound, toID)
	}
	if !to.Active {
		return fmt.Errorf("%w: 接收账户 %s", ErrAccountInactive, toID)
	}
	return nil
}

// sameHolderEligibility 判断接收人是否就是当前持有人。两个入口共用
// ErrSameAccount；整批的错误信息指出藏品编号。
func sameHolderEligibility(toID, ownerID, itemID string, withItem bool) error {
	if toID != ownerID {
		return nil
	}
	if withItem {
		return fmt.Errorf("%w: 接收人 %s 已是藏品 %s 的当前持有人",
			ErrSameAccount, toID, itemID)
	}
	return fmt.Errorf("%w: 接收人 %s 已是当前持有人", ErrSameAccount, toID)
}

// holdingTransferEligibility 判断期望持有人、期望版本与当前持有是否一致，
// 以及操作者是否就是当前持有人。两个入口的拒绝信息相同；系列封存不阻止
// 转让，这里不检查封存状态。
func holdingTransferEligibility(q directTransferEligibility, ownerID string, ownerVer int64) error {
	if ownerID == q.expectedOwner && ownerVer == q.expectedVer && q.operator == ownerID {
		return nil
	}
	return fmt.Errorf("%w: 藏品 %s 当前为 %s 版本 %d",
		ErrConflict, q.itemID, ownerID, ownerVer)
}

// checkSingleTransferEligibility 按单件转让的优先级编排共用规则：
// 藏品及持有记录不存在优先于账户问题；操作者的登记和停用问题优先于
// 接收账户问题；接收账户问题优先于收发同人；收发同人又优先于持有信息
// 不符。例如操作者停用而藏品不存在时仍返回对象不存在；接收人就是当前
// 持有人且期望版本填错时，仍先返回收发同人。
func (r *Registry) checkSingleTransferEligibility(q directTransferEligibility) error {
	if err := r.itemTransferEligibility(q.itemID); err != nil {
		return err
	}
	if err := r.operatorTransferEligibility(q.operator); err != nil {
		return err
	}
	if err := r.recipientTransferEligibility(q.toID); err != nil {
		return err
	}
	h := r.state.Holdings[q.itemID]
	if err := sameHolderEligibility(q.toID, h.OwnerID, q.itemID, false); err != nil {
		return err
	}
	return holdingTransferEligibility(q, h.OwnerID, h.Version)
}

// checkBatchTransferEligibility 按整批转让的优先级编排共用规则：操作者
// 未登记或停用最先报告且不关联任何藏品（itemID 返回空）；操作者可用后，
// 按清单顺序逐件检查，只指出第一件失败的藏品与原因——藏品不存在先于
// 该件接收账户问题，接收账户问题先于收发同人，收发同人先于持有信息
// 不符，后面条目的错误不会提前。
func (r *Registry) checkBatchTransferEligibility(operator string, entries []TransferBatchEntry) (string, error) {
	if err := r.operatorTransferEligibility(operator); err != nil {
		return "", err
	}
	for _, e := range entries {
		q := directTransferEligibility{
			itemID: e.ItemID, operator: operator, toID: e.ToID,
			expectedOwner: e.ExpectedOwner, expectedVer: e.ExpectedVer,
		}
		if err := r.itemTransferEligibility(e.ItemID); err != nil {
			return e.ItemID, err
		}
		if err := r.recipientTransferEligibility(e.ToID); err != nil {
			return e.ItemID, err
		}
		h := r.state.Holdings[e.ItemID]
		if err := sameHolderEligibility(e.ToID, h.OwnerID, e.ItemID, true); err != nil {
			return e.ItemID, err
		}
		if err := holdingTransferEligibility(q, h.OwnerID, h.Version); err != nil {
			return e.ItemID, err
		}
	}
	return "", nil
}
