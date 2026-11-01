# AGENTS.md

给在本仓库工作的 AI 协作者（也适用于人类）的说明书。**代码行为优先于文档**：一旦发现文档与代码不一致，按代码修文档，并顺手补测试。

---

## 1. 这是什么

`workbuddy2api` 是一个单进程 Go 网关：把 WorkBuddy（CN / global 双域）的**账号池**包装成 OpenAI 兼容 API，附带内嵌的 Web 管理面板。

- 无数据库：状态落 `data/state.json`，可选同步到 Upstash Redis（不可用时自动降级为 Noop）。
- 无前端构建链：面板是内嵌的 HTML/CSS/JS 字符串。
- 依赖极简：`go-redis` + `xxhash` + `go.uber.org/atomic`（后两个是 redis 的间接依赖）。
- 上游作者已停止维护（见 README 顶部声明），本仓库是 fork，只在必要处加自用改动。

## 2. 目录地图

| 路径 | 职责 | 量级 |
|---|---|---|
| `cmd/server` | 主程序：配置加载 → 装配 pool/upstream/handler/panel/scheduler → 优雅退出 | 中 |
| `cmd/login` | OAuth 设备码登录，落盘 `auths/workbuddy-<uid>.json`（`--realm=cn\|global`） | 小 |
| `cmd/signin` / `cmd/credit` / `cmd/trial` | 一次性工具：批量签到 / 积分日报 / global trial 领取 | 小 |
| `internal/server` | HTTP 层：路由、Bearer 鉴权、chat 轮转、**错误映射唯一出口**、CORS、请求记账 | ~6.9k |
| `internal/pool` | 账号池：选号、在途租约、冷却/熔断/降权、积分保底、模型级负缓存、粘性、诊断、持久化 | ~8.5k |
| `internal/upstream` | 上游客户端：传输层（SSE/DNS/超时/重试）+ 业务 API（签到、成长、旅行、活动、夜猫子、trial、billing） | ~17.5k |
| `internal/panel` | 管理面板（内嵌前端）+ `/panel/api/*`（账号增删停、配置读写、任务、用量） | ~8.3k |
| `internal/auth` | auth 文件解析（嵌套形 / 扁平形双形态）+ realm 判定 | ~1.1k |
| `internal/scheduler` | 定时任务：签到 / 成长 / 旅行 / 活动 / 保活 / 夜猫子 / 补偿 | ~4.1k |
| `internal/session` | 会话头族 ID 解析 + 粘性路由 | ~1.3k |
| `internal/reqlog` | 请求级指标 + 脱敏 JSONL 归档 | ~0.9k |
| `internal/usage` / `internal/credithist` | token 用量聚合 / 积分变动留痕 | ~2.0k |
| `internal/livecfg` | 运行期可变配置的并发安全持有者（面板改配置热生效） | 小 |
| `internal/redisstore` | Upstash 封装 + Noop 内存降级 | 小 |
| `internal/httpauth` / `internal/logfmt` / `internal/prompt` | Bearer 原语 / 日志格式（uid 截断）/ 系统提示词 | 小 |
| `scripts/*.py` | 判据探测脚本（模型真实输出上限、任务协议逆向） | — |

## 3. 常用命令

```bash
# 构建 + 部署（本机 macOS：构建后同步到 ~/workbuddy2api/wb2api 并重启 launchd 服务）
bash build.sh

# 前台跑（Ctrl-C 停；后台 nohup bash start.sh > service.log 2>&1 &）
bash start.sh

# 全量测试 / 单包 / 单用例
go test ./...
go test ./internal/pool/ ./internal/server/
go test ./internal/server/ -run TestNoPick -v

# 静态检查
go build ./... && go vet ./...
```

- **不要全量 `gofmt`**：仓库里大量历史文件本来就没格式化，全量格式化会制造数千行噪声 diff。只格式化你**改过**的文件。
- 工作区可能用便携 Go（`../.tools/go`），此时需 `GOPATH/GOMODCACHE/GOCACHE` 指到 `../.tools/*`、`GOPROXY=https://goproxy.cn,direct`、`GOSUMDB=off`、`GOTOOLCHAIN=local`（`build.sh` 已封装）。
- 端口看 `config.json` 的 `listen`；`/healthz` 探活、`/status` 看池子、`/panel/` 面板。

## 4. 一次 chat 请求的链路

1. `ServeHTTP`：CORS 预检（`cors.enabled` 时，早于鉴权，204 直返）→ reqlog trace → `mux`
2. `withAuth`：Bearer 校验（`api_key` 空 = 不鉴权）
3. `chatCompletions`：解析请求 → 解析模型（`cn:` / `global:` 前缀 → realm；裸名默认 cn，开 `pool.cross_domain_fallback` 才跨域回退）→ 粘性会话解析
4. 轮转（最多 `MaxRotate`=3 次）：`PickExcludingForRealm` → `Acquire`（在途租约，满了换号）→ 转发上游（流式 SSE 透传）→ 分类结果 → `Release` + 记账（credits / usage / reqlog）
5. 选不到号：`pool.DiagnoseNoPick` 取成因 → **末端错误映射**（唯一出口）→ OpenAI 错误体 + 可选 `Retry-After`

## 5. 代码约定

- **注释写「为什么」**，中文；引用 issue 号（`#100`、`#102`、`#135`）与实测现象。仓库的注释密度是刻意维持的资产，别删。
- **错误映射只在一个地方**：`chatCompletions` 末端。新增错误成因要同时改：pool 诊断 → handler 映射 → README「选号失败的错误码」表 → 两个测试（pool 单测 + HTTP 契约）。
- **有上游原文时原文优先**：本地只在"没有上游错误体可透传"时才抢话（本地调度类失败）。
- **状态变更走 pool 的公开方法**（`Acquire` / `Release` / `Cooldown` / `BlockModelBackoff` / `NoteModelCost` …），不要在 handler 里改池内字段；落盘交给 `persist.go`。
- **锁内不做 IO**：pool 用 `RWMutex`，诊断/统计走 `RLock`；不要在持锁路径里发请求或写文件。
- **时长配置统一用字符串**（`"600s"`、`"2h"`）解析成 `time.Duration`；状态里的时刻用 `time.Time`（`state.json` 里是 RFC3339）。
- 依赖克制：新增第三方库需要说明理由，并同步 `go.sum` 与 Dockerfile 构建。

## 6. 测试约定

- 93 个 `_test.go`，按「**一个可观测契约一个文件**」组织（如 `pool/diagnose_test.go`、`server/handler_nopick_test.go`）。
- 每个测试顶部用中文注释写明"钉住什么、为什么必须钉住"。
- **对照断言优先**：只断言 `503` 会被"假原因"满足（假上游同样 503）。要证明拦截发生在预期层，必须设对照组（见 `server/handler_credit_floor_test.go`）。
- HTTP 层：`httptest.NewRecorder()` + `NewHandler(Config{...})`，池用 `testPoolWith(...)`；pool 单测直接 `New("")` + `Add` / `SetCredits` / `Acquire`。
- 时间相关：构造状态或用极短时长，别 `sleep` 真实分钟级 TTL。
- 改错误码 / 文案 = 改断言 + 改 README，否则测试绿了但文档骗人。

## 7. 安全红线（最高优先级）

- **绝不提交**：`auths/`、`data/`、`config.json`、`backups/`、任何 `*.orig-bak`、`config.json.bak`（含 `api_key`）、access token / cookie / device token。
- `.gitignore` 已挡住大部分，但本地存在**未被覆盖的 `*-bak` 目录**——`git add` 前后都要 `git status` 逐项看，必要时用 `git diff --cached` 再扫一遍 `api_key|secret|token|password|eyJ`。
- 日志与 reqlog 不记录提示词、响应正文、`Authorization`；uid 走 `logfmt` 截断。
- 面板接口不回显完整 token；`/panel/api/config` 写盘前要校验（它直接改运行期配置）。
- 上游声明禁止反代：本项目仅供自用/学习，不要做成公共代理或对外服务。

## 8. 常见改动怎么落

| 任务 | 落点 |
|---|---|
| 加配置项 | `cmd/server/config.go`（结构+默认值）→ `config.example.json` → README 字段速查 → 需热生效再加 `livecfg` + `/panel/api/config` |
| 加错误码 | `pool/diagnose.go` 给成因 → `server/handler.go` 末端映射 → README 表 → pool 单测 + HTTP 契约测试 |
| 加选号策略 | `pool/strategy.go` / `pick.go`；注意过滤链顺序与「全冷却兜底」语义，补 `strategy_test.go` / `pool_test.go` |
| 加面板接口 | `internal/panel`（路由 + 内嵌前端 `app.js` + 测试；`frontend_test.go` 会校验内嵌资源） |
| 加定时任务 | `internal/scheduler/*.go` + `config.schedule.*_hours` + 面板开关 + README |
| 加上游业务 API | `internal/upstream/`（按域/端点分文件），错误分类要过 `Classify` |

## 9. 上游变异应急手册

上游仓库已删库（见 README「与上游的差异」）：**没有 changelog、没有公告、没有兼容承诺、没有公开文档**。它一旦变动，唯一的表现就是"网关突然大面积失败"。下面是排查顺序，不是背景介绍。

### 9.1 先分清是谁的锅（必做，别跳）

同一个请求走两条路，对比结果：

```bash
# A. 直连上游（用 auths/ 里任一账号的 token，绕过网关）
curl -sS -D- https://copilot.tencent.com/v2/chat/completions \
  -H "Authorization: Bearer <token>" \
  -H "User-Agent: WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1" \
  -H 'Content-Type: application/json' \
  -d '{"model":"deepseek-v4-pro","stream":true,"messages":[{"role":"user","content":"ping"}]}'

# B. 经网关（同一请求体）
curl -sS -D- http://127.0.0.1:<listen>/v1/chat/completions \
  -H "Authorization: Bearer <api_key>" -H 'Content-Type: application/json' -d '<同一 body>'
```

| A 直连 | B 网关 | 结论 | 去哪 |
|---|---|---|---|
| 错 | 错 | **上游变了** | `internal/upstream` |
| 对 | 错 | **我们的改写坏了** | `payload.go` / `headers.go` / `sanitize.go` |
| 对 | 对（但客户端报错） | **出站形态问题** | `sse.go` / `dsml.go` |

取证三件套：`data/request-logs/requests-YYYY-MM-DD.jsonl`（看 `status`/`code`/`account`/耗时）、服务日志里的 `WARN`/`ERROR`、面板 `/panel/` 的请求指标。**`account` 为 `-` 表示压根没选出号——那是池子/调度问题，别去上游找原因。**

### 9.2 症状 → 落点

| 症状 | 首查（按序） | 典型根因 | 不要做什么 | 怎么验证 |
|---|---|---|---|---|
| 大面积 `400` + `code 11101` | `payload.go`：`normalizeToolChoice` → `normalizeImageURL` → `normalizeRoles` → `translateMaxCompletionTokens` | 上游收紧了字段类型（对象/字符串、别名不再被接受） | 别冷却账号、别降权（是请求级错误） | 用 9.1 的 A 路复现最小 body，逐字段二分 |
| `403` + `11140 request illegal`（业务信封内） | `headers.go`：UA 平台段（global 必须 `WorkBuddy AI`）→ `injectAccountStableHeaders` → `X-IDE-*`/`X-Agent-Purpose` | 客户端指纹变了，或新增了校验项 | **别禁用账号**；`isChannelRefused`(11-128) 与 `IsAccountRestricted`(11140) 必须按业务码分流 | 逐项替换 UA 三段式 / 头族复测 |
| `403` 非业务信封（APISIX WAF 页、空体） | `dns.go` → `ExtractClientIP` → `transport.go` | 出口 IP 被 WAF 拦 | 别按账号故障处理（`ErrWafBlock` 只软冷却+抖动退避） | 换出口 IP 复测 |
| `401` + `12153`，全池同错 | `client.go` 的 `sessionDeadMarkers` | token 体系变了（refresh 失效） | 别只按 revive 处理，先确认能不能重新登录 | `go run ./cmd/login --realm=cn` 手跑 |
| 工具一次都不执行；模型把工具标记当文本吐出来 | `dsml.go` 的标记正则（全角竖线 U+FF5C 形态）→ `payload.go` 的 tool 配对三步 | 上游换了标记语法，或 `tools` 又在中转链路丢了 | **别放宽 dsml 的三条硬前提**（会吞正文）；别改 `tool_pairing` 的"一律执行" | 日志 `saw M native tool call block(s) but repaired none`；抓原始 SSE 比对 |
| 输出莫名截断、写到一半停 | `context_catalog.go` → `scripts/probe_max_tokens.py` | 上游真实上限变了（`/v3/config` 是**声称值、普遍虚高**） | 别信 `/v3/config` 的数字，别调 `idle_timeout` | 重跑 probe，看 `finish=length` 判据 |
| 客户端把上下文提前截断 | `model_catalog.go` 四级链 → `model.json` | `context_length` 变小了 | 别动"未知 → 1M"的兜底（高估安全、低估丢上下文） | 对比 `/console/enterprises/personal/models` |
| 费用暴涨 | `cache_key.go` 是否仍在注入 → `prompt` 模式是否每次换 system | 前缀缓存失效（system 抖动 / cache key 变了） | 别先去查选号与倍率 | 看响应里的 `prompt_cache_hit_tokens`（带 key 实测≈0.02，不带≈0.34） |
| 整池健康号在数秒内被一起冷却 | `client.go: Classify` 的判定次序 | 上游新增了"渠道满载/限流"业务码，被兜底成 `ErrSoftRate` 逐号冷却（6021 实案：2 秒清空整池） | 别只加号、别调大 `max_in_flight`（掩盖问题） | 看 `state.json` 里各号 `cool_kind`/`until` 是否同一时刻 |
| 流式长时间没响应、或空流 | `idle.go` → `sse.go` | 上游心跳间隔/帧格式变了，或不再发 `[DONE]` | **别给聊天流加总时长上限**（`Timeout=0` 是故意的） | 抓原始 SSE 看心跳与末帧 |
| 签到/成长/旅行/桌面任务全失败 | `tasks.go` / `report.go` / `desktop.go` / `travel.go` | 业务端点路径或事件字段变了（非公开接口，路径以代码常量为准） | 别改调度时间掩盖失败 | 面板对应按钮手点 + 抓包比对 |
| 请求被内容审核拦（`blocked by security policy` 等） | `sanitize.go` 的黑名单特征串 → `prompt` 降级链路 | 上游扩充了黑名单指纹 | 别按账号故障处理（`ErrContentBlocked` 不罚号） | 9.1 的 A 路复现，逐句二分定位触发串 |

### 9.3 三类"静默变异"（最危险）

上游不报错、只改行为，只能靠断言与日志发现：

- **静默钳制**：`max_tokens` 给大了不报错，到真实上限就截断（实测 16 个 CN 模型 8 个被钳制，最狠声称 1M 实际 32K）。
- **静默丢字段**：`tools` 在中转链路丢失是常态 → 工具调用退化成纯文本。
- **静默降级**：未知字段被直接忽略（`max_completion_tokens` 别名就是这样被吞掉的）。

对策固定为三件：**出站前补位**（`payload.go` / `dsml.go`）、**出站后校验**（`sse.go` / `truncation.go`）、**把每次实测结论写进注释**（带日期与数据，注释就是上游文档）。

### 9.4 改完必须做的五件事

1. **新业务码 → 具体 `ErrKind`**：想清楚"罚不罚号、罚多久"。`Kind` 决定前者，`RetryAfter` 决定后者，两者不要混在一个字段里。
2. **动了判定次序 → 注释写清"不先判会发生什么"**。现有注释全是这么来的（"2 秒内把整池健康号全部误杀冷却"），别只写"新增 111xx"。
3. **补测试**：`client_test.go` 加用例；HTTP 契约变了要同步 `server/handler_*_test.go`，且用对照断言（只断言 503 会被假原因满足）。
4. **同步文档**：README 的字段速查/错误码表 + 本文件 9.2 增一行。
5. **优先加开关/降级，再改默认**：改默认前先想"上游变回去了怎么办"。可逆性比优雅重要。

## 10. 已知坑（踩过的，别再踩）

- **`no_healthy_account` 的历史包袱**：它曾同时表示「在途满 / 积分保底 / 冷却 / 池空」，把网关背压读成了"号全挂了"。现已按成因拆码（`pool_saturated` / `pool_contended` / `all_accounts_cooling` / `no_accounts` / `credit_floor_reached` / `rotation_exhausted`）。**`POST /v1/search/messages` 仍回固定 503 `no_healthy_account`**（`server/search.go`，未统一）。
- **`pool.max_in_flight` 默认 3（global 域 2）**：几条长流就能占满，症状是瞬时 429，不是账号故障。根治靠加号或调大上限。
- **全冷却兜底**：软冷却 / 熔断的号会被"最早到期者"顶班，所以"全冷却"不一定真没号；`hard` 冷却（余额耗尽 → 次日 04:00）**不参与**兜底。
- **`minPickGap`=100ms 防惊群**：连续快速请求可能选不到号（→ `pool_contended`），是设计不是 bug。
- **`credit_floor` 只拦 tier 2（实测收费）模型**：tier 0（实测免费）/ tier 1（无观测）不拦，否则账本过期的触底号会死锁在"学不回来"。
- **模型级冷却 ≠ 账号级冷却**：`code 6004` / `11102` / `11158` 是模型维度，换模型即可，别按号挂了处理。
- **`state.json` 里的计数**（`success_count` / `fails` / `soft_streak` / `token_usage`）是运维判断依据，改语义会让历史数据失真——改前想清楚是否要迁移。
- **本地部署**：`auths` / `data` / `config.json` 是指向 `~/workbuddy2api/` 的符号链接；写该目录超出 workspace 沙箱，需要提权。`build.sh` 末尾的重启会断开在途请求。

## 11. 提交与推送

- 仓库远端：`https://github.com/ruige233/workbuddy2api-panel`（fork 自上游，`main` 单分支）。
- 提交信息用中文，正文说清「为什么改 + 症状/实测证据 + 影响的错误码或配置」，而不是逐文件罗列。
- 推送前：`go build ./... && go vet ./... && go test ./...` 全绿；`git diff --cached` 里没有密钥。
- `README.md` 是唯一允许入库的 Markdown（`.gitignore` 里 `*.md` 全忽略，仅白名单 `README.md`、`AGENTS.md`）。
