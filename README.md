# xhx2api — 小浣熊 OpenAI 兼容反代

把商汤「小浣熊」（`xiaohuanxiong.com`）网页端 / 桌面端所依赖的 LLM 网关，逆向成本地一个
**OpenAI 兼容**的 HTTP 反代：客户端用 OpenAI SDK / `curl` 指向本服务，请求会被转发到小浣熊上游
（含 OpenAI 多模态 `content` 数组，也就是可以直接带图片），响应（含流式、思维链、function calling、usage）原样回传。

- **上游**：商汤小浣熊官方接口 `https://xiaohuanxiong.com`（使用你自己的账号登录后取得的 token）。
- **用途**：**仅个人学习与自用**。本项目与商汤科技（SenseTime）**没有任何关系**，非官方、未获授权。
  请遵守上游服务条款，不要用于商业用途、批量注册、转售或任何滥用行为；由此产生的一切后果由使用者自负。

> 本仓库只包含反代程序与部署文件，**不包含**任何账号、token 或密钥。

---

## 目录

- [快速开始](#快速开始)
- [配置项说明](#配置项说明)
- [认证与安全](#认证与安全)
- [账号文件与 token 获取](#账号文件与-token-获取)
- [HTTP 端点](#http-端点)
- [图片输入（多模态）](#图片输入多模态)
- [管理面板](#管理面板)
- [批量导入账号](#批量导入账号)
- [每日签到](#每日签到)
- [测试](#测试)
- [限制与排障](#限制与排障)

---

## 快速开始

### 1. 准备配置

```bash
cp config.example.json config.json
# 编辑 config.json（至少确认监听地址；api_key 可留空，/v1 仍会用首次启动生成的 API Key 鉴权）
```

`config.json` 支持 JSONC（`//`、`/* */` 注释与尾逗号）。**写错键名会启动失败**（未知键会报错）。

### 2. 准备账号

```bash
mkdir -p data
cp data/accounts.example.json data/accounts.json 2>/dev/null || cat > data/accounts.json <<'EOF'
{
  "accounts": [
    {
      "id": "user-你的ID",
      "name": "小浣熊-1",
      "user_id": "你的ID",
      "access_token": "eyJhbGci…",
      "refresh_token": "eyJhbGci…",
      "expires_at": 0,
      "enabled": true,
      "priority": 100,
      "weight": 100
    }
  ]
}
EOF
```

token 的获取方式见 [账号文件与 token 获取](#账号文件与-token-获取)。

### 3. 启动

```bash
# 容器以非 root 用户（uid 10001）运行，data/ 需要可写：首次部署与每次新建 data/ 后都要做一次
sudo chown -R 10001:10001 data

docker compose up -d --build
docker compose logs -f          # 看到「生效配置」与「版本=… 监听=… 账号=…/… 目录=…」即启动成功
```

首次启动会在 `admin.auth_file`（默认 `./data/auth.json`）中创建唯一管理员 `admin`，并生成随机密码与
`xhx_` 开头的 API Key。两份明文只写入认证文件同目录的 `initial-credentials.txt`（默认
`data/initial-credentials.txt`，权限 `0600`）；启动日志只打印该文件绝对路径。读取凭据并登录面板后，
请立即删除该文件。若在首次启动前设置 `XHX_ADMIN_PASSWORD`，密码优先取该环境变量（去空白后至少 12 个字符）。

> 漏掉 `chown` 的表现：服务能起来、`/healthz` 正常，但 `[pool] 警告：…账号文件…` 反复出现，
> 且刷新出来的 token 不会持久化（重启后回到旧 token）。

### 4. 验证（可直接复制）

```bash
# 从 data/initial-credentials.txt 取得 API Key；也可以用 config.json 的 api_key
API_KEY='<initial-credentials.txt 中的 API Key>'

# 健康检查
curl -s http://127.0.0.1:3066/healthz

# 模型列表（OpenAI 形状）
curl -s -H "Authorization: Bearer $API_KEY" http://127.0.0.1:3066/v1/models | head -c 400

# 一条流式 chat（模型名直接用上游 id，如 sn-glm-5-3-flash）
curl -sN -H 'Content-Type: application/json' -H "Authorization: Bearer $API_KEY" \
  -d '{"model":"sn-glm-5-3-flash","messages":[{"role":"user","content":"你好"}],"stream":true}' \
  http://127.0.0.1:3066/v1/chat/completions

# 浏览器打开管理面板
#   http://127.0.0.1:3066/admin
```

> `/v1` 永不接受匿名请求：Bearer 必须匹配 `config.json` 的 `api_key`，或匹配 `auth.json` 中保存的 API Key。
> 管理面板通过用户名/密码登录；仅需兼容旧客户端时，才使用 `admin.api_key` 的
> `Authorization: Bearer`。旧式 `?key=` 只能用于 GET，不能用于写请求。

---

## 配置项说明

下表列出 `config.example.json` 中的**每一个键**（默认值即模板中的值；按段分组）。

### 顶层

| 键 | 默认值 | 含义 |
|---|---|---|
| `listen` | `"0.0.0.0:3066"` | 监听地址与端口；非法（无法解析或端口越界）会报错退出。 |
| `api_key` | `""` | `/v1/*` 的兼容 Bearer 密钥。为空时仍校验认证文件中的 API Key；任何情况下都不会匿名放行。 |
| `shutdown_timeout_seconds` | `10` | 收到 `SIGINT`/`SIGTERM` 后优雅退出的超时（秒）。 |

### `upstream`

| 键 | 默认值 | 含义 |
|---|---|---|
| `llm_base` | `"https://xiaohuanxiong.com/api/web/llm/v2"` | LLM 网关基址（chat、模型目录）。 |
| `auth_base` | `"https://xiaohuanxiong.com/api/web/auth/v1"` | 鉴权基址（token 刷新、订阅信息）。 |
| `points_base` | `"https://xiaohuanxiong.com/api/web/points/v1"` | 积分基址（余额、账单）。 |
| `origin` | `"https://xiaohuanxiong.com"` | 主站 origin（签到与账单接口挂在这里，独立配置点）。 |
| `connect_timeout_seconds` | `30` | TCP 建连超时（秒）。 |
| `response_header_timeout_seconds` | `120` | 等待响应头超时（秒）。 |
| `request_timeout_seconds` | `0` | 单次 chat 的总超时（秒）；`0` = 不设（长回答流式）。 |
| `refresh_timeout_seconds` | `30` | 刷新 token 超时（秒）。 |
| `catalog_timeout_seconds` | `10` | 拉取模型目录超时（秒）。 |
| `balance_timeout_seconds` | `20` | 查询积分/余额超时（秒）。 |
| `bills_timeout_seconds` | `25` | 账单核对超时（秒，签到用）。 |
| `insecure_skip_verify` | `false` | 是否跳过上游 TLS 证书校验（仅排障用，勿在生产打开）。 |
| `user_agent` | `"xhx2api/0.1.0"` | 发往上游的 `User-Agent`。 |
| `extra_headers` | `{}` | 附加到所有上游请求的头（键值均为字符串）。 |

### `accounts`

| 键 | 默认值 | 含义 |
|---|---|---|
| `file` | `"./data/accounts.json"` | 账号文件路径；不存在则空池启动（`/v1` 返回 503）。 |
| `strategy` | `"round_robin"` | 选号策略：`round_robin`（按 weight 平滑加权、同分用 LRU，默认）或 `priority`（按 priority 升序、last_used_at 升序、weight 降序）。 |
| `refresh_margin_seconds` | `300` | 过期前多久主动刷新 token（秒）。 |
| `cooldown_seconds` | `600` | 429 的最低冷却时长（秒）；实际冷却取它与 `Retry-After` 的较大值。 |
| `max_inflight_per_account` | `4` | 单账号最大在途请求数；达到后该账号暂时不参与选号，`0` 表示不限制。 |
| `server_error_cooldown_seconds` | `30` | 5xx/网络错误首次冷却时长；连续失败按指数翻倍。 |
| `failure_decay_seconds` | `600` | 连续失败计数的衰减窗口（秒）；超过窗口后下次失败重新从 1 次计。 |
| `cooldown_max_seconds` | `3600` | 所有账号冷却的统一封顶（秒），同时约束 429 与 5xx 退避。 |
| `max_attempts` | `3` | 一次客户端请求最多尝试几个账号；401 后同账号强制刷新只重试一次，不额外消耗轮次。 |
| `refresh_on_401` | `true` | 收到 401 时强制刷新同账号并重试一次；刷新失败或再次 401 时换号。 |

### `models`

| 键 | 默认值 | 含义 |
|---|---|---|
| `default` | `"sn-glm-5-3-flash"` | 请求未给 `model` 时使用的模型 id。 |
| `validate` | `"catalog"` | 模型校验：`none`（原样透传）或 `catalog`（不在目录里则 400；目录为空时跳过校验、原样透传）。 |
| `catalog_cache_seconds` | `600` | 目录内存缓存有效期（秒）。 |
| `catalog_cache_file` | `"./data/model_catalog.json"` | 目录持久化缓存文件；远程拉不到时用它。 |

> 模型名一律用**上游 id**（`sn-glm-5-3-flash`、`sn-glm-5-3`、`sn-kimi-k3` 等），没有别名映射。
> 目录来源优先级：内存（TTL 内）→ 持久化缓存文件 → 空（来源显示 `empty`，此时 `/v1/models` 返回空列表）。

### `log`

| 键 | 默认值 | 含义 |
|---|---|---|
| `level` | `"info"` | 日志级别：`debug` / `info` / `warn` / `error`。 |
| `access_log` | `true` | 是否向 stdout 打印 `[access] …` 访问日志。 |
| `buffer_size` | `500` | 面板「最近请求」保留的条数。 |
| `file` | `""` | 额外日志文件路径；空 = 只写 stdout。 |

> 启动摘要（生效配置、版本/监听/账号/目录）**不受 `level` 影响**，始终打印。

### `admin`

| 键 | 默认值 | 含义 |
|---|---|---|
| `enabled` | `true` | 是否启用 `/admin` 与 `/admin/api/*`；`false` 时一律 404。 |
| `api_key` | `""` | 兼容旧管理客户端的 Bearer 密钥；为空不代表关闭鉴权。仅 GET 额外兼容 `?key=`，写请求不允许 query key。 |
| `title` | `"小浣熊反代"` | 面板顶部标题。 |
| `auth_file` | `"./data/auth.json"` | 管理员密码哈希与 API Key 摘要的持久化文件；首次不存在时自动创建。 |
| `session_ttl_seconds` | `43200` | 面板登录会话有效期（秒，必须 ≥ 1）。 |
| `cookie_secure` | `false` | 强制会话 Cookie 使用 `Secure`；请求为 TLS 或 `X-Forwarded-Proto=https` 时也会自动启用。 |

### `checkin`

| 键 | 默认值 | 含义 |
|---|---|---|
| `enabled` | `true` | 是否启用每日签到后台任务。 |
| `time` | `"00:01"` | 每日触发时刻（本地时间 `HH:MM`，容器 `TZ=Asia/Shanghai`）。 |
| `on_start` | `true` | 启动时若「今天还没成功」立即补签一次。 |
| `verify_bills` | `true` | `true` = 以「今日账单确有入账」为成功；`false` = 以 `setting_info` 返回 `code==0` 为成功。 |
| `bills_limit` | `20` | 账单核对查询条数。 |
| `retry_interval_minutes` | `30` | 当天失败后的重试间隔（分钟）。 |
| `max_attempts_per_day` | `5` | 当天最多尝试次数（含首次）。 |
| `history_file` | `"./data/checkin.json"` | 签到历史文件。 |
| `history_days` | `30` | 历史保留天数（按日期去重）。 |
| `concurrency` | `2` | 同时执行签到任务的 worker 数（必须 ≥ 1）。 |
| `jitter_seconds` | `60` | 每个账号启动签到的确定性抖动上限（秒）；`0` = 不抖动。 |

### `login`

| 键 | 默认值 | 含义 |
|---|---|---|
| `enabled` | `true` | 是否启用网页登录；`false` 时两个登录接口返回 403，面板隐藏「网页登录」入口（手动添加仍可用）。 |
| `app_name` | `"办公小浣熊客户端"` | 授权页上报的客户端名（中文，会正确 URL 编码）。 |
| `state_ttl_seconds` | `600` | 登录 state 有效期（秒）；过期后必须重新发起。`0` 表示立即过期（仅测试用）。 |

> 授权页路径 `/code/authorize`、`login_source=desktop` 与回调 `office-raccoon://auth/callback`
> 是**协议常量**，不做配置项。

---

## 认证与安全

### 首次启动与凭据文件

- 服务启动时加载 `admin.auth_file`。文件不存在时创建唯一管理员 `admin`，生成随机密码和
  `xhx_` 开头的 API Key；会话 TTL、认证文件路径和 Cookie 安全选项来自 `admin` 配置。
- 首次创建时，用户名、密码和 API Key 的明文只写入认证文件同目录的
  `initial-credentials.txt`（默认 `data/initial-credentials.txt`），权限固定 `0600`。
  启动日志只输出该文件的绝对路径，不输出密码或 Key。请先把 `/v1` API Key 保存到密码管理器或客户端配置，
  确认登录与调用都正常后再删除该文件；`auth.json` 只保存摘要，删除后无法从其中还原明文 Key。
- 想在首次创建时指定密码，可在启动前设置 `XHX_ADMIN_PASSWORD`；值去空白后少于 12 个字符会启动失败。
- `auth.json` 只保存版本、管理员用户名、PBKDF2-HMAC-SHA256 密码派生参数/盐/摘要，以及 API Key 的
  前缀和 SHA-256 摘要；不保存密码或 API Key 明文。文件权限为 `0600`，写回采用临时文件加原子 rename。
- **重置管理员**：停止服务，删除或移走 `admin.auth_file`，再启动服务。服务会重新生成管理员、密码和
  API Key，并重新写出 `initial-credentials.txt`；旧认证文件中的 API Key 同时失效。

### 管理面板会话与 CSRF

- 面板登录成功后设置 `xhx2api_admin` Cookie：`HttpOnly`、`SameSite=Lax`、`Path=/admin`，有效期与
  `admin.session_ttl_seconds` 一致。会话仅保存在进程内存中，服务重启后需要重新登录。
- 登录响应返回 CSRF token。面板只把它保存在当前页面内存中，不使用 `localStorage`、`sessionStorage`
  或任何表单持久化；会话 Cookie 由浏览器自动携带。
- 会话身份调用 `POST` / `PATCH` / `DELETE` 时必须带 `X-CSRF-Token`，缺失或不匹配返回
  `403 csrf_invalid`。旧 `admin.api_key` Bearer 身份不需要 CSRF。
- 旧式 `?key=<admin.api_key>` 只兼容 GET；任何写请求使用 query key 都不会通过鉴权。
- 登录失败统一返回「用户名或密码错误」，并按客户端 IP 做失败限速；会话与认证接口使用 `Cache-Control: no-store`。
- `admin.enabled=false` 时所有 `/admin*` 均为 404。`/admin` 与 `/admin/` 只提供登录页面，不返回数据；
  未登录的页面脚本只探测 `/admin/api/auth/session`，不会请求状态或账号列表。

### `/v1` API Key

- `/v1/models` 与 `/v1/chat/completions` 始终要求 `Authorization: Bearer <key>`，没有匿名模式。
- 合法 key 是 `config.json` 中非空的 `api_key`，或首次启动时写入认证文件并展示在
  `initial-credentials.txt` 中的 API Key；两类 key 任一匹配即可。
- 校验失败统一返回 `401`、错误码 `invalid_api_key`。API Key 比较使用常量时间比较。

---

## 账号文件与 token 获取

### 文件格式（`data/accounts.json`）

```json
{
  "accounts": [
    {
      "id": "user-example",
      "name": "小浣熊-1",
      "user_id": "example-user",
      "access_token": "eyJhbGci…",
      "refresh_token": "eyJhbGci…",
      "expires_at": 0,
      "enabled": true,
      "priority": 100,
      "weight": 100,
      "last_used_at": 0,
      "consecutive_fails": 0,
      "last_failure_at": 0,
      "last_refresh_at": 0,
      "cooldown_until": 0,
      "last_error": ""
    }
  ]
}
```

| 字段 | 说明 |
|---|---|
| `id` | 账号唯一标识，反代内部使用（建议 `user-<user_id>`）。 |
| `name` | 显示名，面板展示用。 |
| `user_id` | 小浣熊用户 id。 |
| `access_token` | 访问令牌（JWT），聊天请求的 `Authorization: Bearer`。 |
| `refresh_token` | 刷新令牌；用于换取新的 `access_token`。 |
| `expires_at` | `access_token` 过期时间（**毫秒** Unix 时间戳）；`0` = 未知（不主动刷新，靠 401 触发）。 |
| `enabled` | 是否启用；缺省 `true`。 |
| `priority` | `priority` 策略的选号优先级，数值越小越优先。 |
| `weight` | 调度权重，默认 `100`；越大获得越多流量，非法或缺失按 `100` 处理。 |
| `last_used_at` | 最近被选中的时间（毫秒），由程序维护，也是 LRU 排序依据。 |
| `consecutive_fails` | 衰减窗口内的连续失败次数，由程序维护；成功清零。 |
| `last_failure_at` | 最近失败时间（毫秒），由程序维护，用于失败衰减与健康分层。 |
| `last_refresh_at` | 上次刷新时间（毫秒），由程序维护。 |
| `cooldown_until` | 冷却截止时间（毫秒），由程序维护。 |
| `last_error` | 最近一次错误，由程序维护。 |

> 面板账号快照还包含运行时 `inflight`（当前在途请求数），该字段不写入账号文件。
> 选号会先过滤停用、无 token、冷却中、被本次请求排除以及在途已满的账号；健康账号始终优先于仍处于
> 失败衰减窗口的账号。

> 文件里**其它未知字段会被原样保留**，程序回写时不会丢。程序回写用「临时文件 + rename」原子替换，权限 `0600`。

### 怎么拿到 token

1. 浏览器登录 `https://xiaohuanxiong.com`，打开开发者工具（F12）：
   - 在 **Network** 里找任意一个发往 `xiaohuanxiong.com` 的请求，复制请求头里的 `Authorization: Bearer eyJ…`，`eyJ…` 即 `access_token`；
   - `refresh_token` 通常在登录/刷新接口的响应里，或存放于 **Application → Local Storage / Cookies**（形如 `refresh_token`）。
2. 或者使用官方桌面端：登录后 token 保存在 `~/.box-agent/config/auth.json`（`access_token` / `refresh_token`）。

**务必注意：**

- `access_token` 会**过期**（一般数小时到一天）。本服务会在临期前自动用 `refresh_token` 刷新。
- `refresh_token` **可能被轮换**（刷新后上游可能下发新的），本服务会自动回写。
- **同一个账号不要同时被两个反代/客户端刷新**：两边都拿旧 `refresh_token` 去刷新，会互相顶掉，
  导致其中一方频繁 401。若要从别的工具迁移，先停掉那边再用本服务。

### 调度与失败重试

- 选号在锁内完成过滤、分层和 `last_used_at` 更新；持锁期间不做网络或文件 IO。每次实际尝试都占用账号
  在途名额，并在成功、失败、超时、客户端断开的每条路径归还。
- `priority` 模式按 `priority` 升序、`last_used_at` 升序（更久未用优先）、`weight` 降序选择。
  `round_robin` 模式使用平滑加权轮转，权重越高获得越多流量，同权重平局用 LRU。
- 连续失败且仍在 `failure_decay_seconds` 窗口内的账号会降为失败层；只有健康层没有可用账号时才使用失败层。
- **401**：首次收到 401 时强制刷新同一账号并重试一次，这次重试不额外消耗 `max_attempts` 轮次；
  刷新失败或再次 401 才换号。
- **429**：解析 `Retry-After` 的整数秒或 HTTP 日期，账号冷却
  `max(cooldown_seconds, Retry-After)`，再受 `cooldown_max_seconds` 封顶，随后换号。所有尝试均 429 时，
  客户端响应的 `Retry-After` 取已尝试账号中最小的剩余冷却秒数。
- **5xx/网络错误**：首次冷却 `server_error_cooldown_seconds`，连续失败按指数翻倍并受
  `cooldown_max_seconds` 封顶，随后换号；连续失败计数超过 `failure_decay_seconds` 后自然重置。
- **其它 4xx**：不冷却、不换号，保持上游状态码并透传错误信息。

---

## HTTP 端点

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/healthz` | 无 | 健康检查：`{"status":"ok","version":…,"uptime_seconds":…,"accounts":{…},"models":…}`。 |
| GET | `/favicon.ico` | 无 | 返回 204（在鉴权前短路）。 |
| GET | `/v1/models` | `/v1` API Key | OpenAI 模型列表（仅上游目录条目；目录为空时 `data` 为空数组）。 |
| POST | `/v1/chat/completions` | `/v1` API Key | 聊天补全，支持流式/非流式、思维链、function calling、图片输入（见[图片输入（多模态）](#图片输入多模态)）。 |
| GET | `/admin`、`/admin/` | 无（`admin.enabled=false` 时 404） | 内嵌管理面板登录页 HTML，不返回业务数据。 |
| POST | `/admin/api/auth/login` | 公开（`admin.enabled=false` 时 404） | 管理员用户名/密码登录，按客户端 IP 失败限速。 |
| GET | `/admin/api/auth/session` | 公开探测，未登录返回 401 | 只返回当前凭据是否已认证；会话模式返回 CSRF 与过期时间。 |
| POST | `/admin/api/auth/logout` | 有效会话 + CSRF | 注销会话并清理 Cookie。 |
| GET | `/admin/api/*` | 会话 Cookie 或旧 `admin.api_key` | 只读 JSON 接口（见下）；query key 仅可用于 GET。 |
| POST/PATCH/DELETE | `/admin/api/*` | 会话 Cookie（须 CSRF）或旧 Bearer key | 账号管理、批量导入与网页登录写接口。 |

`/admin/api/*` 的只读 GET：

| 路径 | 返回 |
|---|---|
| `/admin/api/status` | 版本、启动时间、运行时长、监听地址、生效配置（密钥已掩码）、账号/模型统计。 |
| `/admin/api/accounts` | 账号池快照（不含完整 token，只有尾 4 位）。 |
| `/admin/api/accounts/balance` | 每账号积分与钱包明细（逐账号查上游，结果缓存 30 秒）。 |
| `/admin/api/models` | 模型目录条目。 |
| `/admin/api/requests?limit=N` | 最近 N 条请求日志（默认 100）。 |
| `/admin/api/checkin` | 每账号的每日签到状态与历史。 |

`/admin/api/login/start`、`/admin/api/login/exchange` 与其它 `/admin/api/*` 一样使用管理鉴权（会话或旧
`admin.api_key` Bearer），不是公开接口。`admin.enabled=false` 时全部 `/admin*` 路径返回 404。

---

## 图片输入（多模态）

`/v1/chat/completions` 的 `messages[].content` 支持 OpenAI 的多模态数组写法，图片会随请求一起转发给上游模型：

```bash
IMG=$(base64 -w0 photo.jpg)   # macOS 用：IMG=$(base64 -i photo.jpg)
curl -s -H 'Content-Type: application/json' -H "Authorization: Bearer <api_key>" \
  -d "{\"model\":\"sn-glm-5-3-flash\",\"messages\":[{\"role\":\"user\",\"content\":[
      {\"type\":\"text\",\"text\":\"这张图里是什么？\"},
      {\"type\":\"image_url\",\"image_url\":{\"url\":\"data:image/jpeg;base64,$IMG\"}}]}]}" \
  http://127.0.0.1:3066/v1/chat/completions
```

- 流式 / 非流式都支持；`usage.prompt_tokens` 里包含图片占用的 token（实测 2MB JPEG 约 8000）。
- **优先用 `data:` URL**：传远程 `http(s)` 地址时是**上游服务器去下载**那个地址，墙外地址会一直等到超时
  （`upstream.response_header_timeout_seconds`）。
- 图片作为请求体的一部分转发，大小上限同 `maxChatBodyBytes`（64MB）。
- 实测可识别图片的模型：`sn-glm-5-3-flash`、`sn-sensenova-6-8-flash`、`sn-deepseek-v4-1-flash`、
  `sn-kimi-k3`（较慢，单张图可能一两分钟）。
- 这是上游 chat 接口自带的能力；**网页端的「上传文件」入口**（先把文件传到小浣熊侧再引用）未做逆向，
  本服务不需要它。

---

## 管理面板

浏览器打开 `http://<监听地址>/admin`，用 `initial-credentials.txt` 中的 `admin` 用户名和密码登录。
兼容旧客户端时也可以使用 `/admin?key=<admin_api_key>`，但新部署推荐会话登录。

- 页面启动先请求 `GET /admin/api/auth/session`；登录前不会请求状态、账号列表或其它受保护接口。
  登录成功后才开始加载数据。
- 所有请求均使用 `credentials: "same-origin"`；非 GET 自动附带当前页面内存中的 `X-CSRF-Token`，
  不写入 `localStorage` / `sessionStorage`，也不在页面显示 token、CSRF 或 Cookie。
- 任一受保护请求返回 401/403 时，页面停止轮询并切回登录区；点「退出登录」会注销服务端会话并清除 Cookie。
- 展示状态，并提供**账号管理**（网页登录 / 手动新增 / 批量导入 / 启用禁用 / 删除 / 测试）。
- 登录后**每 5 秒自动刷新**一次；某次刷新失败时顶部显示「接口不可用」，但**不会清空**已展示的数据。
- 自上而下包含六块：顶部状态条、账号池、积分卡、每日签到、模型目录、最近请求。
- 页面为**单文件、零外部依赖**（无 CDN / 外部字体 / 图标），离线可用；暗色主题，窄屏可用（表格可横向滚动）。
- 页面**不会**显示完整 `access_token` / `refresh_token` / `api_key`（接口本身也不返回，只显示尾 4 位）。

### 面板管理账号（写操作）

管理接口由统一中间件鉴权：有效管理员会话 Cookie，或旧 `admin.api_key` 的
`Authorization: Bearer`。会话写请求必须额外带 `X-CSRF-Token`；旧 Bearer key 调用不需要 CSRF。
`?key=` 只允许 GET，管理写请求不能使用 query key。`admin.api_key` 为空不会把管理接口变为匿名开放。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/admin/api/accounts` | 新增账号，body：`name` / `access_token`（必填）/ `refresh_token` / `priority`（默认 100）/ `enabled`（默认 true）。成功 201。 |
| PATCH | `/admin/api/accounts/{id}` | 只改给出的字段：`name` / `priority` / `enabled` / `access_token` / `refresh_token`。成功 200。 |
| DELETE | `/admin/api/accounts/{id}` | 删除账号。成功 200 `{"deleted":"<id>"}`。 |
| POST | `/admin/api/accounts/{id}/test` | 用该账号当前 token 只读打一次上游目录，返回 `{"ok":true,"status":200,"models":<n>}`；上游失败也返回 HTTP 200，用 `{"ok":false,"status":<上游码>,"error":"…"}` 表达。 |

- 鉴权沿用 `/admin/api/*`：会话 Cookie（写请求带 `X-CSRF-Token`），或旧 `Authorization: Bearer <admin.api_key>`；
  旧 `?key=` 仅支持 GET。
- 新增时 `id` = `user-<userId>`（从 JWT 解析，不验签）；取不到 userId 时用 `acct-<8 位随机十六进制>`。
- `expires_at` / `user_id` 从 JWT 解析；解析不出则 `expires_at = 0`（不主动刷新，靠 401 触发）、`user_id = ""`。
- 每次写操作成功后**原子回写** `accounts.json`（临时文件 + rename、权限 `0600`、保留未知字段）。
- 面板「账号池」表格的「操作」列即可启用/禁用、删除（二次确认）、测试；表格上方「新增账号」按钮展开表单。

---

## 批量导入账号

### 面板导入

账号池工具栏的「导入账号」可一次选择多个 `.json` / `.zip` 文件，并提供「覆盖同 ID 账号」和
「仅预演（不写盘）」选项。浏览器使用 `FormData`，每个文件都以 `files` 字段提交，沿用管理员会话 Cookie
和内存 CSRF；提交成功后刷新账号表与状态，结果区展示 `total` / `added` / `updated` / `skipped` /
`invalid` / `errors`。选择预演时会明确显示「预演，未写盘」。

### API

```http
POST /admin/api/accounts/import
Content-Type: multipart/form-data
Authorization: Bearer <admin.api_key>        # 或使用管理员会话 Cookie
X-CSRF-Token: <session csrf token>           # 仅会话写请求需要
```

- multipart 字段：`files` 可重复；JSON 文件可以是标准 `{"accounts":[...]}`，也支持包含
  `cookies_raw` / `localStorage` / `jwt` / `phone` 等字段的浏览器导出 JSON。ZIP 按文件魔数识别，
  不依赖扩展名，并可包含多层目录中的 JSON。
- `overwrite=true|false`（缺省 false）决定已有 ID 是否覆盖；`dry_run=true|false`（缺省 false）只分类统计、
  不写账号文件。单条坏记录不会中断其它记录。
- 限制：请求总量 **32 MiB**；一次最多 **200 个上传文件**；单个 ZIP 最多 **200 个条目（含目录）**；
  ZIP 单条目解压后最多 **8 MiB**；ZIP 展开总量最多 **32 MiB**。超限会被拒绝或计入错误。
- 响应为 JSON：`dry_run`、`overwrite`、`total`、`added`、`updated`、`skipped`、`invalid`、
  `errors`、`errors_truncated`。`errors` 最多返回 100 条；错误文本不回显 token/API Key。

**覆盖与重复 ID 语义：**

- `overwrite=false`：导入 ID 已存在时计入 `skipped`，保留原账号；同一批次重复 ID 保留首条，后续计入
  `skipped`。
- `overwrite=true`：导入 ID 已存在时计入 `updated`，更新凭证、名称、用户 ID 和附加字段，同时保留
  `enabled`、`priority`、`weight` 等人工调度设置，并清除旧凭证留下的冷却与连续失败状态。同一批次重复
  ID 以末条为准，被替换的条目计入 `skipped`。
- 缺少 `id` 或 `access_token` 的记录计入 `invalid`；`dry_run` 完全走同一分类流程，但内存账号池与
  `accounts.json` 均不改变。

---

## 网页登录（一次性授权码换凭证）

面板「账号池」上方的**「网页登录」**按钮走小浣熊官方登录页，登录成功后自动拿到
`access_token` / `refresh_token`，**不需要手动粘贴 token**。

流程（`login.enabled=true` 时）：

1. 面板点「网页登录」→ `POST /admin/api/login/start`，服务用 `crypto/rand` 生成一次性
   `state`（32 位十六进制，带过期时间）并返回授权页 URL：
   `https://xiaohuanxiong.com/code/authorize?login_source=desktop&appname=…&state=…`。
2. 打开该链接，在官方页面用微信/验证码/密码登录；成功后官方页面跳转自定义协议
   `office-raccoon://auth/callback?code=…&state=…`（浏览器打不开这个协议是正常的）。
3. 把地址栏里的**整条 URL** 复制回面板输入框，点「完成登录」→
   `POST /admin/api/login/exchange`，服务校验 state 后用 code 调
   `POST {auth_base}/login_with_authorization_code` 换凭证并落账号。

接口：

| 方法 | 路径 | body | 成功返回 |
|---|---|---|---|
| POST | `/admin/api/login/start` | 无 | `{"url":…,"state":…,"expires_at":…,"ttl_seconds":…}` |
| POST | `/admin/api/login/exchange` | `{"callback_url":"office-raccoon://auth/callback?code=…&state=…"}` 或 `{"code":…,"state":…}` | `{"id":"user-…","updated":false,"account":{…}}`（同 id 再登录一次为 `updated:true`） |

- 鉴权沿用 `/admin/api/*`：管理员会话 Cookie（写请求带 `X-CSRF-Token`）或旧
  `Authorization: Bearer <admin.api_key>`；旧 `?key=` 仅支持 GET。
- **state 安全语义**：state 由本服务生成、内存 pending 表登记（带过期时间）；回调里的 state 必须与其中
  一条逐字相等且未过期，且**一次性消费**。校验失败/过期/缺失一律 400，且**不调用上游**。
  回调 URL 解析后逐项比对 `scheme=office-raccoon`、`host=auth`（大小写不敏感）、`path=/callback`，
  不使用前缀匹配。`code` ≤ 8192、`state` ≤ 512 字符。
- `login.enabled=false` 时两个接口返回 403「网页登录已禁用（config.json 的 login.enabled=false）」，面板隐藏入口。
- 换回的 `office_identity` / `office_org_name` / `office_org_role` 非空时一并写入账号记录（作为未知字段保留）。
- 日志只记 `[login]` 的 id/结果，**不打印 code 或 token**。

---

## 挂进 CPA（CLIProxyAPI）

本服务本身就是 OpenAI 兼容端点，在 CPA 里按 `openai-compatibility` 渠道挂即可，只需要三样东西：
**base-url**、**api-key**（= 本服务 `/v1` API Key）、**模型名**（就是上游 id，见 `/v1/models`；
本服务**不做别名**）。

```yaml
# 追加到 /zfspool/data/ai/cpamp/cliproxyapi/config.yaml 顶层的 openai-compatibility: 列表里
  - name: xhx2api
    prefix: xhx
    base-url: http://10.1.0.8:3066/v1
    api-key-entries:
      - api-key: <本服务 /v1 API Key>
    models:
      - name: sn-glm-5-3-flash
        alias: ""
      - name: sn-glm-5-3
        alias: ""
      - name: sn-kimi-k3
        alias: ""
      - name: sn-deepseek-v4-1-flash
        alias: ""
      - name: sn-sensenova-6-8-flash
        alias: ""
      - name: sn-sensenova-6-8-flash-lite
        alias: ""
      # 可选（visible=false 的内部模型）：raccoon-8c4485 / raccoon-19b265 / raccoon-405a1c
```

- API Key 可用 `config.json` 中非空的 `api_key`，或首次启动 `initial-credentials.txt` 里的 `xhx_` Key。
  `auth.json` 只保存摘要，不能从其中还原明文 Key。
- 生效：`ssh ai 'cd /zfspool/data/ai/cpamp && docker compose restart cli-proxy-api'`
- 挂上后模型对外名带前缀（`xhx/sn-glm-5-3-flash`）。验证：
  `curl -s -H "Authorization: Bearer <CPA key>" http://10.1.0.8:8317/v1/models | grep xhx`
- 图片输入经 CPA 同样可用（实测 `xhx/sn-glm-5-3-flash` 流式/非流式都正常，`usage` 里的图片 token 也保留）。

---

## 每日签到

小浣熊每天会给账号发放一次「每日积分」。本服务内置签到任务，替账号领取这笔积分：

- 触发时机：进程启动时若「今天还没成功」立即补签（`checkin.on_start`）→ 每天 `checkin.time`（默认 `00:01`，本地时区）→ 当天失败后按 `retry_interval_minutes` 重试。
- 每个账号**每天最多成功一次**（靠 `data/checkin.json` 幂等判定，不会重复打上游）。
- `verify_bills=true` 时，以「当日账单确有正数入账」为成功判据；否则以触发接口 `code==0` 为成功。
- 结果会写入日志（`[Checkin] 账号 <id> 今日积分 +300` / 失败 warn），并可在面板「每日签到」表或 `GET /admin/api/checkin` 查看。
- 后台等待按最长 30 秒的墙钟分段执行；每段结束都重新读取墙钟并计算下一事件，系统休眠唤醒后不会继续傻等
  原计时器。每日触发与失败重试取更早者，跨天会重新计算。
- 同一时刻只允许一个签到批次运行（单飞）；正在跑时下一次触发直接跳过，避免重启/定时器重叠造成重复批次。
- `checkin.concurrency` 控制 worker 数（默认 2，至少 1）。账号按确定性的 `FNV-1a(accountID|date)` 抖动
  错峰启动，抖动范围由 `checkin.jitter_seconds` 限制；同账号同一天重启后延迟仍一致，`0` 表示关闭抖动。
- 设 `checkin.enabled=false` 可关闭后台任务（历史仍可读）。

---

## 测试

在仓库的 `src/` 目录运行：

```bash
cd src
gofmt -l .               # 应无输出
go test -count=1 ./...
go vet ./...
```

测试只使用 Go 标准库，不需要真实小浣熊账号、token 或网络访问。

---

## 限制与排障

### 已知限制

- 支持**文本**聊天、function calling 与**图片输入**（OpenAI 多模态 `content` 数组，见[图片输入（多模态）](#图片输入多模态)）；
  上游网页端的**「上传文件」入口**（先把文件传到小浣熊侧、再在对话里引用）未做逆向，本项目不需要它。
- **不支持**注册机、新手任务等；网页登录已支持（见「网页登录」），也可手动粘贴 token。
- 上游模型行为、额度、风控策略随时可能变化，本项目不保证长期可用。
- 单账号被多处同时刷新会互相顶掉（见上文 token 说明）。

### 常见问题

**401 反复出现**
- 说明 `access_token` 失效且自动刷新没成功。先看面板「账号池」的**最近错误**与 `last_error`：
  - 若刷新失败：`refresh_token` 可能已过期或被别处轮换。重新登录抓一份新的 token 写入 `data/accounts.json`。
  - 确认**没有第二个程序**在用同一账号刷新。
- 日志里会看到 `[chat] 账号 … 刷新失败：…`。

**429（限流）**
- 上游对该账号限流，服务按 `max(accounts.cooldown_seconds, Retry-After)` 冷却（`Retry-After` 支持整数秒或
  HTTP 日期），并以 `accounts.cooldown_max_seconds` 封顶，随后自动换号。
- 面板「冷却中」列可看到状态。若频繁 429，降低请求频率或增加账号（`accounts.max_attempts` 决定一次请求最多换几个号）。

**5xx / 网络错误**
- 该账号按连续失败次数指数退避，首次 `accounts.server_error_cooldown_seconds`，封顶
  `accounts.cooldown_max_seconds`，随后换号；连续失败计数超过 `failure_decay_seconds` 后自然衰减。

**无法登录面板 / 重置管理员**
- 首次凭据在 `admin.auth_file` 同目录的 `initial-credentials.txt`，读取后删除。若文件已丢失，且没有可用的
  `admin.api_key` Bearer，可停止服务后删除或移走 `auth.json` 再重启，生成新的管理员密码和 API Key。
- 服务重启会清空内存会话，需要重新登录；HTTPS 反向代理应传递 `X-Forwarded-Proto: https`，或显式设置
  `admin.cookie_secure=true`。

**批量导入失败**
- 检查是否超过 32 MiB 请求、200 个文件、单 ZIP 200 条目/单条目 8 MiB/展开 32 MiB 限制。
- `dry_run` 只统计不写盘；确认未误选「仅预演」。相同 ID 在 `overwrite=false` 时跳过，在
  `overwrite=true` 时更新并保留 `enabled` / `priority` / `weight`。

**目录拉不到**
- 不影响服务转发：会依次退回**持久化缓存文件** → **空**（来源显示 `empty`）。目录为空时 `/v1/models` 返回空列表；`validate=catalog` 会跳过校验、原样透传上游。
- 排查：检查 `upstream.llm_base` 连通性与网络/代理；启动日志若有 `[catalog] 远程目录拉取失败…` 即为此问题。
- 面板顶部「模型数（来源）」会显示当前来源是 `remote` / `cache` / `empty`。

**启动失败：未知键 `aliases` / `fallback`**
- `models.aliases` 与 `models.fallback` 已在第四轮移除。配置里若仍留着这两个键，会因未知键报错并以退出码 2 启动失败。
- 处理：从 `config.json` 删除这两个键；模型名直接使用上游 id（如 `sn-glm-5-3-flash`）。

**账号文件写回失败**
- 日志出现 `[pool] 警告：…账号文件…` 即写回失败。常见原因：`data/` 目录不可写、挂载为只读、磁盘满。
- 写回失败**不影响本次请求**，但 token 刷新结果不会持久化，重启后可能仍是旧 token。
- 检查容器内 `data/` 挂载与权限（程序以非 root 用户运行，需可写）。

**签到失败**
- 看面板「每日签到」表的**最近消息 / 最近错误**，或 `GET /admin/api/checkin` 的 `last_error`。
- 日志里的 `[Checkin] 账号 … 签到失败：…` 会给出原因（token 刷新失败 / 触发发放失败 / 今日未见积分入账 / 账单核对失败）。
- 常见原因：token 失效、`checkin.time` 时区不对、当天已达 `max_attempts_per_day` 上限。

---

## 免责声明

本项目为个人学习与自用工具，与商汤科技无任何关联，也未获其授权。请自行确保使用方式符合上游服务条款
与当地法律法规；请勿用于商业牟利、批量注册或任何滥用行为。使用风险由使用者自行承担。
