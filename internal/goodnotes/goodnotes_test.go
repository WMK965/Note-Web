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

	entries, err := ParseFile(path)
	if err != nil {
		t.Fatalf("parse sample: %v", err)
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

	entries, err := ParseFile(path)
	if err != nil {
		t.Fatalf("parse synthetic archive: %v", err)
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
