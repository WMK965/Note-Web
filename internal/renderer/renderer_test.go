package renderer

import (
	"strings"
	"testing"
)

// 真实 mutool 1.28 输出（来自《生理学（第十版）》，`mutool show -- x.pdf outline`）
const mutoolSample = "|	\"封面页\\x00\"	#page=1&zoom=nan,0,-.2800293\n" +
	"+	\"第一章　绪论\\x00\"	#page=24&zoom=nan,0,-.29101563\n" +
	"-		\"第一节　生理学的研究对象和任务\\x00\"	#page=24&zoom=nan,0,-.29101563\n" +
	"|			\"一、生理学的研究对象\\x00\"	#page=24&zoom=nan,0,-.29101563\n" +
	"|			\"二、生理学的研究任务\\x00\"	#page=25&zoom=nan,0,-.29101563\n" +
	"-		\"第二节　生理学的常用研究方法\\x00\"	#page=26&zoom=nan,0,-.29101563\n" +
	"|			\"一、动物实验\\x00\"	#page=27&zoom=nan,0,-.29101563\n" +
	"|	\"封底页\\x00\"	#page=450&zoom=nan,0,-.2800293\n"

func TestParseMutoolOutline(t *testing.T) {
	entries := parseMutoolOutline(mutoolSample)

	if len(entries) != 8 {
		t.Fatalf("解析出 %d 条目录项，期望 8 条", len(entries))
	}

	want := []struct {
		level int
		page  int
		title string
	}{
		{0, 1, "封面页"},
		{0, 24, "第一章　绪论"},
		{1, 24, "第一节　生理学的研究对象和任务"},
		{2, 24, "一、生理学的研究对象"},
		{2, 25, "二、生理学的研究任务"},
		{1, 26, "第二节　生理学的常用研究方法"},
		{2, 27, "一、动物实验"},
		{0, 450, "封底页"},
	}
	for i, w := range want {
		got := entries[i]
		if got.Level != w.level || got.Page != w.page || got.Title != w.title {
			t.Errorf("第 %d 条 = {level:%d page:%d title:%q}，期望 {level:%d page:%d title:%q}",
				i, got.Level, got.Page, got.Title, w.level, w.page, w.title)
		}
	}
}

// 无法跳转的目标（命名目标、外部链接）不应被误当成页码
func TestParseMutoolOutlineNonPageTargets(t *testing.T) {
	entries := parseMutoolOutline(
		"|	\"命名目标\\x00\"	#nameddest=chapter1\n" +
			"|	\"外部链接\\x00\"	https://example.com/x.pdf\n" +
			"|	\"无目标\\x00\"\n")

	if len(entries) != 3 {
		t.Fatalf("解析出 %d 条目录项，期望 3 条", len(entries))
	}
	for _, e := range entries {
		if e.Page != 0 {
			t.Errorf("%q 的目标无法定位到页面，Page 应为 0，实际 %d", e.Title, e.Page)
		}
	}
}

// 标题中的转义与异常行处理
func TestParseMutoolOutlineEscapesAndNoise(t *testing.T) {
	entries := parseMutoolOutline(
		"warning: repaired broken tree structure in outline\n" +
			"|	\"引号 \\\" 与反斜杠 \\\\ 结束\\x00\"	#page=2&zoom=nan,0,-.5\n" +
			"|	\"\\x00\"	#page=3&zoom=nan,0,-.5\n" +
			"|	\"没有闭合引号	#page=4&zoom=nan,0,-.5\n" +
			"\n")

	if len(entries) != 1 {
		t.Fatalf("解析出 %d 条目录项，期望 1 条（其余为噪声/空标题/引号未闭合）", len(entries))
	}
	got := entries[0]
	if got.Title != `引号 " 与反斜杠 \ 结束` {
		t.Errorf("标题 = %q", got.Title)
	}
	if got.Page != 2 {
		t.Errorf("页码 = %d，期望 2", got.Page)
	}
}

// 层级跳跃时不应崩溃，且层级不超过上限
func TestParseMutoolOutlineDeepIndent(t *testing.T) {
	entries := parseMutoolOutline("|" + strings.Repeat("\t", 50) + "\"过深\\x00\"\t#page=1\n")
	if len(entries) != 1 {
		t.Fatalf("解析出 %d 条目录项", len(entries))
	}
	if entries[0].Level != maxOutlineDepth {
		t.Errorf("层级 = %d，期望被限制为 %d", entries[0].Level, maxOutlineDepth)
	}
}
