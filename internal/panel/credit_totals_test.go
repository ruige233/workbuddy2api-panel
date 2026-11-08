package panel

// 钉住「积分剩余 / 总额」统计条口径：disabled（已禁用）与 paused（暂停选号）账号
// 不参与汇总。它们已退出选号候选，额度是冻结不可用的；若算进剩余/总额，会让
// 首页卡片把「可用额度」虚增，误导运维判断池子里还能花多少分。
// 复用 node+vm 切片起 app.js（仿 frontend_test.go 的 TestAppJSCreditDimensionFormatting），
// 只提取 creditTotals 函数——它是一等契约，逐字钉住过滤逻辑。

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestCreditTotalsExcludesDisabledAndPaused(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit totals test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function creditTotals');
const end = src.indexOf('async function loadOverview');
if (start < 0 || end < 0 || end <= start) throw new Error('creditTotals not found');
const ctx = { Number, String, Boolean, Array, Object };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.creditTotals = creditTotals;', ctx);

const mk = (credits, credits_total, flags) => Object.assign({
  uid: 'u', credits: credits, credits_total: credits_total,
}, flags || {});

const out = {};
// 三个活跃 + 一个禁用 + 一个暂停：只剩前三个计入。
out.mixed = ctx.creditTotals([
  mk(100, 200),
  mk(50, 100),
  mk(25, 50),
  mk(999, 999, { disabled: true }),
  mk(888, 888, { paused: true }),
]);
// 全部禁用/暂停：rem/tot 都应为 0。
out.none = ctx.creditTotals([
  mk(999, 999, { disabled: true }),
  mk(888, 888, { paused: true }),
]);
// 空 / null：不抛异常，回 {0,0}。
out.empty = ctx.creditTotals(null);
out.blank = ctx.creditTotals([]);
// 缺字段（旧数据无 credits_total）：credits 计入、tot 不虚构（0）。
out.legacy = ctx.creditTotals([mk(7, undefined), mk(3, undefined)]);
process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "credit-totals-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	cmd := exec.Command(node, f.Name(), "app.js")
	cmd.Dir = "." // 测试工作目录 = internal/panel
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("creditTotals node test failed: %v\n%s", err, out)
	}
	var got map[string]map[string]float64
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("node 输出不是 JSON: %v\n%s", err, out)
	}
	if g := got["mixed"]; g["rem"] != 175 || g["tot"] != 350 {
		t.Fatalf("mixed rem/tot = %v/%v，want 175/350（禁用与暂停号必须被排除）", g["rem"], g["tot"])
	}
	if g := got["none"]; g["rem"] != 0 || g["tot"] != 0 {
		t.Fatalf("none rem/tot = %v/%v，want 0/0", g["rem"], g["tot"])
	}
	if g := got["empty"]; g["rem"] != 0 || g["tot"] != 0 {
		t.Fatalf("empty rem/tot = %v/%v，want 0/0", g["rem"], g["tot"])
	}
	if g := got["blank"]; g["rem"] != 0 || g["tot"] != 0 {
		t.Fatalf("blank rem/tot = %v/%v，want 0/0", g["rem"], g["tot"])
	}
	if g := got["legacy"]; g["rem"] != 10 || g["tot"] != 0 {
		t.Fatalf("legacy rem/tot = %v/%v，want 10/0（无总额不虚构）", g["rem"], g["tot"])
	}
}
