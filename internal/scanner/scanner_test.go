package scanner

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/util"
)

// fakeCache 记录扫描器对缓存目录的操作，便于断言清理行为
type fakeCache struct {
	removed []string
}

func (f *fakeCache) InvalidatePDF(int64) error { return nil }

func (f *fakeCache) RemoveCacheDir(cacheKey string) error {
	f.removed = append(f.removed, cacheKey)
	return nil
}

func (f *fakeCache) SweepOrphanCacheDirs(map[string]bool) (int, int64, error) {
	return 0, 0, nil
}

// 文件消失后先软删除（宽限期内保留标记），宽限期结束再连同缓存一起清理
func TestScanSoftDeletesThenPurgesMissingPDF(t *testing.T) {
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "gone.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4 minimal"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Init(filepath.Join(dir, "note-web.db"))
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	key := util.CacheKey(pdfPath)
	id, err := database.AddPDF(pdfPath, "gone", key, 3, info.Size(), info.ModTime().Unix())
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}
	if _, err := database.AddMark(id, 2, "用户标记", 0, 0, 1); err != nil {
		t.Fatalf("add mark: %v", err)
	}
	if err := os.Remove(pdfPath); err != nil {
		t.Fatalf("remove pdf: %v", err)
	}

	cache := &fakeCache{}
	cfg := config.PDFConfig{Dirs: []string{dir}, ScanInterval: 300, RetentionDays: 30}
	if err := New(database, nil, cfg, config.DatabaseConfig{}, cache).Scan(); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	pdf, err := database.GetPDF(id)
	if err != nil {
		t.Fatalf("record disappeared inside the grace period: %v", err)
	}
	if !pdf.Deleted || pdf.DeletedAt == nil {
		t.Fatalf("record was not soft-deleted: deleted=%v deletedAt=%v", pdf.Deleted, pdf.DeletedAt)
	}
	if marks, err := database.ListMarks(id); err != nil || len(marks) != 1 {
		t.Fatalf("marks inside the grace period = %+v (err=%v)", marks, err)
	}

	// 宽限期为 0：下一轮扫描直接物理清理
	immediate := config.PDFConfig{Dirs: []string{dir}, ScanInterval: 300, RetentionDays: 0}
	if err := New(database, nil, immediate, config.DatabaseConfig{}, cache).Scan(); err != nil {
		t.Fatalf("second scan: %v", err)
	}

	if _, err := database.GetPDF(id); err != sql.ErrNoRows {
		t.Errorf("record after purge error = %v, want sql.ErrNoRows", err)
	}
	if marks, err := database.ListMarks(id); err != nil || len(marks) != 0 {
		t.Errorf("marks after purge = %+v (err=%v)", marks, err)
	}
	if len(cache.removed) != 1 || cache.removed[0] != key {
		t.Errorf("cache removals = %v, want [%s]", cache.removed, key)
	}
}

// 目录整体不可达（挂载掉线、路径写错）时不能把下面的记录判成已删除
func TestScanKeepsRecordsWhenDirectoryUnavailable(t *testing.T) {
	dir := t.TempDir()
	offlineDir := filepath.Join(dir, "offline")
	pdfPath := filepath.Join(offlineDir, "book.pdf")

	database, err := db.Init(filepath.Join(dir, "note-web.db"))
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	id, err := database.AddPDF(pdfPath, "book", util.CacheKey(pdfPath), 5, 10, 1)
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}

	cfg := config.PDFConfig{Dirs: []string{offlineDir}, ScanInterval: 300, RetentionDays: 0}
	if err := New(database, nil, cfg, config.DatabaseConfig{}, nil).Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	pdf, err := database.GetPDF(id)
	if err != nil {
		t.Fatalf("record was purged although its directory could not be scanned: %v", err)
	}
	if pdf.Deleted {
		t.Error("record was soft-deleted although its directory could not be scanned")
	}
}

// 发现数量骤降时只软删除、推迟物理清理，下一轮恢复正常后再清理
func TestScanDefersPurgeOnDrasticDrop(t *testing.T) {
	dir := t.TempDir()
	database, err := db.Init(filepath.Join(dir, "note-web.db"))
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	var paths []string
	for i := 0; i < 10; i++ {
		path := filepath.Join(dir, fmt.Sprintf("book-%02d.pdf", i))
		if err := os.WriteFile(path, []byte("%PDF-1.4 minimal"), 0o600); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.AddPDF(path, fmt.Sprintf("book-%02d", i), util.CacheKey(path), 5, info.Size(), info.ModTime().Unix()); err != nil {
			t.Fatalf("add pdf: %v", err)
		}
		paths = append(paths, path)
	}

	cfg := config.PDFConfig{Dirs: []string{dir}, ScanInterval: 300, RetentionDays: 0}
	scanner := New(database, nil, cfg, config.DatabaseConfig{}, nil)
	if err := scanner.Scan(); err != nil { // 第一轮建立 10 个文件的基线
		t.Fatalf("baseline scan: %v", err)
	}

	for _, path := range paths[:6] { // 移走 6 个，跌幅 60%
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := scanner.Scan(); err != nil {
		t.Fatalf("scan after drop: %v", err)
	}

	for _, path := range paths[:6] {
		pdf, err := database.GetPDFByPath(path)
		if err != nil {
			t.Fatalf("record %s was purged on a suspicious scan: %v", path, err)
		}
		if !pdf.Deleted {
			t.Errorf("record %s was not soft-deleted", path)
		}
	}

	// 下一轮扫描结果稳定，推迟的清理应当执行
	if err := scanner.Scan(); err != nil {
		t.Fatalf("third scan: %v", err)
	}
	if _, err := database.GetPDFByPath(paths[0]); err != sql.ErrNoRows {
		t.Errorf("deferred purge did not run on the next stable scan: %v", err)
	}
}

func TestScanImportsMatchingGoodNotesOutline(t *testing.T) {
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "book.pdf")
	if err := os.WriteFile(pdfPath, []byte("unchanged test PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	pdfInfo, err := os.Stat(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Init(filepath.Join(dir, "note-web.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	id, err := database.AddPDF(pdfPath, "book", util.CacheKey(pdfPath), 2, pdfInfo.Size(), pdfInfo.ModTime().Unix())
	if err != nil {
		t.Fatal(err)
	}

	goodNotesPath := filepath.Join(dir, "book.goodnotes")
	writeGoodNotesArchive(t, goodNotesPath)
	cfg := config.PDFConfig{Dirs: []string{dir}, ScanInterval: 300, RetentionDays: 30}
	s := New(database, nil, cfg, config.DatabaseConfig{}, nil)
	if err := s.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	marks, err := database.ListMarks(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 2 || marks[0].Title != "第一章" || marks[0].PageNum != 1 || marks[1].Title != "第一节" || marks[1].PageNum != 2 {
		t.Fatalf("imported marks = %+v", marks)
	}
	if marks[1].ParentID != marks[0].ID || marks[1].Level != 1 {
		t.Errorf("child hierarchy = parent %d level %d, want parent %d level 1", marks[1].ParentID, marks[1].Level, marks[0].ID)
	}
	storedPDF, err := database.GetPDF(id)
	if err != nil {
		t.Fatal(err)
	}
	if storedPDF.GoodNotesPath != goodNotesPath {
		t.Errorf("GoodNotes path = %q, want %q", storedPDF.GoodNotesPath, goodNotesPath)
	}
}

func writeGoodNotesArchive(t *testing.T, path string) {
	t.Helper()
	page1 := "00000000-0000-0000-0000-000000000001"
	page2 := "00000000-0000-0000-0000-000000000011"
	rootID := "00000000-0000-0000-0000-000000000101"
	childID := "00000000-0000-0000-0000-000000000102"
	notes := bytes.Join([][]byte{
		pbDelimited(pbMessage(pbString(1, incrementLastHex(page1)), pbString(2, "notes/page1"))),
		pbDelimited(pbMessage(pbString(1, incrementLastHex(page2)), pbString(2, "notes/page2"))),
	}, nil)
	events := bytes.Join([][]byte{
		pbDelimited(pbBytes(65, pbMessage(pbString(1, page1), pbString(2, rootID), pbBytes(5, pbString(1, "第一章"))))),
		pbDelimited(pbBytes(65, pbMessage(pbString(1, page2), pbString(2, childID), pbBytes(5, pbString(1, "第一节")), pbBytes(6, pbString(1, rootID))))),
	}, nil)

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, data := range map[string][]byte{"index.notes.pb": notes, "index.events.pb": events} {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func incrementLastHex(id string) string {
	last := id[len(id)-1]
	return id[:len(id)-1] + string(last+1)
}

func pbMessage(fields ...[]byte) []byte { return bytes.Join(fields, nil) }

func pbString(field int, value string) []byte { return pbBytes(field, []byte(value)) }

func pbBytes(field int, value []byte) []byte {
	result := binary.AppendUvarint(nil, uint64(field<<3|2))
	result = binary.AppendUvarint(result, uint64(len(value)))
	return append(result, value...)
}

func pbDelimited(value []byte) []byte {
	result := binary.AppendUvarint(nil, uint64(len(value)))
	return append(result, value...)
}

// 深度扫描开关：关闭后应立即清掉从内嵌 PDF 读出的目录，重新开启则恢复
func TestRefreshOutlineRespectsDeepScanPreference(t *testing.T) {
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "book.pdf")
	if err := os.WriteFile(pdfPath, []byte("%PDF-1.4 minimal"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(pdfPath)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Init(filepath.Join(dir, "note-web.db"))
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	id, err := database.AddPDF(pdfPath, "book", util.CacheKey(pdfPath), 2, info.Size(), info.ModTime().Unix())
	if err != nil {
		t.Fatalf("add pdf: %v", err)
	}
	writeImportedArchive(t, filepath.Join(dir, "book.goodnotes"))

	scanner := New(database, nil, config.PDFConfig{Dirs: []string{dir}, ScanInterval: 300, RetentionDays: 30}, config.DatabaseConfig{}, nil)
	if err := scanner.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}

	marks, err := database.ListMarks(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(marks) != 3 {
		t.Fatalf("深度扫描应导入 3 条目录，实际 %d 条: %+v", len(marks), marks)
	}
	pdf, err := database.GetPDF(id)
	if err != nil {
		t.Fatal(err)
	}
	if pdf.OutlineOrigin != "deep" {
		t.Errorf("outline origin = %q, want %q", pdf.OutlineOrigin, "deep")
	}

	// 关闭深度扫描 -> 目录立即消失
	if err := database.SetDeepScan(id, 0); err != nil {
		t.Fatal(err)
	}
	if err := scanner.RefreshOutline(id); err != nil {
		t.Fatalf("refresh with deep scan off: %v", err)
	}
	if marks, err := database.ListMarks(id); err != nil || len(marks) != 0 {
		t.Errorf("关闭深度扫描后仍残留目录: %+v (err=%v)", marks, err)
	}
	if pdf, err = database.GetPDF(id); err != nil || pdf.OutlineOrigin != "" {
		t.Errorf("关闭后 outline origin = %q (err=%v), 期望为空", pdf.OutlineOrigin, err)
	}

	// 重新开启 -> 目录恢复
	if err := database.SetDeepScan(id, 1); err != nil {
		t.Fatal(err)
	}
	if err := scanner.RefreshOutline(id); err != nil {
		t.Fatalf("refresh with deep scan on: %v", err)
	}
	if marks, err := database.ListMarks(id); err != nil || len(marks) != 3 {
		t.Errorf("重新开启后目录 = %+v (err=%v), 期望 3 条", marks, err)
	}
}

// writeImportedArchive 生成一个没有字段 65 目录、但内嵌带目录 PDF 的归档
func writeImportedArchive(t *testing.T, path string) {
	t.Helper()
	pageIDs := []string{
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
	}
	notes := make([]byte, 0)
	for _, pageID := range pageIDs {
		noteID := incrementLastHex(pageID)
		notes = append(notes, pbDelimited(pbMessage(pbString(1, noteID), pbString(2, "notes/"+noteID)))...)
	}
	events := append(pbDelimited(pbBytes(54, pbMessage(pbString(2, pageIDs[0]), pbBytes(4, pbString(1, "a"))))),
		pbDelimited(pbBytes(54, pbMessage(pbString(2, pageIDs[1]), pbBytes(4, pbString(1, "b")))))...)

	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, data := range map[string][]byte{
		"index.notes.pb":       notes,
		"index.events.pb":      events,
		"attachments/embedded": outlinePDFBytes(),
	} {
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// outlinePDFBytes 返回一个带两级目录的最小 PDF
func outlinePDFBytes() []byte {
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
