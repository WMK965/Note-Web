package db

import (
	"path/filepath"
	"testing"

	"note-web/internal/models"
)

func newTestDB(t *testing.T) *Database {
	t.Helper()
	database, err := Init(filepath.Join(t.TempDir(), "note-web.db"))
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

// 目录项必须按层级建立父子关系，前端才能渲染成树
func TestAddOutlineMarksBuildsHierarchy(t *testing.T) {
	database := newTestDB(t)

	id, err := database.AddPDF("/documents/book.pdf", "book", "key-book", 100, 10, 1)
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}

	entries := []models.OutlineEntry{
		{Level: 0, Page: 1, Title: "第一章"},
		{Level: 1, Page: 2, Title: "第一节"},
		{Level: 2, Page: 3, Title: "一、小节"},
		{Level: 1, Page: 4, Title: "第二节"},
		{Level: 0, Page: 5, Title: "第二章"},
		{Level: 3, Page: 6, Title: "层级跳跃"},
	}
	if err := database.AddOutlineMarks(id, entries); err != nil {
		t.Fatalf("add outline marks: %v", err)
	}

	marks, err := database.ListMarks(id)
	if err != nil {
		t.Fatalf("list marks: %v", err)
	}
	if len(marks) != len(entries) {
		t.Fatalf("标记数 = %d，期望 %d", len(marks), len(entries))
	}

	// 期望的父级（按下标引用，-1 表示顶级）
	wantParents := []int{-1, 0, 1, 0, -1, 4}
	wantLevels := []int{0, 1, 2, 1, 0, 1} // 层级跳跃被收敛到上一层
	for i, m := range marks {
		want := int64(0)
		if wantParents[i] >= 0 {
			want = marks[wantParents[i]].ID
		}
		if m.ParentID != want {
			t.Errorf("第 %d 条 %q 的 parent_id = %d，期望 %d", i, m.Title, m.ParentID, want)
		}
		if m.Level != wantLevels[i] {
			t.Errorf("第 %d 条 %q 的 level = %d，期望 %d", i, m.Title, m.Level, wantLevels[i])
		}
		if !m.IsOutline {
			t.Errorf("第 %d 条应标记为 PDF 自带目录", i)
		}
	}
}

// 重新提取目录时应整体替换旧目录项
func TestAddOutlineMarksReplacesPrevious(t *testing.T) {
	database := newTestDB(t)

	id, err := database.AddPDF("/documents/book.pdf", "book", "key-book", 100, 10, 1)
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}

	if err := database.AddOutlineMarks(id, []models.OutlineEntry{{Level: 0, Page: 1, Title: "旧目录"}}); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if err := database.DeleteOutlineMarks(id); err != nil {
		t.Fatalf("delete outline: %v", err)
	}
	if err := database.AddOutlineMarks(id, []models.OutlineEntry{{Level: 0, Page: 1, Title: "新目录"}}); err != nil {
		t.Fatalf("second add: %v", err)
	}

	marks, err := database.ListMarks(id)
	if err != nil {
		t.Fatalf("list marks: %v", err)
	}
	if len(marks) != 1 || marks[0].Title != "新目录" {
		t.Fatalf("目录项 = %+v，期望只有“新目录”", marks)
	}
}

func TestReplaceOutlineMarksFromGoodNotesRecordsSource(t *testing.T) {
	database := newTestDB(t)
	id, err := database.AddPDF("/documents/book.pdf", "book", "key-book", 10, 10, 1)
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}
	userMarkID, err := database.AddMark(id, 3, "用户标记", 0, 0, 1)
	if err != nil {
		t.Fatalf("add user mark: %v", err)
	}

	entries := []models.OutlineEntry{
		{Level: 0, Page: 2, Title: "第一章"},
		{Level: 1, Page: 3, Title: "第一节"},
	}
	if err := database.ReplaceOutlineMarksFromGoodNotes(id, entries, "/documents/book.goodnotes", 123, 456); err != nil {
		t.Fatalf("replace GoodNotes outline: %v", err)
	}
	pdf, err := database.GetPDF(id)
	if err != nil {
		t.Fatal(err)
	}
	if pdf.GoodNotesPath != "/documents/book.goodnotes" || pdf.GoodNotesSize != 123 || pdf.GoodNotesMtime != 456 {
		t.Errorf("GoodNotes source = (%q, %d, %d)", pdf.GoodNotesPath, pdf.GoodNotesSize, pdf.GoodNotesMtime)
	}
	marks, err := database.ListMarks(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 3 {
		t.Fatalf("mark count = %d, want 3", len(marks))
	}
	foundUser := false
	for _, mark := range marks {
		if mark.ID == userMarkID {
			foundUser = true
			if mark.IsOutline {
				t.Error("user mark became an outline mark")
			}
		}
	}
	if !foundUser {
		t.Fatal("user mark was lost while replacing the outline")
	}
}
