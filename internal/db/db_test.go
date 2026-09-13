package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

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

// 删除 pdfs 记录时，目录项与用户标记都必须被级联清除
func TestDeletePDFCascadesMarks(t *testing.T) {
	database := newTestDB(t)
	id, err := database.AddPDF("/documents/book.pdf", "book", "key-book", 10, 10, 1)
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}
	if _, err := database.AddMark(id, 1, "用户标记", 0, 0, 1); err != nil {
		t.Fatalf("add user mark: %v", err)
	}
	if err := database.AddOutlineMarks(id, []models.OutlineEntry{{Level: 0, Page: 2, Title: "第一章"}}); err != nil {
		t.Fatalf("add outline: %v", err)
	}

	if _, err := database.Exec(`DELETE FROM pdfs WHERE id = ?`, id); err != nil {
		t.Fatalf("delete pdf: %v", err)
	}
	marks, err := database.ListMarks(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 0 {
		t.Fatalf("marks survived cascade delete: %+v", marks)
	}
}

// 宽限期内的软删除记录不能被清理，超期后记录与标记一起消失
func TestPurgeDeletedPDFsHonoursRetention(t *testing.T) {
	database := newTestDB(t)
	id, err := database.AddPDF("/documents/gone.pdf", "gone", "key-gone", 5, 10, 1)
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}
	if _, err := database.AddMark(id, 2, "用户标记", 0, 0, 1); err != nil {
		t.Fatalf("add user mark: %v", err)
	}
	if err := database.SoftDeletePDFs([]int64{id}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	pdf, err := database.GetPDF(id)
	if err != nil {
		t.Fatal(err)
	}
	if !pdf.Deleted || pdf.DeletedAt == nil {
		t.Fatalf("soft delete was not recorded: deleted=%v deletedAt=%v", pdf.Deleted, pdf.DeletedAt)
	}

	// 30 天的宽限期还没过
	purged, err := database.PurgeDeletedPDFs(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatalf("purge within retention: %v", err)
	}
	if len(purged) != 0 {
		t.Fatalf("purged %d record(s) inside the grace period", len(purged))
	}

	// 宽限期已过
	purged, err = database.PurgeDeletedPDFs(time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("purge after retention: %v", err)
	}
	if len(purged) != 1 || purged[0].ID != id || purged[0].CacheKey != "key-gone" {
		t.Fatalf("purged = %+v, want the soft-deleted record", purged)
	}
	if _, err := database.GetPDF(id); err != sql.ErrNoRows {
		t.Errorf("GetPDF after purge error = %v, want sql.ErrNoRows", err)
	}
	if marks, err := database.ListMarks(id); err != nil || len(marks) != 0 {
		t.Errorf("marks after purge = %+v (err=%v)", marks, err)
	}
}

// 重复软删除不能刷新 deleted_at，否则宽限期会被无限延长
func TestSoftDeleteKeepsFirstTimestamp(t *testing.T) {
	database := newTestDB(t)
	id, err := database.AddPDF("/documents/gone.pdf", "gone", "key-gone", 5, 10, 1)
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}
	if err := database.SoftDeletePDFs([]int64{id}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if _, err := database.Exec(`UPDATE pdfs SET deleted_at = 100 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := database.SoftDeletePDFs([]int64{id}); err != nil {
		t.Fatalf("second soft delete: %v", err)
	}
	var stamp int64
	if err := database.QueryRow(`SELECT deleted_at FROM pdfs WHERE id = ?`, id).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp != 100 {
		t.Errorf("deleted_at = %d, want the original 100", stamp)
	}
}

// 孤儿标记（pdf_id 已不存在）必须能被清理
func TestCleanupOrphanMarks(t *testing.T) {
	database := newTestDB(t)
	if _, err := database.Exec(`PRAGMA foreign_keys = off`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`INSERT INTO page_marks (pdf_id, page_num, title) VALUES (999, 1, '孤儿')`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`PRAGMA foreign_keys = on`); err != nil {
		t.Fatal(err)
	}

	removed, err := database.CleanupOrphanMarks()
	if err != nil {
		t.Fatalf("cleanup orphan marks: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
}

// 旧库升级：补 deleted_at、回填时间戳、给 page_marks 加级联
func TestMigrationUpgradesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE pdfs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT UNIQUE NOT NULL,
			title TEXT NOT NULL,
			cache_key TEXT NOT NULL DEFAULT '',
			page_count INTEGER NOT NULL DEFAULT 0,
			file_size INTEGER NOT NULL DEFAULT 0,
			file_mtime INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			cache_active INTEGER NOT NULL DEFAULT 0,
			cache_size INTEGER NOT NULL DEFAULT 0,
			cached_pages INTEGER NOT NULL DEFAULT 0,
			cache_last_access TIMESTAMP,
			cache_version INTEGER NOT NULL DEFAULT 0,
			changed_pages TEXT NOT NULL DEFAULT '',
			deleted INTEGER NOT NULL DEFAULT 0,
			rebuilding INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);
		CREATE TABLE page_marks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pdf_id INTEGER NOT NULL,
			page_num INTEGER NOT NULL,
			title TEXT NOT NULL,
			level INTEGER NOT NULL DEFAULT 0,
			is_outline INTEGER NOT NULL DEFAULT 0,
			sort_order REAL NOT NULL DEFAULT 0,
			parent_id INTEGER DEFAULT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (pdf_id) REFERENCES pdfs(id)
		);
		INSERT INTO pdfs (path, title, deleted, updated_at)
			VALUES ('/documents/gone.pdf', 'gone', 1, '2026-01-02 03:04:05');
		INSERT INTO page_marks (pdf_id, page_num, title) VALUES (1, 3, '旧标记');
		INSERT INTO page_marks (pdf_id, page_num, title) VALUES (42, 1, '孤儿标记');
	`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Init(path)
	if err != nil {
		t.Fatalf("migrate legacy db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	marks, err := database.ListMarks(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 1 || marks[0].Title != "旧标记" {
		t.Fatalf("marks after migration = %+v, want only the valid one", marks)
	}

	pdf, err := database.GetPDF(1)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix()
	if pdf.DeletedAt == nil || pdf.DeletedAt.Unix() != want {
		t.Errorf("deleted_at = %v, want %d", pdf.DeletedAt, want)
	}

	// 重建后的外键必须带级联
	if _, err := database.Exec(`DELETE FROM pdfs WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if marks, err := database.ListMarks(1); err != nil || len(marks) != 0 {
		t.Errorf("marks survived cascade delete = %+v (err=%v)", marks, err)
	}
}

// VACUUM 按间隔执行：首次执行，间隔内跳过，间隔为 0 时不执行
func TestMaybeVacuumRespectsInterval(t *testing.T) {
	database := newTestDB(t)

	done, err := database.MaybeVacuum(time.Hour)
	if err != nil {
		t.Fatalf("first vacuum: %v", err)
	}
	if !done {
		t.Error("first vacuum was skipped, want it to run")
	}

	done, err = database.MaybeVacuum(time.Hour)
	if err != nil {
		t.Fatalf("second vacuum: %v", err)
	}
	if done {
		t.Error("second vacuum ran inside the configured interval")
	}

	if done, err = database.MaybeVacuum(0); err != nil || done {
		t.Errorf("zero interval vacuum = (%v, %v), want (false, nil)", done, err)
	}
}
