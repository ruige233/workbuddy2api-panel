// diagnose_test.go 选号失败成因诊断（DiagnoseNoPick）的口径约束：
// 每种成因必须能被单独识别——混成一个 no_healthy_account 就失去了可操作性。
package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// TestDiagnoseEmptyPool 池子为空：成因 empty（重试无用，要加号）。
func TestDiagnoseEmptyPool(t *testing.T) {
	p := New("")
	d := p.DiagnoseNoPick("m", "", nil)
	if d.Reason != NoPickEmpty {
		t.Fatalf("reason=%q want %q", d.Reason, NoPickEmpty)
	}
	if d.Total != 0 {
		t.Errorf("total=%d want 0", d.Total)
	}
}

// TestDiagnoseInFlightFull 号健康但在途名额占满 → in_flight_full（纯背压，短退避）。
//
// 这是实测踩坑的形态：两个号被几条长流占满 max_in_flight，网关回 no_healthy_account，
// 运维以为号全挂了。诊断必须把它与"冷却/禁用"分开。
func TestDiagnoseInFlightFull(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("前置失效：应能占住在途名额")
	}
	defer p.Release("u1")

	// 选号层先确认确实选不到（诊断不能凭空造结论）。
	if a := p.PickExcludingForRealm(nil, "m", ""); a != nil {
		t.Fatalf("在途占满时应选不到号，got %v", a.UID)
	}
	d := p.DiagnoseNoPick("m", "", nil)
	if d.Reason != NoPickInFlightFull {
		t.Fatalf("reason=%q want %q（%+v）", d.Reason, NoPickInFlightFull, d)
	}
	if d.InFlightFull != 1 || d.Total != 1 || d.Cooling != 0 {
		t.Errorf("counts=%+v want in_flight_full=1 total=1 cooling=0", d)
	}
	// 背压不预知恢复时间：不给 RetryAfter（server 层固定 1s 引导短退避）。
	if d.RetryAfter != 0 {
		t.Errorf("retryAfter=%v want 0（背压无确定恢复时刻）", d.RetryAfter)
	}
}

// TestDiagnoseCooling 全账号 hard 冷却 → cooling + 最早解冻时刻与 RetryAfter。
func TestDiagnoseCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000, 0)
	p.SetCredits("u2", 1000, 0)
	// CoolHard（余额耗尽，等签到）不参与全冷却兜底 → pick 真的返回 nil。
	p.Cooldown("u1", CoolHard, 2*time.Hour, "hard_credit")
	p.Cooldown("u2", CoolHard, time.Hour, "hard_credit")

	if a := p.PickExcludingForRealm(nil, "m", ""); a != nil {
		t.Fatalf("全 hard 冷却时应选不到号，got %v", a.UID)
	}
	d := p.DiagnoseNoPick("m", "", nil)
	if d.Reason != NoPickCooling {
		t.Fatalf("reason=%q want %q（%+v）", d.Reason, NoPickCooling, d)
	}
	if d.Cooling != 2 {
		t.Errorf("cooling=%d want 2", d.Cooling)
	}
	// 最早解冻取两者中较早的（u2，约 1h）：文案/Retry-After 都要按"最早能好"算，
	// 否则客户端会白等两小时。
	if d.RetryAfter <= 50*time.Minute || d.RetryAfter > time.Hour {
		t.Errorf("retryAfter=%v want ≈1h（取最早解冻者）", d.RetryAfter)
	}
	if d.EarliestRecovery.IsZero() {
		t.Error("earliestRecovery 不应为零值")
	}
}

// TestDiagnoseCreditFloor 全号触底 + 收费模型 → credit_floor（等签到，不是号挂了）。
func TestDiagnoseCreditFloor(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCreditFloor(100)
	p.SetCredits("poor", 30, 0)
	p.NoteModelCost("poor", "paid-model", 2.9, 1000)

	if a := p.PickExcludingForRealm(nil, "paid-model", ""); a != nil {
		t.Fatalf("触底号应被保底拦住，got %v", a.UID)
	}
	d := p.DiagnoseNoPick("paid-model", "", nil)
	if d.Reason != NoPickCreditFloor {
		t.Fatalf("reason=%q want %q（%+v）", d.Reason, NoPickCreditFloor, d)
	}
	if d.CreditFloor != 1 {
		t.Errorf("credit_floor=%d want 1", d.CreditFloor)
	}
}

// TestDiagnoseModelBlocked 号健康、但对请求模型全被挡 → model_blocked（换模型）。
func TestDiagnoseModelBlocked(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)
	p.BlockModelBackoff("u1", "gpt-x", "11158 not allowed")

	d := p.DiagnoseNoPick("gpt-x", "", nil)
	if d.Reason != NoPickModelBlocked {
		t.Fatalf("reason=%q want %q（%+v）", d.Reason, NoPickModelBlocked, d)
	}
	// 同一账号换个模型仍可服务：诊断不能把"模型被挡"报成"池子没号"。
	if got := p.DiagnoseNoPick("other-model", "", nil); got.Reason != NoPickNone || got.Healthy != 1 {
		t.Errorf("换模型 reason=%q healthy=%d want %q/1（%+v）", got.Reason, got.Healthy, NoPickNone, got)
	}
}

// TestDiagnoseExhausted 本轮已试遍：exhausted（真因在上游，不在池子）。
func TestDiagnoseExhausted(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)

	d := p.DiagnoseNoPick("m", "", map[string]bool{"u1": true})
	if d.Reason != NoPickExhausted {
		t.Fatalf("reason=%q want %q（%+v）", d.Reason, NoPickExhausted, d)
	}
	if d.Exhausted != 1 {
		t.Errorf("exhausted=%d want 1", d.Exhausted)
	}
}

// TestDiagnoseRaceIsNone 诊断口径下还有号可选（pick 的 nil 来自并发抢名额或防撞号
// 窗口）：报 NoPickNone，不伪造成"没号"——server 层据此给"立刻重试"。
func TestDiagnoseRaceIsNone(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000, 0)

	d := p.DiagnoseNoPick("m", "", nil)
	if d.Reason != NoPickNone {
		t.Fatalf("reason=%q want %q（%+v）", d.Reason, NoPickNone, d)
	}
	if d.Healthy != 1 {
		t.Errorf("healthy=%d want 1（确实有号可选，只是瞬态抢不到）", d.Healthy)
	}
}

// TestDiagnoseRealmScoped realm 谓词生效：别的域有号不算"这个域有号"。
func TestDiagnoseRealmScoped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "example.workbuddy.ai"})
	p.SetCredits("g1", 1000, 0)

	if d := p.DiagnoseNoPick("m", "cn", nil); d.Reason != NoPickEmpty {
		t.Fatalf("cn 域 reason=%q want %q（%+v）", d.Reason, NoPickEmpty, d)
	}
	if d := p.DiagnoseNoPick("m", "", nil); d.Reason != NoPickNone || d.Healthy != 1 {
		t.Fatalf("全池 reason=%q healthy=%d want %q/1（%+v）", d.Reason, d.Healthy, NoPickNone, d)
	}
}
