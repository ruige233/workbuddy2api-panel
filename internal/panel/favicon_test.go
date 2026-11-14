package panel

// 标签页图标的接线测试（2026-11-14）：浏览器地址栏/标签页此前空白，
// 因为面板从未提供 favicon，index.html 也没有 <link rel="icon">。
// 图标取自官方 WorkBuddy 桌面端 icon.icns（产品本身那枚），本文件只钉住「接线」：
// 路由可达、Content-Type 正确、index.html 确实引用了它们。
//
// 为什么不钉图标内容本身：图标是美术资源，换一版图标不该让测试变红；
// 但「HTML 引用了不存在的路径」这类断链必须被测试拦住。

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFaviconRoutesServeImage 两个图标路由必须返回 200 + 正确的图片 Content-Type
// + 非空 body。Content-Type 写错会让浏览器按文本渲染、图标不显示。
func TestFaviconRoutesServeImage(t *testing.T) {
	p := newTestPanel()
	cases := []struct{ path, ctype string }{
		{"/panel/favicon.ico", "image/x-icon"},
		{"/panel/icon-180.png", "image/png"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s: status=%d，want 200", c.path, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != c.ctype {
			t.Errorf("%s: Content-Type=%q，want %q", c.path, got, c.ctype)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s: 响应体为空", c.path)
		}
		// 图标是静态资源，必须带长缓存（否则每次开面板都重下）。
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age=") {
			t.Errorf("%s: Cache-Control=%q，缺少 max-age", c.path, cc)
		}
		// 安全头仍须齐全（与其它面板资源同口径）。
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: 缺少 CSP", c.path)
		}
	}
}

// TestIndexHTMLReferencesFavicon index.html 必须真的引用图标，且引用的路径
// 与服务实际暴露的路由一致——断链是本次要防的回归。
func TestIndexHTMLReferencesFavicon(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	for _, want := range []string{
		`rel="icon"`,
		`href="favicon.ico"`,
		`rel="apple-touch-icon"`,
		`href="icon-180.png"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index.html 缺少 %s", want)
		}
	}

	// 引用是相对路径，浏览器按文档 URL（/panel/）解析 → 实际请求 /panel/<name>。
	// 逐个确认服务端确实能出这两个文件，避免「HTML 写了但路由没挂」。
	for _, name := range []string{"favicon.ico", "icon-180.png"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/"+name, nil))
		if rec.Code != 200 || rec.Body.Len() == 0 {
			t.Fatalf("index.html 引用的 /panel/%s 实际取不到（status=%d, len=%d）",
				name, rec.Code, rec.Body.Len())
		}
	}
}

// TestFaviconIsRealICO 防「把 png 改后缀当 ico 发」：ICO 文件头必须是
// 00 00 01 00（reserved=0, type=1=icon）。这类错误在浏览器里表现为图标静默不显示，
// 排查成本高，故在测试层拦住。
func TestFaviconIsRealICO(t *testing.T) {
	if len(faviconICO) < 6 {
		t.Fatalf("favicon.ico 过短: %d 字节", len(faviconICO))
	}
	if !bytes.Equal(faviconICO[:4], []byte{0x00, 0x00, 0x01, 0x00}) {
		t.Fatalf("favicon.ico 文件头不是 ICO 格式: % x", faviconICO[:4])
	}
	// PNG 文件头（apple-touch-icon 必须是真 PNG）。
	if !bytes.Equal(appleTouchIcon[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		t.Fatalf("icon-180.png 不是 PNG 格式")
	}
}
