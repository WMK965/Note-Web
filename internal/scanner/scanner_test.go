package scanner

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"note-web/internal/config"
	"note-web/internal/db"
	"note-web/internal/util"
)

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
	cfg := config.PDFConfig{Dirs: []string{dir}, ScanInterval: 300}
	s := New(database, nil, cfg, nil)
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
