package goodnotes

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestParseKnownSample(t *testing.T) {
	path := filepath.Join("..", "..", "tmp", "test.goodnotes")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("sample archive is not available: %v", err)
	}

	outline, err := ParseFile(path)
	if err != nil {
		t.Fatalf("parse sample: %v", err)
	}
	entries := outline.Entries
	if outline.Source != SourceGoodNotes {
		t.Errorf("source = %q, want %q", outline.Source, SourceGoodNotes)
	}
	wantTitles := []string{"第一章", "第一章第一节", "第二章"}
	wantPages := []int{2, 3, 4}
	wantLevels := []int{0, 1, 0}
	if len(entries) != len(wantTitles) {
		t.Fatalf("entry count = %d, want %d", len(entries), len(wantTitles))
	}
	for i, entry := range entries {
		if entry.Title != wantTitles[i] || entry.Page != wantPages[i] || entry.Level != wantLevels[i] {
			t.Errorf("entry %d = %+v, want title=%q page=%d level=%d", i, entry, wantTitles[i], wantPages[i], wantLevels[i])
		}
	}
}

func TestParseSyntheticArchive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outline.goodnotes")
	pageEvents := []string{
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-00000000000F",
		"00000000-0000-0000-0000-000000000020",
		"00000000-0000-0000-0000-000000000030",
	}
	notes := make([]byte, 0)
	for _, pageID := range pageEvents {
		noteID := adjacentUUID(pageID, 1)
		note := protobufMessage(protobufString(1, noteID), protobufString(2, "notes/"+noteID))
		notes = append(notes, protobufDelimited(note)...)
	}
	firstID := "00000000-0000-0000-0000-000000000101"
	childID := "00000000-0000-0000-0000-000000000102"
	secondID := "00000000-0000-0000-0000-000000000103"
	outlineEvents := [][]byte{
		outlineEnvelope(pageEvents[3], secondID, "第二章", ""),
		outlineEnvelope(pageEvents[2], childID, "第一章第一节", firstID),
		outlineEnvelope(pageEvents[1], firstID, "第一章", ""),
	}
	events := bytes.Join(outlineEvents, nil)
	writeArchive(t, path, map[string][]byte{
		"index.notes.pb":  notes,
		"index.events.pb": events,
	})

	outline, err := ParseFile(path)
	if err != nil {
		t.Fatalf("parse synthetic archive: %v", err)
	}
	entries := outline.Entries
	if outline.Source != SourceGoodNotes {
		t.Errorf("source = %q, want %q", outline.Source, SourceGoodNotes)
	}
	wantTitles := []string{"第一章", "第一章第一节", "第二章"}
	wantPages := []int{2, 3, 4}
	wantLevels := []int{0, 1, 0}
	if len(entries) != len(wantTitles) {
		t.Fatalf("entry count = %d, want %d", len(entries), len(wantTitles))
	}
	for i, entry := range entries {
		if entry.Title != wantTitles[i] || entry.Page != wantPages[i] || entry.Level != wantLevels[i] {
			t.Errorf("entry %d = %+v, want title=%q page=%d level=%d", i, entry, wantTitles[i], wantPages[i], wantLevels[i])
		}
	}
}

func TestParseCurrentPagesAppliesSortAndDeletion(t *testing.T) {
	first := "00000000-0000-0000-0000-000000000001"
	deleted := "00000000-0000-0000-0000-000000000002"
	last := "00000000-0000-0000-0000-000000000003"
	events := bytes.Join([][]byte{
		pageEnvelope(54, last, "z"),
		pageEnvelope(54, deleted, "m"),
		pageEnvelope(54, first, "a"),
		pageEnvelope(56, deleted, ""),
	}, nil)

	pages, err := parseCurrentPages(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 {
		t.Fatalf("page count = %d, want 2", len(pages))
	}
	if pages[first] != 1 || pages[last] != 2 {
		t.Errorf("page order = first:%d last:%d, want 1 and 2", pages[first], pages[last])
	}
	if _, ok := pages[deleted]; ok {
		t.Fatal("deleted page remains in current page map")
	}
}

func TestDeletedPageOutlineDoesNotMatchAdjacentPage(t *testing.T) {
	deleted := "00000000-0000-0000-0000-000000000001"
	current := "00000000-0000-0000-0000-000000000002"
	pageEvents := bytes.Join([][]byte{
		pageEnvelope(54, deleted, "a"),
		pageEnvelope(54, current, "b"),
		pageEnvelope(56, deleted, ""),
	}, nil)
	outline := outlineEnvelope(deleted, "00000000-0000-0000-0000-000000000101", "已删除目录", "")
	records, err := parseOutlineRecords(append(pageEvents, outline...))
	if err != nil {
		t.Fatal(err)
	}
	pages, err := parseCurrentPages(pageEvents)
	if err != nil {
		t.Fatal(err)
	}
	if entries := buildEntries(records, pages); len(entries) != 0 {
		t.Fatalf("deleted page outline was retained: %+v", entries)
	}
}

func TestParseFileRejectsOversizedIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized.goodnotes")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	header := &zip.FileHeader{Name: "index.notes.pb", Method: zip.Store, UncompressedSize64: maxNotesBytes + 1}
	header.SetMode(0o600)
	writer, err := archive.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(make([]byte, maxNotesBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := ParseFile(path); err == nil {
		t.Fatal("ParseFile accepted an oversized index")
	}
}

// 归档没有 GoodNotes 书签、但内嵌 PDF 自带目录时，应当回退读取那份 PDF 的目录
func TestParseArchiveFallsBackToEmbeddedPDF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "imported.goodnotes")
	pageEvents := []string{
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
	}
	notes := make([]byte, 0)
	for _, pageID := range pageEvents {
		noteID := adjacentUUID(pageID, 1)
		note := protobufMessage(protobufString(1, noteID), protobufString(2, "notes/"+noteID))
		notes = append(notes, protobufDelimited(note)...)
	}
	// 只有页面事件，没有任何字段 65 的目录记录
	events := pageEnvelope(54, pageEvents[0], "a")
	events = append(events, pageEnvelope(54, pageEvents[1], "b")...)
	writeArchive(t, path, map[string][]byte{
		"index.notes.pb":       notes,
		"index.events.pb":      events,
		"attachments/embedded": syntheticOutlinePDF(),
		"attachments/notpdf":   []byte("PNG image data"),
	})

	outline, err := ParseFile(path)
	if err != nil {
		t.Fatalf("parse archive: %v", err)
	}
	if outline.Source != SourceDeep {
		t.Fatalf("source = %q, want %q", outline.Source, SourceDeep)
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
	if len(outline.Entries) != len(want) {
		t.Fatalf("entry count = %d, want %d: %+v", len(outline.Entries), len(want), outline.Entries)
	}
	for i, w := range want {
		got := outline.Entries[i]
		if got.Level != w.level || got.Page != w.page || got.Title != w.title {
			t.Errorf("entry %d = %+v, want level=%d page=%d title=%q", i, got, w.level, w.page, w.title)
		}
	}
}

// 内嵌附件里没有 PDF 时，应视为该笔记本没有目录
func TestParseArchiveWithoutPDFAttachment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plain.goodnotes")
	pageID := "00000000-0000-0000-0000-000000000001"
	noteID := adjacentUUID(pageID, 1)
	notes := protobufDelimited(protobufMessage(protobufString(1, noteID), protobufString(2, "notes/"+noteID)))
	writeArchive(t, path, map[string][]byte{
		"index.notes.pb":  notes,
		"index.events.pb": pageEnvelope(54, pageID, "a"),
		"attachments/x":   []byte("PNG data"),
	})

	outline, err := ParseFile(path)
	if err != nil {
		t.Fatalf("parse archive: %v", err)
	}
	if len(outline.Entries) != 0 || outline.Source != "" {
		t.Errorf("outline = %+v, want empty", outline)
	}
}

// 关闭深度扫描后，即使内嵌 PDF 自带目录也不应读取
func TestParseFileWithDeepScanDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deep-off.goodnotes")
	pageID := "00000000-0000-0000-0000-000000000001"
	noteID := adjacentUUID(pageID, 1)
	notes := protobufDelimited(protobufMessage(protobufString(1, noteID), protobufString(2, "notes/"+noteID)))
	writeArchive(t, path, map[string][]byte{
		"index.notes.pb":       notes,
		"index.events.pb":      pageEnvelope(54, pageID, "a"),
		"attachments/embedded": syntheticOutlinePDF(),
	})

	outline, err := ParseFileWithOptions(path, Options{DeepScan: false})
	if err != nil {
		t.Fatalf("parse archive: %v", err)
	}
	if len(outline.Entries) != 0 || outline.Source != "" {
		t.Errorf("deep scan disabled but got %+v", outline)
	}

	// 默认（自动）模式下应当读到内嵌 PDF 的目录
	enabled, err := ParseFile(path)
	if err != nil {
		t.Fatalf("parse archive with default options: %v", err)
	}
	if enabled.Source != SourceDeep || len(enabled.Entries) != 3 {
		t.Errorf("default options = %+v, want 3 deep-scan entries", enabled)
	}
}

// syntheticOutlinePDF 构造一个带两级目录的最小 PDF
func syntheticOutlinePDF() []byte {
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

func TestPageAliasesCarriesAcrossUUIDBytes(t *testing.T) {
	aliases := pageAliases("00000000-0000-0000-0000-0000000000FF")
	want := "00000000-0000-0000-0000-000000000100"
	if len(aliases) < 2 || aliases[1] != want {
		t.Fatalf("pageAliases = %v, want next UUID %q", aliases, want)
	}
}

func TestParseOutlineRecordsUsesLatestState(t *testing.T) {
	pageID := "00000000-0000-0000-0000-000000000010"
	recordID := "00000000-0000-0000-0000-000000000020"
	created := protobufMessage(
		protobufString(1, pageID),
		protobufString(2, recordID),
		protobufBytes(5, protobufString(1, "旧标题")),
	)
	updated := protobufMessage(
		protobufString(1, pageID),
		protobufString(2, recordID),
		protobufBytes(5, protobufString(1, "新标题")),
	)
	stream := append(protobufDelimited(protobufBytes(65, created)), protobufDelimited(protobufBytes(65, updated))...)

	records, err := parseOutlineRecords(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := records[recordID].title; got != "新标题" {
		t.Fatalf("latest title = %q, want %q", got, "新标题")
	}
}

func protobufMessage(fields ...[]byte) []byte {
	return bytes.Join(fields, nil)
}

func protobufString(field int, value string) []byte {
	return protobufBytes(field, []byte(value))
}

func protobufBytes(field int, value []byte) []byte {
	result := binary.AppendUvarint(nil, uint64(field<<3|2))
	result = binary.AppendUvarint(result, uint64(len(value)))
	return append(result, value...)
}

func protobufDelimited(value []byte) []byte {
	result := binary.AppendUvarint(nil, uint64(len(value)))
	return append(result, value...)
}

func outlineEnvelope(pageID, recordID, title, parentID string) []byte {
	fields := [][]byte{
		protobufString(1, pageID),
		protobufString(2, recordID),
		protobufBytes(5, protobufString(1, title)),
	}
	if parentID != "" {
		fields = append(fields, protobufBytes(6, protobufString(1, parentID)))
	}
	return protobufDelimited(protobufBytes(65, protobufMessage(fields...)))
}

func pageEnvelope(field int, pageID, position string) []byte {
	fields := [][]byte{protobufString(2, pageID)}
	if position != "" {
		fields = append(fields, protobufBytes(4, protobufString(1, position)))
	}
	return protobufDelimited(protobufBytes(field, protobufMessage(fields...)))
}

func writeArchive(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, data := range files {
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
