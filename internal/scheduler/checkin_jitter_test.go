package scheduler

import (
	"context"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// TestCheckinJitterRange 抖动值必须落在 [0, max) 且 max<=0 时为零：
// 抖动是「每天准点」这个机器人特征的唯一反制，越界即失去意义。
func TestCheckinJitterRange(t *testing.T) {
	if got := checkinJitter(0); got != 0 {
		t.Errorf("checkinJitter(0) = %v, want 0", got)
	}
	if got := checkinJitter(-time.Minute); got != 0 {
		t.Errorf("checkinJitter(-1m) = %v, want 0", got)
	}
	const max = 3 * time.Minute
	for i := 0; i < 200; i++ {
		d := checkinJitter(max)
		if d < 0 || d >= max {
			t.Fatalf("checkinJitter(%v) = %v, out of [0,max)", max, d)
		}
	}
}

// TestCheckinJitterSpread 200 次采样应铺满窗口的每个四分位：
// 均匀性差（例如全挤在低值段）会让签到时刻仍可被聚类识别。
func TestCheckinJitterSpread(t *testing.T) {
	const max = 3 * time.Minute
	const n = 200
	quarters := make([]int, 4)
	for i := 0; i < n; i++ {
		d := checkinJitter(max)
		q := int(d / (max / 4))
		if q > 3 {
			q = 3
		}
		quarters[q]++
	}
	for q, c := range quarters {
		// 均匀分布下每桶期望 50；crypto/rand 偶然性放宽到 15..85。
		if c < 15 || c > 85 {
			t.Errorf("quarter %d count = %d (of %d), outside plausible uniform range", q, c, n)
		}
	}
}

// TestRunBatchCheckinJitterSleeps 排程派发路径必须先睡抖动再跑签到：
// 用 jitterMax=2s 的短窗口验证 runBatch(taskCheckin) 的耗时 ≥ 一个非零抖动，
// 并在日志里留下 jitter 行（可观测：用户能在 service 日志里看到当天抖了多少）。
func TestRunBatchCheckinJitterSleeps(t *testing.T) {
	oldMax := checkinJitterMax
	checkinJitterMax = 2 * time.Second
	t.Cleanup(func() { checkinJitterMax = oldMax })

	var buf strings.Builder
	oldOut := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(oldOut) })

	s := New(Config{Pool: pool.New("")})
	start := time.Now()
	s.runBatch(context.Background(), []taskKind{taskCheckin})
	elapsed := time.Since(start)
	// 抖动 [0,2s)：平均 1s。只断言「明显非零」与「不超上限」，不断言精确值。
	if elapsed < 200*time.Millisecond {
		t.Fatalf("runBatch returned in %v, jitter sleep missing?", elapsed)
	}
	if elapsed > checkinJitterMax+5*time.Second {
		t.Fatalf("runBatch took %v, exceeds jitter window", elapsed)
	}
	if !strings.Contains(buf.String(), "checkin jitter") {
		t.Errorf("log missing jitter line, got: %s", buf.String())
	}
}
