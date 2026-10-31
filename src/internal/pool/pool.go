// Package pool 实现 §5 的账号池：加载/回写账号文件、选号、单飞刷新、冷却。
//
// 硬约束：持锁期间不做网络 IO、不写盘、不打日志（SPEC §2.6）。
// 因此网络刷新与文件回写都在 mu 外完成；状态变更用主 mutex 保护，所有会落盘
// 的写路径用 writeMu 串行化。Acquire/Release 只短暂持有 mu，不等待写盘。
package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"xhx2api/internal/logx"
	"xhx2api/internal/upstream"
)

// 账号池写操作错误（SPEC4 §G.2）。
var (
	// ErrAccountExists 表示要新增的账号 id 已存在。
	ErrAccountExists = errors.New("账号 id 已存在")
	// ErrAccountNotFound 表示账号不存在。
	ErrAccountNotFound = errors.New("账号不存在")
)

// Account 是账号文件里的一条账号记录。
//
// 未知字段通过 extra 原样保留，回写时不丢（SPEC §5.1）。
type Account struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	UserID           string `json:"user_id"`
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresAt        int64  `json:"expires_at"`
	Enabled          bool   `json:"enabled"`
	Priority         int    `json:"priority"`
	Weight           int    `json:"weight"`
	LastUsedAt       int64  `json:"last_used_at"`
	ConsecutiveFails int    `json:"consecutive_fails"`
	LastFailureAt    int64  `json:"last_failure_at"`
	LastRefreshAt    int64  `json:"last_refresh_at"`
	CooldownUntil    int64  `json:"cooldown_until"`
	LastError        string `json:"last_error"`

	// 在途请求只存在内存，不随账号文件持久化。
	inFlight int
	// leaseID 标识一次 Acquire；只存在本次返回的快照中，用于幂等 Release。
	leaseID uint64

	extra map[string]json.RawMessage
}

// UnmarshalJSON 保留全部原始字段（未知字段回写时不丢）。
func (a *Account) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	type alias Account
	var tmp alias
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}
	*a = Account(tmp)
	if _, ok := raw["enabled"]; !ok {
		a.Enabled = true // 缺省启用
	}
	if a.Weight <= 0 {
		a.Weight = 100 // weight 缺省或非法时按默认权重参与调度
	}
	a.extra = raw
	return nil
}

// MarshalJSON 输出已知字段 + 未知字段。
func (a Account) MarshalJSON() ([]byte, error) {
	type alias Account
	base, err := json.Marshal(alias(a))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	for key, value := range a.extra {
		if _, ok := fields[key]; !ok {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

func (a *Account) clone() *Account {
	if a == nil {
		return nil
	}
	dup := *a
	if a.extra != nil {
		dup.extra = make(map[string]json.RawMessage, len(a.extra))
		for key, value := range a.extra {
			dup.extra[key] = value
		}
	}
	return &dup
}

// SetExtra 设置一个未知字段（回写账号文件时原样保留）。
//
// 网页登录换回的 office_identity / office_org_name / office_org_role 用它落盘
// （SPEC5 §K2），与账号文件里既有的未知字段走同一份存储。
func (a *Account) SetExtra(key string, value json.RawMessage) {
	if a.extra == nil {
		a.extra = map[string]json.RawMessage{}
	}
	a.extra[key] = value
}

// Config 是账号池配置（由 §3 accounts 换算）。
type Config struct {
	File                       string
	Strategy                   string
	RefreshMarginSeconds       int
	CooldownSeconds            int
	MaxInflightPerAccount      int
	ServerErrorCooldownSeconds int
	FailureDecaySeconds        int
	CooldownMaxSeconds         int
	MaxAttempts                int
	RefreshOn401               bool
}

// Pool 是进程内共享的账号池。
type Pool struct {
	mu sync.Mutex
	// writeMu 串行化持久化写事务：修改内存、生成待写快照和原子落盘必须整体
	// 与其他持久化写互斥，避免导入写盘期间其他写路径插队导致内存/磁盘分叉。
	writeMu sync.Mutex

	cfg    Config
	client *upstream.Client
	logger *logx.Logger

	accounts  []*Account
	rrWeight  map[string]int64
	inflight  map[string]*refreshFlight
	nextLease uint64
	leases    map[uint64]string
}

type refreshFlight struct {
	done      chan struct{}
	access    string
	refresh   string
	expiresAt int64
	err       error
}

// New 构造账号池并加载账号文件（文件不存在只警告，不失败）。
func New(cfg Config, client *upstream.Client, logger *logx.Logger) *Pool {
	p := &Pool{
		cfg:      cfg,
		client:   client,
		logger:   logger,
		rrWeight: map[string]int64{},
		inflight: map[string]*refreshFlight{},
		leases:   map[uint64]string{},
	}
	p.load()
	return p
}

func (p *Pool) load() {
	data, err := os.ReadFile(p.cfg.File)
	if err != nil {
		if os.IsNotExist(err) {
			p.logger.Warnf("[pool] 警告：账号文件 %s 不存在，服务将以空账号池启动（任何 /v1 请求将返回 503）", p.cfg.File)
			return
		}
		p.logger.Warnf("[pool] 警告：读取账号文件失败（%s）：%v", p.cfg.File, err)
		return
	}
	var file struct {
		Accounts []*Account `json:"accounts"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		p.logger.Warnf("[pool] 警告：解析账号文件失败（%s）：%v", p.cfg.File, err)
		return
	}
	p.accounts = file.Accounts
	for _, account := range p.accounts {
		if account.Weight <= 0 {
			account.Weight = 100
		}
	}
}

// persistLocked 把当前账号池原子写回文件；调用方必须持有 writeMu。
// 快照只在 mu 内复制，序列化与文件 IO 均在 mu 外完成。
func (p *Pool) persistLocked() error {
	p.mu.Lock()
	accounts := cloneAccountListLocked(p.accounts)
	p.mu.Unlock()
	return p.writeAccountsLocked(accounts)
}

// writeAccountsLocked 将给定快照原子写回文件（临时文件 + rename，权限 0600）。
// 调用方必须持有 writeMu；本函数不访问 p.accounts，也不持有 mu。
func (p *Pool) writeAccountsLocked(accounts []*Account) error {
	payload := struct {
		Accounts []*Account `json:"accounts"`
	}{Accounts: accounts}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		p.logger.Warnf("[pool] 警告：序列化账号文件失败：%v", err)
		return fmt.Errorf("序列化账号文件失败：%v", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(p.cfg.File)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			p.logger.Warnf("[pool] 警告：创建账号目录失败（%s）：%v", dir, err)
			return fmt.Errorf("创建账号目录失败：%v", err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".accounts-*.tmp")
	if err != nil {
		p.logger.Warnf("[pool] 警告：创建临时账号文件失败：%v", err)
		return fmt.Errorf("创建临时账号文件失败：%v", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：写入临时账号文件失败：%v", err)
		return fmt.Errorf("写入临时账号文件失败：%v", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：关闭临时账号文件失败：%v", err)
		return fmt.Errorf("关闭临时账号文件失败：%v", err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		p.logger.Warnf("[pool] 警告：设置账号文件权限失败：%v", err)
	}
	if err := os.Rename(tmpName, p.cfg.File); err != nil {
		os.Remove(tmpName)
		p.logger.Warnf("[pool] 警告：替换账号文件失败：%v", err)
		return fmt.Errorf("替换账号文件失败：%v", err)
	}
	return nil
}

func (p *Pool) findLocked(id string) *Account {
	for _, account := range p.accounts {
		if account.ID == id {
			return account
		}
	}
	return nil
}

func (p *Pool) expiring(expiresAt int64, now int64) bool {
	if expiresAt <= 0 {
		return false // 过期时间未知 → 不主动刷新，靠 401 触发
	}
	margin := int64(p.cfg.RefreshMarginSeconds) * 1000
	return now+margin >= expiresAt
}

func effectiveWeight(account *Account) int64 {
	if account.Weight <= 0 {
		return 100 // 账号文件缺省 weight 时保持旧行为：等权
	}
	return int64(account.Weight)
}

// failureTierLocked 把仍处于失败衰减窗口内的账号降层；成功或衰减后回到健康层。
func (p *Pool) failureTierLocked(account *Account, now int64) int {
	if account.ConsecutiveFails <= 0 {
		return 0
	}
	decay := int64(p.cfg.FailureDecaySeconds) * 1000
	if decay <= 0 || account.LastFailureAt <= 0 || now-account.LastFailureAt >= decay {
		return 0
	}
	return 1
}

func (p *Pool) eligibleLocked(account *Account, exclude map[string]bool, now int64) bool {
	if !account.Enabled || account.AccessToken == "" || account.CooldownUntil > now {
		return false
	}
	if exclude != nil && exclude[account.ID] {
		return false
	}
	if p.cfg.MaxInflightPerAccount > 0 && account.inFlight >= p.cfg.MaxInflightPerAccount {
		return false
	}
	return true
}

// pickRoundRobinLocked 用平滑加权轮转分配请求，并以 LRU 作为同权重候选的平局依据。
//
// 只对失败层级最低的候选调用，避免连续失败账号抢占健康账号。
func (p *Pool) pickRoundRobinLocked(candidates []*Account) *Account {
	if p.rrWeight == nil {
		p.rrWeight = map[string]int64{}
	}
	var total int64
	for _, account := range candidates {
		total += effectiveWeight(account)
	}
	if total <= 0 {
		return candidates[0]
	}

	var chosen *Account
	var chosenCurrent int64
	for _, account := range candidates {
		current := p.rrWeight[account.ID] + effectiveWeight(account)
		p.rrWeight[account.ID] = current
		if chosen == nil || current > chosenCurrent ||
			(current == chosenCurrent && account.LastUsedAt < chosen.LastUsedAt) {
			chosen = account
			chosenCurrent = current
		}
	}
	p.rrWeight[chosen.ID] -= total
	return chosen
}

// pickLocked 在锁内完成过滤、分层、排序与 last_used 更新；acquire 时同时占用在途名额。
func (p *Pool) pickLocked(exclude map[string]bool, acquire bool) *Account {
	now := time.Now().UnixMilli()
	candidates := make([]*Account, 0, len(p.accounts))
	bestTier := 1
	for _, account := range p.accounts {
		if !p.eligibleLocked(account, exclude, now) {
			continue
		}
		tier := p.failureTierLocked(account, now)
		if tier < bestTier {
			bestTier = tier
		}
		candidates = append(candidates, account)
	}
	if len(candidates) == 0 {
		return nil
	}

	// 健康账号始终优先；只有健康层不可用时才进入失败衰减层。
	filtered := candidates[:0]
	for _, account := range candidates {
		if p.failureTierLocked(account, now) == bestTier {
			filtered = append(filtered, account)
		}
	}
	candidates = filtered

	var chosen *Account
	if p.cfg.Strategy == "round_robin" {
		chosen = p.pickRoundRobinLocked(candidates)
	} else {
		sort.SliceStable(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]
			if left.Priority != right.Priority {
				return left.Priority < right.Priority
			}
			if left.LastUsedAt != right.LastUsedAt {
				return left.LastUsedAt < right.LastUsedAt
			}
			return effectiveWeight(left) > effectiveWeight(right)
		})
		chosen = candidates[0]
	}
	chosen.LastUsedAt = now
	result := chosen.clone()
	if acquire {
		chosen.inFlight++
		p.nextLease++
		result.leaseID = p.nextLease
		p.leases[p.nextLease] = chosen.ID
	}
	return result
}

// Pick 选一个可用账号但不占用在途名额，保留给只读选号或兼容调用。
func (p *Pool) Pick(exclude map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickLocked(exclude, false)
}

// Acquire 原子地选号并增加账号在途计数；调用方必须保证每条路径恰好 Release 一次。
func (p *Pool) Acquire(exclude map[string]bool) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pickLocked(exclude, true)
}

// Release 归还一次 Acquire 租约。重复调用同一租约是 no-op，不会误减其他在途请求。
func (p *Pool) Release(acquired *Account) {
	if acquired == nil || acquired.leaseID == 0 {
		return
	}
	p.mu.Lock()
	id, ok := p.leases[acquired.leaseID]
	if !ok {
		p.mu.Unlock()
		return
	}
	delete(p.leases, acquired.leaseID)
	account := p.findLocked(id)
	if account != nil && account.inFlight > 0 {
		account.inFlight--
	}
	p.mu.Unlock()
}

// RecordSuccess 清除连续失败与最近失败时间，使账号回到健康层。
func (p *Pool) RecordSuccess(id string) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return
	}
	account.ConsecutiveFails = 0
	account.LastFailureAt = 0
	account.LastError = ""
	p.mu.Unlock()
	_ = p.persistLocked()
}

func (p *Pool) failureCooldown(consecutiveFails int) time.Duration {
	base := time.Duration(p.cfg.ServerErrorCooldownSeconds) * time.Second
	if base <= 0 {
		return 0
	}
	cooldown := base
	for i := 1; i < consecutiveFails && i < 32; i++ {
		if cooldown > time.Duration(1<<62) {
			break
		}
		cooldown *= 2
		limit := time.Duration(p.cfg.CooldownMaxSeconds) * time.Second
		if limit > 0 && cooldown >= limit {
			return limit
		}
	}
	limit := time.Duration(p.cfg.CooldownMaxSeconds) * time.Second
	if limit > 0 && cooldown > limit {
		return limit
	}
	return cooldown
}

// RecordFailure 累计连续失败并按指数退避写入冷却，返回本次冷却时长。
func (p *Pool) RecordFailure(id string) time.Duration {
	now := time.Now().UnixMilli()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return 0
	}
	decay := int64(p.cfg.FailureDecaySeconds) * 1000
	if account.ConsecutiveFails <= 0 || account.LastFailureAt <= 0 || decay <= 0 || now-account.LastFailureAt >= decay {
		account.ConsecutiveFails = 1
	} else {
		account.ConsecutiveFails++
	}
	account.LastFailureAt = now
	cooldown := p.failureCooldown(account.ConsecutiveFails)
	if cooldown > 0 {
		until := now + cooldown.Milliseconds()
		if until > account.CooldownUntil {
			account.CooldownUntil = until
		}
	}
	account.LastError = "上游连续失败 " + fmt.Sprint(account.ConsecutiveFails) + " 次"
	p.mu.Unlock()
	_ = p.persistLocked()
	return cooldown
}

// CooldownFor 以指定时长冷却账号，并受 cooldown_max_seconds 封顶。
func (p *Pool) CooldownFor(id string, duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	if limit := time.Duration(p.cfg.CooldownMaxSeconds) * time.Second; limit > 0 && duration > limit {
		duration = limit
	}
	now := time.Now().UnixMilli()
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return
	}
	until := now + duration.Milliseconds()
	if until > account.CooldownUntil {
		account.CooldownUntil = until
	}
	account.LastError = "上游返回 429，账号已冷却"
	p.mu.Unlock()
	_ = p.persistLocked()
}

// MinCooldownRemaining 返回给定账号中最小的正剩余冷却时长；均未冷却时返回 0。
func (p *Pool) MinCooldownRemaining(ids []string) time.Duration {
	now := time.Now().UnixMilli()
	p.mu.Lock()
	defer p.mu.Unlock()
	var minimum time.Duration
	found := false
	for _, id := range ids {
		account := p.findLocked(id)
		if account == nil {
			continue
		}
		remaining := time.Duration(account.CooldownUntil-now) * time.Millisecond
		if remaining <= 0 {
			continue
		}
		if !found || remaining < minimum {
			minimum = remaining
			found = true
		}
	}
	return minimum
}

// Token 返回指定账号当前可用的 access_token。
//
// force=false 时仅在临期（expires_at > 0 且 now+margin >= expires_at）时刷新；
// force=true 时无条件刷新（401 后的强制刷新）。刷新失败不覆盖旧 token，记录
// last_error 并返回错误。同一账号的并发刷新只发一次真实请求（单飞）。
func (p *Pool) Token(ctx context.Context, id string, force bool) (string, error) {
	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return "", fmt.Errorf("账号 %s 不存在", id)
	}
	token := account.AccessToken
	refreshToken := account.RefreshToken
	expiresAt := account.ExpiresAt
	needRefresh := force || p.expiring(expiresAt, time.Now().UnixMilli())
	if !needRefresh || refreshToken == "" {
		p.mu.Unlock()
		return token, nil
	}
	// 单飞：已有进行中的刷新则等待其结果。
	if flight, ok := p.inflight[id]; ok {
		p.mu.Unlock()
		p.logger.Debugf("[pool] 账号 %s 复用进行中的刷新（单飞等待）", id)
		<-flight.done
		if flight.err != nil {
			return "", flight.err
		}
		return flight.access, nil
	}
	flight := &refreshFlight{done: make(chan struct{})}
	p.inflight[id] = flight
	p.mu.Unlock()

	access, newRefresh, err := p.client.Refresh(ctx, refreshToken)

	// 网络刷新完成后才进入写事务；写锁覆盖状态更新与落盘，导入写盘期间
	// 不会插入一次只改内存但尚未持久化的 token 更新。
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	if err == nil {
		if account := p.findLocked(id); account != nil {
			account.AccessToken = access
			if newRefresh != "" {
				account.RefreshToken = newRefresh
			}
			if exp := upstream.JWTExpiry(access); exp > 0 {
				account.ExpiresAt = exp
			}
			account.LastRefreshAt = time.Now().UnixMilli()
			account.LastError = ""
			flight.access = access
		} else {
			err = fmt.Errorf("账号 %s 在刷新期间被移除", id)
		}
	} else if account := p.findLocked(id); account != nil {
		account.LastError = err.Error()
	}
	flight.err = err
	delete(p.inflight, id)
	close(flight.done)
	p.mu.Unlock()

	_ = p.persistLocked()
	if err != nil {
		return "", err
	}
	return access, nil
}

// Cooldown 把账号冷却 cooldown_seconds。
func (p *Pool) Cooldown(id string) {
	p.CooldownFor(id, time.Duration(p.cfg.CooldownSeconds)*time.Second)
}

// MarkError 记录账号最近一次错误（不冷却）。
func (p *Pool) MarkError(id, message string) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	if account := p.findLocked(id); account != nil {
		account.LastError = message
	}
	p.mu.Unlock()
	_ = p.persistLocked()
}

// FirstToken 返回首个启用账号的 access_token（用于目录刷新等只读动作）；没有则空串。
func (p *Pool) FirstToken() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, account := range p.accounts {
		if account.Enabled && account.AccessToken != "" {
			return account.AccessToken
		}
	}
	return ""
}

// Counts 返回 total/enabled/available/cooldown 计数。
func (p *Pool) Counts() (total, enabled, available, cooldown int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixMilli()
	for _, account := range p.accounts {
		total++
		if !account.Enabled {
			continue
		}
		enabled++
		if account.CooldownUntil > now {
			cooldown++
			continue
		}
		if account.AccessToken != "" {
			available++
		}
	}
	return
}

// AccountView 是账号的只读快照（供 /healthz、§7.1 使用）。
type AccountView struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	UserID           string `json:"user_id"`
	Enabled          bool   `json:"enabled"`
	Priority         int    `json:"priority"`
	Weight           int    `json:"weight"`
	LastUsedAt       int64  `json:"last_used_at"`
	ConsecutiveFails int    `json:"consecutive_fails"`
	InFlight         int    `json:"inflight"`
	TokenTail        string `json:"token_tail"`
	ExpiresAt        int64  `json:"expires_at"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
	Expired          bool   `json:"expired"`
	CooldownUntil    int64  `json:"cooldown_until"`
	LastRefreshAt    int64  `json:"last_refresh_at"`
	LastError        string `json:"last_error"`
	HasRefreshToken  bool   `json:"has_refresh_token"`
}

// viewOf 由账号构造只读快照（Views / View / 写接口响应复用同一份）。
func viewOf(account *Account, now int64) AccountView {
	view := AccountView{
		ID:               account.ID,
		Name:             account.Name,
		UserID:           account.UserID,
		Enabled:          account.Enabled,
		Priority:         account.Priority,
		Weight:           int(effectiveWeight(account)),
		LastUsedAt:       account.LastUsedAt,
		ConsecutiveFails: account.ConsecutiveFails,
		InFlight:         account.inFlight,
		TokenTail:        upstream.TokenTail(account.AccessToken),
		ExpiresAt:        account.ExpiresAt,
		CooldownUntil:    account.CooldownUntil,
		LastRefreshAt:    account.LastRefreshAt,
		LastError:        account.LastError,
		HasRefreshToken:  account.RefreshToken != "",
	}
	if account.ExpiresAt > 0 {
		view.ExpiresInSeconds = (account.ExpiresAt - now) / 1000
		view.Expired = account.ExpiresAt <= now
	}
	return view
}

// Views 返回全部账号的只读快照。
func (p *Pool) Views() []AccountView {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixMilli()
	views := make([]AccountView, 0, len(p.accounts))
	for _, account := range p.accounts {
		views = append(views, viewOf(account, now))
	}
	return views
}

// View 返回单个账号的只读快照；不存在返回 false。
func (p *Pool) View(id string) (AccountView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.findLocked(id)
	if account == nil {
		return AccountView{}, false
	}
	return viewOf(account, time.Now().UnixMilli()), true
}

// AccountPatch 描述一次账号更新；nil 字段保持不变（SPEC4 §G.2）。
type AccountPatch struct {
	Name         *string
	Priority     *int
	Enabled      *bool
	AccessToken  *string
	RefreshToken *string
}

// Add 新增账号并原子回写；id 冲突返回 ErrAccountExists。持锁期间不写盘。
func (p *Pool) Add(account *Account) error {
	if account == nil {
		return errors.New("账号为空")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	if p.findLocked(account.ID) != nil {
		p.mu.Unlock()
		return ErrAccountExists
	}
	dup := account.clone()
	if dup.Weight <= 0 {
		dup.Weight = 100
	}
	p.accounts = append(p.accounts, dup)
	p.mu.Unlock()
	return p.persistLocked()
}

// Upsert 按 id 新增或更新账号（SPEC5 §K2 网页登录落账号）。
//
// 已存在时只更新 token / expires_at / user_id 与未知字段（office_*），
// 保留 name / enabled / priority 等用户设置；返回 updated=true。
// 不存在时按新增处理，返回 updated=false。持锁期间不写盘。
func (p *Pool) Upsert(account *Account) (bool, error) {
	if account == nil {
		return false, errors.New("账号为空")
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	existing := p.findLocked(account.ID)
	updated := existing != nil
	if updated {
		existing.AccessToken = account.AccessToken
		if account.RefreshToken != "" {
			existing.RefreshToken = account.RefreshToken
		}
		existing.ExpiresAt = account.ExpiresAt
		existing.UserID = account.UserID
		existing.LastError = ""
		for key, value := range account.extra {
			if existing.extra == nil {
				existing.extra = map[string]json.RawMessage{}
			}
			existing.extra[key] = value
		}
	} else {
		dup := account.clone()
		if dup.Weight <= 0 {
			dup.Weight = 100
		}
		p.accounts = append(p.accounts, dup)
	}
	p.mu.Unlock()
	return updated, p.persistLocked()
}

// Update 只改给出的字段并原子回写；账号不存在返回 ErrAccountNotFound。
//
// 更新 access_token 时同步重算 expires_at 与 user_id（SPEC4 §G.2）。
func (p *Pool) Update(id string, patch AccountPatch) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return ErrAccountNotFound
	}
	if patch.Name != nil {
		account.Name = *patch.Name
	}
	if patch.Priority != nil {
		account.Priority = *patch.Priority
	}
	if patch.Enabled != nil {
		account.Enabled = *patch.Enabled
	}
	if patch.RefreshToken != nil {
		account.RefreshToken = *patch.RefreshToken
	}
	if patch.AccessToken != nil {
		account.AccessToken = *patch.AccessToken
		account.ExpiresAt = upstream.JWTExpiry(*patch.AccessToken)
		account.UserID = upstream.JWTUserID(*patch.AccessToken)
	}
	p.mu.Unlock()
	return p.persistLocked()
}

// Remove 删除账号并原子回写；账号不存在返回 ErrAccountNotFound。
func (p *Pool) Remove(id string) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()

	p.mu.Lock()
	index := -1
	for i, account := range p.accounts {
		if account.ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		p.mu.Unlock()
		return ErrAccountNotFound
	}
	p.accounts = append(p.accounts[:index], p.accounts[index+1:]...)
	delete(p.rrWeight, id)
	p.mu.Unlock()
	return p.persistLocked()
}

// TestToken 用账号当前 token 只读地打一次上游目录，返回状态码与模型条数。
//
// 不刷新 token、不写回（SPEC4 §G.2）。账号不存在返回 ErrAccountNotFound。
func (p *Pool) TestToken(ctx context.Context, id string) (int, int, error) {
	p.mu.Lock()
	account := p.findLocked(id)
	if account == nil {
		p.mu.Unlock()
		return 0, 0, ErrAccountNotFound
	}
	token := account.AccessToken
	p.mu.Unlock()
	if token == "" {
		return 0, 0, errors.New("账号没有可用 token")
	}
	return p.client.CatalogProbe(ctx, token)
}

// TokenFor 返回账号当前 token（不刷新），供只读查询使用；不存在返回空串。
func (p *Pool) TokenFor(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if account := p.findLocked(id); account != nil {
		return account.AccessToken
	}
	return ""
}
