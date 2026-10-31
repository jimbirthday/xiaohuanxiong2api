package pool

import (
	"fmt"
	"strings"
)

const importErrorLimit = 100

// ImportOptions 控制批量导入的覆盖与预演语义。
type ImportOptions struct {
	Overwrite bool
	DryRun    bool
}

// ImportResult 是一次账号批量导入的分类统计。
type ImportResult struct {
	Total           int
	Added           int
	Updated         int
	Skipped         int
	Invalid         int
	Errors          []string
	ErrorsTruncated bool
}

// accountImportPlan 描述导入对现有账号的字段更新和需要追加的新账号。
// 提交时按 ID 就地更新，避免替换切片导致并发租约与在途计数失去关联。
type accountImportPlan struct {
	updates   map[string]*Account
	additions []*Account
}

// importHooks 只供包内测试制造确定性交错；回调在持有 writeMu、但不持有 mu
// 的写事务窗口内执行。生产导入始终使用零值。
type importHooks struct {
	beforeWrite func()
	afterWrite  func(error)
}

// ImportAccounts 批量新增/覆盖账号，并在一次持久化中落盘。
//
// 同一批次内重复 ID：overwrite=false 保留首条，后续计入 skipped；
// overwrite=true 保留末条，被替换的条目也计入 skipped。覆盖已有账号时只更新
// 凭证、名称、用户 ID 与附加字段，保留 enabled/priority/weight 等人工设置，
// 同时清除旧凭证导致的冷却与连续失败状态。
func (p *Pool) ImportAccounts(imported []*Account, opts ImportOptions) (ImportResult, error) {
	return p.importAccounts(imported, opts, importHooks{})
}

// importAccounts 在池级写事务内执行 disk-first 导入：先基于稳定快照生成
// 待写账号列表并原子落盘，成功后才按 ID 合并到内存。写盘失败时内存完全不变；
// Acquire/Release 不使用 writeMu，因此写盘期间仍可短暂更新运行期在途状态。
func (p *Pool) importAccounts(imported []*Account, opts ImportOptions, hooks importHooks) (ImportResult, error) {
	// 导入与其他持久化写路径共享 writeMu：分类、落盘和内存提交期间，Add/
	// Update/Remove/失败状态/Token 持久化都不能插队；Acquire/Release 不受影响。
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	base := cloneAccountListLocked(p.accounts)
	p.mu.Unlock()

	plan, result := stageAccountImport(base, imported, opts)
	if opts.DryRun || result.Added+result.Updated == 0 {
		return result, nil
	}

	next := buildImportSnapshot(base, plan)
	if hooks.beforeWrite != nil {
		hooks.beforeWrite()
	}
	err := p.writeAccountsLocked(next)
	if hooks.afterWrite != nil {
		hooks.afterWrite(err)
	}
	if err != nil {
		// 磁盘未改变时不碰内存；锁外的并发写会在解锁后基于旧内存继续执行，
		// 因而最终内存与磁盘都只包含这些并发变更，不会残留本次导入。
		return result, err
	}

	// disk-first 成功后只做内存提交；applyImportedFields 保留并发 Acquire 的
	// inFlight/leaseID，已有账号不会被整表替换。
	p.mu.Lock()
	applyImportPlanLocked(p, plan)
	p.mu.Unlock()
	return result, nil
}

// stageAccountImport 基于一份稳定快照完成导入分类，不修改池状态。
func stageAccountImport(base []*Account, imported []*Account, opts ImportOptions) (accountImportPlan, ImportResult) {
	result := ImportResult{Total: len(imported), Errors: []string{}}

	plan := accountImportPlan{
		updates:   make(map[string]*Account),
		additions: make([]*Account, 0, len(imported)),
	}
	existing := make(map[string]*Account, len(base))
	positions := make(map[string]int, len(imported))
	staged := make(map[string]bool, len(imported))
	for _, account := range base {
		if account == nil {
			continue
		}
		if _, ok := existing[account.ID]; !ok {
			existing[account.ID] = account
		}
	}

	for index, incoming := range imported {
		if incoming == nil {
			addImportIssue(&result, "", fmt.Sprintf("第 %d 项账号为空", index+1), true)
			continue
		}
		id := strings.TrimSpace(incoming.ID)
		incoming = incoming.clone()
		incoming.ID = id
		if id == "" || strings.TrimSpace(incoming.AccessToken) == "" {
			addImportIssue(&result, id, fmt.Sprintf("第 %d 项缺少 id 或 access_token", index+1), true)
			continue
		}

		if staged[id] {
			if !opts.Overwrite {
				result.Skipped++
				addImportIssue(&result, id, "同一批次存在重复 id，保留首条", false)
				continue
			}
			if original := existing[id]; original != nil {
				plan.updates[id] = mergeImportedAccount(original, incoming)
			} else {
				plan.additions[positions[id]] = prepareImportedAccount(incoming)
			}
			result.Skipped++
			addImportIssue(&result, id, "同一批次存在重复 id，以后一条为准", false)
			continue
		}

		if original := existing[id]; original != nil {
			if !opts.Overwrite {
				staged[id] = true
				result.Skipped++
				addImportIssue(&result, id, "账号已存在，未覆盖", false)
				continue
			}
			plan.updates[id] = mergeImportedAccount(original, incoming)
			staged[id] = true
			result.Updated++
			continue
		}

		positions[id] = len(plan.additions)
		staged[id] = true
		plan.additions = append(plan.additions, prepareImportedAccount(incoming))
		result.Added++
	}
	return plan, result
}

// cloneAccountListLocked 深拷贝账号列表；调用方必须持有 p.mu。
func cloneAccountListLocked(accounts []*Account) []*Account {
	if accounts == nil {
		return nil
	}
	cloned := make([]*Account, len(accounts))
	for index, account := range accounts {
		cloned[index] = account.clone()
	}
	return cloned
}

// applyImportPlanLocked 按 ID 更新现有账号并追加新账号；调用方必须持有 p.mu。
// 更新只替换持久化字段，不触碰在途计数与租约标识。
func applyImportPlanLocked(p *Pool, plan accountImportPlan) {
	for id, imported := range plan.updates {
		if current := p.findLocked(id); current != nil {
			applyImportedFields(current, imported)
		}
	}
	for _, account := range plan.additions {
		p.accounts = append(p.accounts, account.clone())
	}
}

// applyImportedFields 复制账号字段但保留目标的运行期租约状态。
func applyImportedFields(target, source *Account) {
	inFlight := target.inFlight
	leaseID := target.leaseID
	lastUsedAt := target.LastUsedAt
	cloned := source.clone()
	*target = *cloned
	target.inFlight = inFlight
	target.leaseID = leaseID
	target.LastUsedAt = lastUsedAt
}

// buildImportSnapshot 从稳定基线生成待写快照，不修改池内账号或在途状态。
func buildImportSnapshot(base []*Account, plan accountImportPlan) []*Account {
	next := cloneAccountListLocked(base)
	for id, imported := range plan.updates {
		for _, current := range next {
			if current != nil && current.ID == id {
				applyImportedFields(current, imported)
				break
			}
		}
	}
	for _, account := range plan.additions {
		next = append(next, account.clone())
	}
	return next
}

func prepareImportedAccount(account *Account) *Account {
	if account.Weight <= 0 {
		account.Weight = 100
	}
	account.inFlight = 0
	account.CooldownUntil = 0
	account.ConsecutiveFails = 0
	account.LastFailureAt = 0
	account.LastError = ""
	return account
}

func mergeImportedAccount(original, incoming *Account) *Account {
	merged := original.clone()
	if incoming.Name != "" {
		merged.Name = incoming.Name
	}
	if incoming.UserID != "" {
		merged.UserID = incoming.UserID
	}
	merged.AccessToken = incoming.AccessToken
	if incoming.RefreshToken != "" {
		merged.RefreshToken = incoming.RefreshToken
	}
	merged.ExpiresAt = incoming.ExpiresAt
	for key, value := range incoming.extra {
		merged.SetExtra(key, value)
	}
	merged.CooldownUntil = 0
	merged.ConsecutiveFails = 0
	merged.LastFailureAt = 0
	merged.LastError = ""
	return merged
}

func addImportIssue(result *ImportResult, id, message string, invalid bool) {
	if invalid {
		result.Invalid++
	}
	if len(result.Errors) >= importErrorLimit {
		result.ErrorsTruncated = true
		return
	}
	if id != "" {
		result.Errors = append(result.Errors, safeImportID(id)+": "+message)
		return
	}
	result.Errors = append(result.Errors, message)
}

func safeImportID(id string) string {
	id = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, id)
	runes := []rune(id)
	if len(runes) > 80 {
		return string(runes[:80]) + "…"
	}
	return id
}
