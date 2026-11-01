// handler_nopick_test.go 选号失败成因的 HTTP 契约：
// 同一个"选不到号"必须按成因分叉成不同状态码/错误码/Retry-After，
// 否则运维和客户端都只能看到笼统的 no_healthy_account 而无法判断该等还是该加号。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

const nopickBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// TestNoPickInFlightFullIsPoolSaturated 实测踩坑形态：号健康、在途名额被长流占满。
// 契约：429 + pool_saturated + Retry-After，且不得再报"号全挂了"。
func TestNoPickInFlightFullIsPoolSaturated(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	if !p.Acquire("u1") {
		t.Fatal("前置失效：应能占住在途名额")
	}
	defer p.Release("u1")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(nopickBody)))

	if rec.Code != 429 {
		t.Fatalf("code=%d want 429（背压可重试，不是 503）, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pool_saturated") {
		t.Errorf("body=%s want pool_saturated", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("body=%s 不应再出现笼统的 no_healthy_account", body)
	}
	if ra := rec.Header().Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After=%q want \"1\"（背压引导短退避）", ra)
	}
}

// TestNoPickEmptyPoolIsNoAccounts 池子为空：503 no_accounts——重试无用，要加号。
func TestNoPickEmptyPoolIsNoAccounts(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(nopickBody)))

	if rec.Code != 503 {
		t.Fatalf("code=%d want 503, body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "no_accounts") {
		t.Errorf("body=%s want no_accounts", body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("body=%s 不应再出现笼统的 no_healthy_account", body)
	}
	// 池子空不是"稍后再试"能好的事，不引导重试。
	if ra := rec.Header().Get("Retry-After"); ra != "" {
		t.Errorf("Retry-After=%q want 空（空池重试无意义）", ra)
	}
}

// TestNoPickModelBlockedStaysModelUnavailable 模型级全挡 → 400 model_unavailable，
// 不能被前面的选号诊断吃掉（否则用户会以为要加号，实际只需换模型）。
func TestNoPickModelBlockedStaysModelUnavailable(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.BlockModelBackoff("u1", "gpt-x", "11158 not allowed")
	if a := p.PickExcludingForRealm(nil, "gpt-x", ""); a != nil {
		t.Fatalf("模型被挡时不应选到号，got %v", a.UID)
	}

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(
		`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "model_unavailable") {
		t.Errorf("code=%d body=%s want model_unavailable", rec.Code, body)
	}
	if strings.Contains(body, "no_healthy_account") {
		t.Errorf("body=%s 不应再出现笼统的 no_healthy_account", body)
	}
}
