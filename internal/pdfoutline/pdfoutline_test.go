package pdfoutline

import (
	"os"
	"path/filepath"
	"testing"
)

// 合成一个最小 PDF：两级目录、中文（UTF-16BE）标题、命名目标
func syntheticPDF() []byte {
	return []byte(`%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R /Outlines 5 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R >>
endobj
4 0 obj
<< /Type /Page /Parent 2 0 R >>
endobj
5 0 obj
<< /Type /Outlines /First 6 0 R /Last 8 0 R /Count 3 >>
endobj
6 0 obj
<< /Title (Chapter 1) /Parent 5 0 R /Next 8 0 R /First 7 0 R /Dest [3 0 R /XYZ 0 792 0] >>
endobj
7 0 obj
<< /Title <FEFF7B2C4E0082820000> /Parent 6 0 R /Dest [4 0 R /Fit] >>
endobj
8 0 obj
<< /Title (Chapter 2) /Parent 5 0 R /A << /S /GoTo /D [4 0 R /Fit] >> >>
endobj
trailer
<< /Root 1 0 R /Size 9 >>
%%EOF
`)
}

func TestParseSyntheticPDF(t *testing.T) {
	entries, err := Parse(syntheticPDF())
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []struct {
		level int
		page  int
		title string
	}{
		{0, 1, "Chapter 1"},
		{1, 2, "第一节"},
		{0, 2, "Chapter 2"},
	}
	if len(entries) != len(want) {
		t.Fatalf("条目数 = %d, 期望 %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		if entries[i].Level != w.level || entries[i].Page != w.page || entries[i].Title != w.title {
			t.Errorf("第 %d 条 = %+v, 期望 level=%d page=%d title=%q", i, entries[i], w.level, w.page, w.title)
		}
	}
}

func TestParseRejectsNonPDF(t *testing.T) {
	if _, err := Parse([]byte("not a pdf at all")); err == nil {
		t.Error("Parse accepted a non-PDF payload")
	}
	if _, err := Parse([]byte("%PDF-1.4\n")); err == nil {
		t.Error("缺少 Catalog 的残缺 PDF 应当报错")
	}
}

// 体积上限可由调用方指定：超过上限直接拒绝，避免超大附件把内存吃满
func TestParseWithOptionsHonoursMaxFileSize(t *testing.T) {
	data := syntheticPDF()
	if _, err := ParseWithOptions(data, Options{MaxFileSize: int64(len(data))}); err != nil {
		t.Fatalf("上限等于文件大小时不应报错: %v", err)
	}
	if _, err := ParseWithOptions(data, Options{MaxFileSize: int64(len(data) - 1)}); err == nil {
		t.Error("超过上限时应当报错")
	}
	// 0 表示使用默认上限
	if entries, err := ParseWithOptions(data, Options{}); err != nil || len(entries) == 0 {
		t.Errorf("默认上限应当能正常解析: entries=%d err=%v", len(entries), err)
	}
}

// 合法但没有 /Outlines 的 PDF 应当返回空结果而不是错误
func TestParsePDFWithoutOutline(t *testing.T) {
	data := []byte(`%PDF-1.4
1 0 obj
<< /Type /Catalog /Pages 2 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R] /Count 1 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R >>
endobj
trailer
<< /Root 1 0 R >>
%%EOF
`)
	entries, err := Parse(data)
	if err != nil || len(entries) != 0 {
		t.Errorf("无目录 PDF = %v (err=%v), 期望空结果且无错误", entries, err)
	}
}

// /A 是间接引用（形如 /A 9 0 R）时，动作字典在别的对象里，也必须能解出页码
func TestParseOutlineWithIndirectAction(t *testing.T) {
	data := []byte(`%PDF-1.7
1 0 obj
<< /Type /Catalog /Pages 2 0 R /Outlines 5 0 R >>
endobj
2 0 obj
<< /Type /Pages /Kids [3 0 R 4 0 R] /Count 2 >>
endobj
3 0 obj
<< /Type /Page /Parent 2 0 R >>
endobj
4 0 obj
<< /Type /Page /Parent 2 0 R >>
endobj
5 0 obj
<< /Type /Outlines /First 6 0 R /Last 8 0 R /Count 3 >>
endobj
6 0 obj
<< /Title (Chapter 1) /Parent 5 0 R /Next 8 0 R /First 7 0 R /A 9 0 R >>
endobj
7 0 obj
<< /Title (Section 1.1) /Parent 6 0 R /A 10 0 R >>
endobj
8 0 obj
<< /Title (Chapter 2) /Parent 5 0 R /Dest [4 0 R /Fit] >>
endobj
9 0 obj
<< /S /GoTo /D [3 0 R /XYZ 0 792 0] >>
endobj
10 0 obj
<< /S /GoTo /D [4 0 R /Fit] >>
endobj
trailer
<< /Root 1 0 R /Size 11 >>
%%EOF
`)
	entries, err := Parse(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []struct {
		level int
		page  int
		title string
	}{
		{0, 1, "Chapter 1"},
		{1, 2, "Section 1.1"},
		{0, 2, "Chapter 2"},
	}
	if len(entries) != len(want) {
		t.Fatalf("条目数 = %d, 期望 %d: %+v", len(entries), len(want), entries)
	}
	for i, w := range want {
		if entries[i].Level != w.level || entries[i].Page != w.page || entries[i].Title != w.title {
			t.Errorf("第 %d 条 = %+v, 期望 level=%d page=%d title=%q", i, entries[i], w.level, w.page, w.title)
		}
	}
}

// 真实样本：文件名与目录内容都由用户提供的《生理学（第十版）》
func TestParseRealBookIfPresent(t *testing.T) {
	path := filepath.Join("..", "..", "tmp", "生理学（第十版）.pdf")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("样本不存在: %v", err)
	}
	entries, err := Parse(data)
	if err != nil {
		t.Fatalf("解析样本: %v", err)
	}
	if len(entries) < 100 {
		t.Fatalf("条目数 = %d, 明显偏少", len(entries))
	}
	levels := map[int]int{}
	for _, e := range entries {
		levels[e.Level]++
		if e.Title == "" {
			t.Errorf("出现空标题: %+v", e)
		}
	}
	if levels[0] == 0 || levels[1] == 0 || levels[2] == 0 {
		t.Errorf("层级分布异常: %v", levels)
	}
	t.Logf("样本目录: %d 条，层级分布 %v", len(entries), levels)
	if entries[0].Title != "封面页" || entries[0].Page != 1 || entries[0].Level != 0 {
		t.Errorf("首条 = %+v, 期望 封面页 第1页 0 级", entries[0])
	}
	// 抽查若干条：标题与页码都要落在合理范围内
	for _, e := range entries {
		if e.Page < 0 || e.Page > 450 {
			t.Errorf("页码越界: %+v", e)
		}
	}
}
