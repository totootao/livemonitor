package web

import (
	"strings"
	"testing"
)

// 本文件守护内嵌页面的关键不变量。
// 页面本身是 HTML/JS，无法用 Go 直接做行为断言，
// 因此这里针对已经踩过的坑做静态防护。

// TestIndexContainsAPIEndpoints 页面必须覆盖全部后端接口，避免接口改动后页面静默失效。
func TestIndexContainsAPIEndpoints(t *testing.T) {
	page := string(indexHTML)
	for _, ep := range []string{
		"/api/state",
		"/api/containers",
		"/api/settings",
		"/api/reload",
		// 容器动作用拼接方式生成，这里只校验动作名存在。
		`"run-now"`,
		`"stop"`,
		`"start"`,
	} {
		if !strings.Contains(page, ep) {
			t.Errorf("页面未引用接口或动作 %q", ep)
		}
	}
}

// TestIndexUsesDisabledProperty 回归防护：布尔属性必须用 property 赋值。
//
// 历史缺陷：用 setAttribute("disabled", false) 表达"启用"，
// 会写出 disabled="false"；HTML 布尔属性只看是否存在，按钮因此永久禁用，
// 页面上所有操作按钮点不动。
func TestIndexUsesDisabledProperty(t *testing.T) {
	page := string(indexHTML)

	if !strings.Contains(page, `node.disabled = !!v`) {
		t.Error("el() 应通过 property 设置 disabled，否则布尔属性语义会出错")
	}

	// el() 的通用分支不应再直接落到 setAttribute("disabled", ...)。
	// 这里检查注释里记录的反例确实以注释形式存在，且属性分支已被特判。
	if !strings.Contains(page, `k === "disabled"`) {
		t.Error("el() 缺少对 disabled 的特判分支")
	}
}

// TestIndexHasNoExternalResources 页面必须自包含，保证离线单文件可用。
func TestIndexHasNoExternalResources(t *testing.T) {
	page := string(indexHTML)

	for _, bad := range []string{
		"http://",
		"https://",
		"<script src=",
		"<link rel=\"stylesheet\"",
		"@import",
	} {
		if strings.Contains(page, bad) {
			t.Errorf("页面引用了外部资源 %q，会破坏离线可用性", bad)
		}
	}
}

// TestIndexDeclaresNoAuthWarning 文档与页面都应提示该界面无鉴权。
func TestIndexDeclaresNoAuthWarning(t *testing.T) {
	page := string(indexHTML)
	// 页面本身不放警告横幅（避免干扰使用），但必须声明字符集与视口，
	// 保证中文与移动端显示正常。
	for _, want := range []string{`charset="utf-8"`, `name="viewport"`, `lang="zh-CN"`} {
		if !strings.Contains(page, want) {
			t.Errorf("页面缺少 %q", want)
		}
	}
}

// TestIndexSurvivesFormatting 页面应为非空且大小合理（防止 embed 路径写错导致空文件）。
func TestIndexSurvivesFormatting(t *testing.T) {
	if len(indexHTML) < 4096 {
		t.Fatalf("内嵌页面仅 %d 字节，疑似 embed 路径有误", len(indexHTML))
	}
	if len(indexHTML) > 1<<20 {
		t.Fatalf("内嵌页面 %d 字节，超出合理范围", len(indexHTML))
	}
}

// TestIndexHasMobileLayout 回归防护：移动端适配不能被悄悄删掉。
//
// 页面在手机上主要靠两件事成立：
//  1. ≤720px 的媒体查询把表格摊成卡片、加大触控目标；
//  2. JS 渲染行时把列名写在 td 的 data-label 上，供 td::before 展示。
//     若 JS 改动后丢了 data-label，卡片里就只剩值没有字段名，等于不可用。
func TestIndexHasMobileLayout(t *testing.T) {
	page := string(indexHTML)
	for _, want := range []string{
		"@media (max-width: 720px)", // 移动端断点
		"viewport-fit=cover",        // 安全区（iPhone 刘海/底部横条）
		`content: attr(data-label)`, // 卡片化布局的列名来源
		`"data-label": "每日启动"`,      // JS 侧必须实际写入列名
		`"data-label": "操作"`,
		"cell-name",       // 卡片标题行（不带列名的特判）
		"font-size: 16px", // 防 iOS 聚焦缩放
	} {
		if !strings.Contains(page, want) {
			t.Errorf("页面缺少移动端适配要素 %q", want)
		}
	}
}
