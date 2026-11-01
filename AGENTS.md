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

## 9. 已知坑（踩过的，别再踩）

- **`no_healthy_account` 的历史包袱**：它曾同时表示「在途满 / 积分保底 / 冷却 / 池空」，把网关背压读成了"号全挂了"。现已按成因拆码（`pool_saturated` / `pool_contended` / `all_accounts_cooling` / `no_accounts` / `credit_floor_reached` / `rotation_exhausted`）。**`POST /v1/search/messages` 仍回固定 503 `no_healthy_account`**（`server/search.go`，未统一）。
- **`pool.max_in_flight` 默认 3（global 域 2）**：几条长流就能占满，症状是瞬时 429，不是账号故障。根治靠加号或调大上限。
- **全冷却兜底**：软冷却 / 熔断的号会被"最早到期者"顶班，所以"全冷却"不一定真没号；`hard` 冷却（余额耗尽 → 次日 04:00）**不参与**兜底。
- **`minPickGap`=100ms 防惊群**：连续快速请求可能选不到号（→ `pool_contended`），是设计不是 bug。
- **`credit_floor` 只拦 tier 2（实测收费）模型**：tier 0（实测免费）/ tier 1（无观测）不拦，否则账本过期的触底号会死锁在"学不回来"。
- **模型级冷却 ≠ 账号级冷却**：`code 6004` / `11102` / `11158` 是模型维度，换模型即可，别按号挂了处理。
- **`state.json` 里的计数**（`success_count` / `fails` / `soft_streak` / `token_usage`）是运维判断依据，改语义会让历史数据失真——改前想清楚是否要迁移。
- **本地部署**：`auths` / `data` / `config.json` 是指向 `~/workbuddy2api/` 的符号链接；写该目录超出 workspace 沙箱，需要提权。`build.sh` 末尾的重启会断开在途请求。

## 10. 提交与推送

- 仓库远端：`https://github.com/ruige233/workbuddy2api-panel`（fork 自上游，`main` 单分支）。
- 提交信息用中文，正文说清「为什么改 + 症状/实测证据 + 影响的错误码或配置」，而不是逐文件罗列。
- 推送前：`go build ./... && go vet ./... && go test ./...` 全绿；`git diff --cached` 里没有密钥。
- `README.md` 是唯一允许入库的 Markdown（`.gitignore` 里 `*.md` 全忽略，仅白名单 `README.md`、`AGENTS.md`）。
