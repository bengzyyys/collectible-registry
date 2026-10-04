package registry

import "fmt"

// 单件发行（Issue）与整批发行（IssueBatch）共用的发行资格规则。
//
// 一次发行只有在以下条件全部成立时才能进行：
//   - 操作者账户已登记且可用；
//   - 系列存在、未封存，且操作者就是系列创建账户；
//   - 藏品编号从未被使用过；
//   - 初始持有人账户已登记且可用。
//
// 规则的每一步都由两个入口按各自的顺序编排：
//   - 单件发行按"操作者 → 系列（不存在 → 已封存 → 非创建账户）→ 藏品
//     编号 → 初始持有人"的顺序，拒绝结果仍带本次请求的藏品编号；
//   - 整批发行同样先判断操作者与系列（这两层问题不附具体藏品编号），
//     随后先对所有条目统一检查编号占用、再逐件检查初始持有人：因此跨
//     条目比较时编号占用始终优先于持有人问题，同一类问题内报告清单顺序
//     最前的那件。
//
// 参数合法性（必填内容、非空清单、清单内编号不重复）仍由两个入口各自的
// validatePresent 在进入资格检查前完成；规则共用后，两个入口的拒绝次序、
// 错误类型与藏品编号关联方式保持不变。

// issueOperatorEligibility 判断操作者账户是否已登记且可用。操作者的
// 未登记与停用问题在两个入口中都最先报告，且不关联具体藏品。
func (r *Registry) issueOperatorEligibility(operator string) error {
	op, ok := r.state.Accounts[operator]
	if !ok {
		return fmt.Errorf("%w: 操作者账户 %s", ErrNotFound, operator)
	}
	if !op.Active {
		return fmt.Errorf("%w: 操作者账户 %s", ErrAccountInactive, operator)
	}
	return nil
}

// issueSeriesEligibility 判断系列是否可由该操作者发行：先查存在，再查
// 是否封存，最后核对操作者是否为创建账户。藏品编号与初始持有人的问题
// 不能提前覆盖这三层拒绝。
func (r *Registry) issueSeriesEligibility(seriesID, operator string) error {
	s, ok := r.state.Series[seriesID]
	if !ok {
		return fmt.Errorf("%w: 系列 %s", ErrNotFound, seriesID)
	}
	if s.Sealed {
		return fmt.Errorf("%w: 系列 %s", ErrSeriesSealed, seriesID)
	}
	if s.CreatorID != operator {
		return fmt.Errorf("%w: 只有系列创建账户 %s 可以发行", ErrForbidden, s.CreatorID)
	}
	return nil
}

// issueItemAvailableEligibility 判断藏品编号是否尚未被占用。
func (r *Registry) issueItemAvailableEligibility(itemID string) error {
	if _, ok := r.state.Items[itemID]; ok {
		return fmt.Errorf("%w: 藏品编号 %s 已被使用", ErrAlreadyExists, itemID)
	}
	return nil
}

// issueHolderEligibility 判断初始持有人账户是否已登记且可用。两个入口的
// 拒绝信息相同；整批由编排层另行在结果中关联藏品编号。
func (r *Registry) issueHolderEligibility(holderID string) error {
	h, ok := r.state.Accounts[holderID]
	if !ok {
		return fmt.Errorf("%w: 初始持有人账户 %s", ErrNotFound, holderID)
	}
	if !h.Active {
		return fmt.Errorf("%w: 初始持有人账户 %s", ErrAccountInactive, holderID)
	}
	return nil
}

// checkSingleIssueEligibility 按单件发行的优先级编排共用规则：操作者的
// 登记/停用问题优先于系列问题；系列问题内部依次为不存在、已封存、操作
// 者不是创建账户；藏品编号已占用又优先于初始持有人的未登记或停用。
func (r *Registry) checkSingleIssueEligibility(operator, seriesID, itemID, holderID string) error {
	if err := r.issueOperatorEligibility(operator); err != nil {
		return err
	}
	if err := r.issueSeriesEligibility(seriesID, operator); err != nil {
		return err
	}
	if err := r.issueItemAvailableEligibility(itemID); err != nil {
		return err
	}
	return r.issueHolderEligibility(holderID)
}

// checkBatchIssueEligibility 按整批发行的优先级编排共用规则：操作者与
// 系列层面的问题最先报告且不关联任何藏品（返回的 itemID 为空）；这两层
// 通过后，先对全部条目检查编号占用，再按清单顺序逐件检查初始持有人——
// 因此即使前一件持有人未登记、后一件编号已占用，仍报告后一件编号被占用；
// 没有任何编号占用时，才报告清单顺序最前的持有人问题。任何一件不通过
// 则整批拒绝，返回值 itemID 指出问题所在的那件藏品。
func (r *Registry) checkBatchIssueEligibility(operator, seriesID string, entries []IssueBatchEntry) (string, error) {
	if err := r.issueOperatorEligibility(operator); err != nil {
		return "", err
	}
	if err := r.issueSeriesEligibility(seriesID, operator); err != nil {
		return "", err
	}
	for _, e := range entries {
		if err := r.issueItemAvailableEligibility(e.ItemID); err != nil {
			return e.ItemID, err
		}
	}
	for _, e := range entries {
		if err := r.issueHolderEligibility(e.HolderID); err != nil {
			return e.ItemID, err
		}
	}
	return "", nil
}
