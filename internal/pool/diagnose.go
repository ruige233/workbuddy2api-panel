// 选号失败成因诊断：把「pick 返回 nil」拆成互不相同的可执行原因。
//
// 为什么需要它：pick 返回 nil 时网关此前一律回 503 no_healthy_account
// （"all accounts are temporarily unavailable"）。但 nil 至少有六种成因，
// 客户端该做的事完全不同：
//
//	empty          池子/该域没有账号          → 加号（重试无意义）
//	in_flight_full 号健康、在途名额占满        → 短退避重试（纯背压）
//	cooling        全号账号级冷却（软限流/熔断/降权）→ 等冷却到期再试
//	model_blocked  号在、但对该模型全被挡     → 换模型（现有 400 model_unavailable 路径）
//	credit_floor   全号低于积分保底线          → 等签到回血
//	exhausted      本轮轮转已试遍              → 真因是上次上游错误，见 lastErr
//
// 实测踩过的坑：运维看到 no_healthy_account 以为"号全挂了"，实际只是两个号被
// 几条长流把 max_in_flight 占满（背压），一分钟内自愈。错误码不区分成因，
// 排查方向就必然跑偏——所以把分类留在 pool 层（它才知道真实状态），
// 文案与 HTTP 状态映射留在 server 层（它才知道 OpenAI 兼容口径）。
package pool

import "time"

// NoPickReason 选号失败成因分类。空串 = 池中确实有号可选（pick 失败属并发竞态）。
type NoPickReason string

const (
	// NoPickNone 诊断口径下仍有可服务账号：pick 的 nil 来自并发抢名额或
	// minPickGap 防撞号窗口（两者都是瞬态，客户端动作等同于 in_flight_full）。
	NoPickNone NoPickReason = ""
	// NoPickEmpty 该 realm 没有任何账号（未导入 / realm 前缀写错）。
	NoPickEmpty NoPickReason = "empty"
	// NoPickInFlightFull 存在健康账号，但都在途名额占满（纯网关侧背压）。
	NoPickInFlightFull NoPickReason = "in_flight_full"
	// NoPickCooling 全部账号处于账号级冷却（软限流 / 熔断 / 连败降权 / 余额耗尽）。
	NoPickCooling NoPickReason = "cooling"
	// NoPickModelBlocked 账号健康，但对请求模型全部处于模型级冷却（11102/11158/6004）。
	NoPickModelBlocked NoPickReason = "model_blocked"
	// NoPickCreditFloor 全部账号低于积分保底线，且该模型收费（保底拦截）。
	NoPickCreditFloor NoPickReason = "credit_floor"
	// NoPickExhausted 本轮轮转已把候选号试遍（成因在上游，见 lastErr）。
	NoPickExhausted NoPickReason = "exhausted"
	// NoPickUnavailable 判不出单一成因（混合形态且无可用号）。
	NoPickUnavailable NoPickReason = "unavailable"
)

// PickDiagnosis 一次选号失败的成因快照（只读诊断结果，不含任何账号敏感信息）。
type PickDiagnosis struct {
	// Diagnosed 标记本结构由 DiagnoseNoPick 填充（零值 = 没诊断过）。
	// 必须显式标记：Reason 的空串既表示"池子确实有号"，也是零值，无法自证。
	Diagnosed bool
	Reason    NoPickReason
	Realm     string // 诊断范围（"" = 全池）
	Model     string // 请求裸模型名（"" = 无模型维度）

	// 账号计数（互斥分桶，Total = 各桶之和）。
	Total        int
	Healthy      int // 真正可服务（健康 + 模型可用 + 未触底 + 在途未满 + 未试过）
	Cooling      int // 账号级冷却中
	Disabled     int
	Paused       int
	InFlightFull int
	ModelBlocked int
	CreditFloor  int
	Exhausted    int // 本轮已试过（tried）

	// EarliestRecovery 冷却类成因下最早解冻时刻（零值 = 无观测）。
	EarliestRecovery time.Time
	// RetryAfter 建议重试间隔（EarliestRecovery-now；冷却类成因之外为 0）。
	RetryAfter time.Duration
}

// DiagnoseNoPick 只读诊断「为什么选不到号」，口径与 pick 的候选过滤一致。
//
// tried 传本轮已试过的 UID（可为 nil）；realm=="" 统计全池。调用方在 pick 返回
// nil 后调用一次即可——它持 RLock 遍历，不改状态、不触发冷却、不写盘。
func (p *Pool) DiagnoseNoPick(model, realm string, tried map[string]bool) PickDiagnosis {
	d := PickDiagnosis{Diagnosed: true, Realm: realm, Model: model}
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		d.Total++
		switch {
		case e.disabled:
			d.Disabled++
		case e.paused:
			d.Paused++
		case !e.healthy(now):
			// 账号级冷却/熔断/降权：最早解冻时刻取三者中最近的有效截止。
			d.Cooling++
			if until := e.expiry(now); !until.IsZero() {
				if d.EarliestRecovery.IsZero() || until.Before(d.EarliestRecovery) {
					d.EarliestRecovery = until
				}
			}
		case !e.healthyForModel(now, model):
			// 账号本身健康，只是该模型处于 11102/11158/6004 模型级冷却。
			d.ModelBlocked++
		case p.floorBlockedForRealmModel(e, model, realm, now):
			d.CreditFloor++
		case p.inFlightFull(e):
			// 在途满优先于 tried：它的客户端动作（短退避）比"已试过"更具体。
			d.InFlightFull++
		case tried[uid]:
			d.Exhausted++
		default:
			d.Healthy++
		}
	}

	switch {
	case d.Total == 0:
		d.Reason = NoPickEmpty
	case d.Healthy > 0:
		// 诊断口径下还有号可选：pick 的 nil 只能来自并发抢名额或防撞号窗口（瞬态）。
		// 保持 NoPickNone 这个诚实口径——池子并没有"没号"，server 层据此给
		// "立刻重试"而不是"号全挂了"。
		d.Reason = NoPickNone
	case d.InFlightFull > 0:
		d.Reason = NoPickInFlightFull
	case d.ModelBlocked > 0 && d.Cooling == 0:
		// 只剩模型维度：换模型就能解决，交给 server 层的 model_unavailable 口径。
		d.Reason = NoPickModelBlocked
	case d.CreditFloor > 0 && d.Cooling == 0:
		d.Reason = NoPickCreditFloor
	case d.Cooling > 0:
		d.Reason = NoPickCooling
		if !d.EarliestRecovery.IsZero() {
			d.RetryAfter = d.EarliestRecovery.Sub(now)
			if d.RetryAfter < 0 {
				d.RetryAfter = 0
			}
		}
	case d.Exhausted > 0:
		d.Reason = NoPickExhausted
	default:
		// 全被禁用/暂停，或混合形态无单一主因。
		d.Reason = NoPickUnavailable
	}
	return d
}
