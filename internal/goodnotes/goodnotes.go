// Package goodnotes reads the outline data stored in a GoodNotes archive.
package goodnotes

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"note-web/internal/models"
)

const (
	maxEventsBytes = 64 << 20
	maxNotesBytes  = 8 << 20
	maxRecordBytes = 16 << 20
)

// ParseFile extracts the current outline entries from a .goodnotes archive.
// GoodNotes stores the data as a length-delimited protobuf stream. The schema
// is private, so the parser reads only the stable fields used by the outline
// records and ignores the rest.
func ParseFile(path string) ([]models.OutlineEntry, error) {
	archive, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("open goodnotes archive: %w", err)
	}
	defer archive.Close()

	notesData, err := readArchiveFile(archive, "index.notes.pb", maxNotesBytes)
	if err != nil {
		return nil, fmt.Errorf("read index.notes.pb: %w", err)
	}
	eventsData, err := readArchiveFile(archive, "index.events.pb", maxEventsBytes)
	if err != nil {
		return nil, fmt.Errorf("read index.events.pb: %w", err)
	}

	pages, err := parsePages(notesData)
	if err != nil {
		return nil, fmt.Errorf("parse index.notes.pb: %w", err)
	}
	if eventPages, pageErr := parseCurrentPages(eventsData); pageErr != nil {
		return nil, fmt.Errorf("parse GoodNotes page events: %w", pageErr)
	} else if len(eventPages) > 0 {
		pages = eventPages
	}
	records, err := parseOutlineRecords(eventsData)
	if err != nil {
		return nil, fmt.Errorf("parse index.events.pb: %w", err)
	}

	return buildEntries(records, pages), nil
}

func readArchiveFile(archive *zip.ReadCloser, name string, maxSize int64) ([]byte, error) {
	for _, file := range archive.File {
		if file.Name != name {
			continue
		}
		if file.FileInfo().Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%s is a symbolic link", name)
		}
		if file.UncompressedSize64 > uint64(maxSize) {
			return nil, fmt.Errorf("%s is too large", name)
		}
		reader, err := file.Open()
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxSize+1))
		closeErr := reader.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if int64(len(data)) > maxSize {
			return nil, fmt.Errorf("%s is too large", name)
		}
		return data, nil
	}
	return nil, os.ErrNotExist
}

type wireField struct {
	number int
	wire   int
	value  uint64
	bytes  []byte
}

func parseFields(data []byte) ([]wireField, error) {
	fields := make([]wireField, 0, 8)
	for pos := 0; pos < len(data); {
		key, next, err := readVarint(data, pos)
		if err != nil {
			return nil, err
		}
		pos = next
		number := int(key >> 3)
		wire := int(key & 7)
		if number <= 0 {
			return nil, fmt.Errorf("invalid field number %d", number)
		}

		field := wireField{number: number, wire: wire}
		switch wire {
		case 0:
			field.value, pos, err = readVarint(data, pos)
		case 1:
			if len(data)-pos < 8 {
				return nil, io.ErrUnexpectedEOF
			}
			field.bytes = data[pos : pos+8]
			pos += 8
		case 2:
			length, lengthEnd, lengthErr := readVarint(data, pos)
			if lengthErr != nil {
				return nil, lengthErr
			}
			pos = lengthEnd
			if length > uint64(len(data)-pos) || length > maxRecordBytes {
				return nil, fmt.Errorf("length-delimited field is too large")
			}
			field.bytes = data[pos : pos+int(length)]
			pos += int(length)
		case 5:
			if len(data)-pos < 4 {
				return nil, io.ErrUnexpectedEOF
			}
			field.bytes = data[pos : pos+4]
			pos += 4
		default:
			return nil, fmt.Errorf("unsupported protobuf wire type %d", wire)
		}
		if err != nil {
			return nil, err
		}
		fields = append(fields, field)
	}
	return fields, nil
}

func parseDelimited(data []byte, fn func([]byte) error) error {
	for pos := 0; pos < len(data); {
		length, next, err := readVarint(data, pos)
		if err != nil {
			return err
		}
		pos = next
		if length > maxRecordBytes || length > uint64(len(data)-pos) {
			return fmt.Errorf("delimited record is too large")
		}
		if err := fn(data[pos : pos+int(length)]); err != nil {
			return err
		}
		pos += int(length)
	}
	return nil
}

func readVarint(data []byte, pos int) (uint64, int, error) {
	var value uint64
	for shift := uint(0); shift < 64; shift += 7 {
		if pos >= len(data) {
			return 0, pos, io.ErrUnexpectedEOF
		}
		b := data[pos]
		pos++
		value |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return value, pos, nil
		}
	}
	return 0, pos, fmt.Errorf("varint overflows uint64")
}

func lastBytes(fields []wireField, number int) ([]byte, bool) {
	for i := len(fields) - 1; i >= 0; i-- {
		if fields[i].number == number && fields[i].wire == 2 {
			return fields[i].bytes, true
		}
	}
	return nil, false
}

func lastString(fields []wireField, number int) (string, bool) {
	value, ok := lastBytes(fields, number)
	if !ok {
		return "", false
	}
	return string(value), true
}

func parsePages(data []byte) (map[string]int, error) {
	pages := make(map[string]int)
	pageNumber := 0
	err := parseDelimited(data, func(record []byte) error {
		fields, err := parseFields(record)
		if err != nil {
			return err
		}
		id, ok := lastString(fields, 1)
		if !ok || !looksLikeUUID(id) {
			return nil
		}
		pageNumber++
		for _, alias := range pageAliases(id) {
			pages[alias] = pageNumber
		}
		return nil
	})
	return pages, err
}

type pageRecord struct {
	id       string
	position string
	ordinal  int
	deleted  bool
}

func parseCurrentPages(data []byte) (map[string]int, error) {
	records := make(map[string]*pageRecord)
	ordinal := 0
	err := parseDelimited(data, func(envelope []byte) error {
		fields, err := parseFields(envelope)
		if err != nil {
			return err
		}
		for _, field := range fields {
			if field.wire != 2 || (field.number != 54 && field.number != 56) {
				continue
			}
			payload, payloadErr := parseFields(field.bytes)
			if payloadErr != nil {
				return payloadErr
			}
			id, ok := lastString(payload, 2)
			if !ok || !looksLikeUUID(id) {
				continue
			}
			record := records[id]
			if record == nil {
				ordinal++
				record = &pageRecord{id: id, ordinal: ordinal}
				records[id] = record
			}
			if field.number == 56 {
				record.deleted = true
				continue
			}
			record.deleted = false
			if positionData, present := lastBytes(payload, 4); present {
				positionFields, nestedErr := parseFields(positionData)
				if nestedErr != nil {
					return nestedErr
				}
				if position, hasPosition := lastString(positionFields, 1); hasPosition {
					record.position = position
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	ordered := make([]*pageRecord, 0, len(records))
	for _, record := range records {
		if !record.deleted {
			ordered = append(ordered, record)
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].position != ordered[j].position {
			return ordered[i].position < ordered[j].position
		}
		return ordered[i].ordinal < ordered[j].ordinal
	})
	pages := make(map[string]int, len(ordered))
	for i, record := range ordered {
		pages[strings.ToUpper(record.id)] = i + 1
	}
	return pages, nil
}

type outlineRecord struct {
	id       string
	pageID   string
	title    string
	hasTitle bool
	parentID string
	first    int
}

func parseOutlineRecords(data []byte) (map[string]*outlineRecord, error) {
	records := make(map[string]*outlineRecord)
	ordinal := 0
	err := parseDelimited(data, func(envelope []byte) error {
		fields, err := parseFields(envelope)
		if err != nil {
			return err
		}
		for _, field := range fields {
			// Field 65 is the GoodNotes page-outline/bookmark record.
			if field.number != 65 || field.wire != 2 {
				continue
			}
			ordinal++
			if err := mergeOutlineRecord(records, field.bytes, ordinal); err != nil {
				return err
			}
		}
		return nil
	})
	return records, err
}

func mergeOutlineRecord(records map[string]*outlineRecord, data []byte, ordinal int) error {
	fields, err := parseFields(data)
	if err != nil {
		return err
	}
	id, ok := lastString(fields, 2)
	if !ok || id == "" {
		return nil
	}
	record := records[id]
	if record == nil {
		record = &outlineRecord{id: id, first: ordinal}
		records[id] = record
	}
	if pageID, ok := lastString(fields, 1); ok {
		record.pageID = pageID
	}
	if titleData, ok := lastBytes(fields, 5); ok {
		titleFields, nestedErr := parseFields(titleData)
		if nestedErr != nil {
			return nestedErr
		}
		if title, present := lastString(titleFields, 1); present {
			record.title = title
			record.hasTitle = true
		}
	}
	if parentData, ok := lastBytes(fields, 6); ok {
		parentFields, nestedErr := parseFields(parentData)
		if nestedErr != nil {
			return nestedErr
		}
		if parent, present := lastString(parentFields, 1); present {
			record.parentID = parent
		}
	}
	return nil
}

type outlineNode struct {
	record *outlineRecord
	page   int
}

func buildEntries(records map[string]*outlineRecord, pages map[string]int) []models.OutlineEntry {
	nodes := make(map[string]*outlineNode, len(records))
	for _, record := range records {
		if !record.hasTitle || cleanTitle(record.title) == "" {
			continue
		}
		page := pages[strings.ToUpper(strings.TrimSpace(record.pageID))]
		// The event log retains outline records for deleted notebook pages.
		if page == 0 {
			continue
		}
		nodes[record.id] = &outlineNode{record: record, page: page}
	}

	children := make(map[string][]*outlineNode)
	roots := make([]*outlineNode, 0, len(nodes))
	for _, node := range nodes {
		parent := nodes[node.record.parentID]
		if parent == nil || parent == node {
			roots = append(roots, node)
			continue
		}
		children[parent.record.id] = append(children[parent.record.id], node)
	}
	sortOutlineNodes(roots)
	for id := range children {
		sortOutlineNodes(children[id])
	}

	entries := make([]models.OutlineEntry, 0, len(nodes))
	visited := make(map[string]bool, len(nodes))
	visiting := make(map[string]bool, len(nodes))
	var appendTree func(*outlineNode, int)
	appendTree = func(node *outlineNode, level int) {
		if visited[node.record.id] || visiting[node.record.id] {
			return
		}
		if level > 20 {
			level = 20
		}
		visiting[node.record.id] = true
		visited[node.record.id] = true
		entries = append(entries, models.OutlineEntry{
			Level: level,
			Page:  node.page,
			Title: cleanTitle(node.record.title),
		})
		for _, child := range children[node.record.id] {
			appendTree(child, level+1)
		}
		delete(visiting, node.record.id)
	}
	for _, root := range roots {
		appendTree(root, 0)
	}

	// A malformed parent cycle has no root. Keep those entries accessible.
	remaining := make([]*outlineNode, 0)
	for _, node := range nodes {
		if !visited[node.record.id] {
			remaining = append(remaining, node)
		}
	}
	sortOutlineNodes(remaining)
	for _, node := range remaining {
		appendTree(node, 0)
	}
	return entries
}

func sortOutlineNodes(nodes []*outlineNode) {
	sort.SliceStable(nodes, func(i, j int) bool {
		if nodes[i].page != nodes[j].page {
			return nodes[i].page < nodes[j].page
		}
		return nodes[i].record.first < nodes[j].record.first
	})
}

func cleanTitle(title string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == 0 || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, title))
}

func looksLikeUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func pageAliases(id string) []string {
	id = strings.ToUpper(strings.TrimSpace(id))
	if !looksLikeUUID(id) {
		return nil
	}
	aliases := []string{id}
	if next := adjacentUUID(id, 1); next != "" {
		aliases = append(aliases, next)
	}
	if previous := adjacentUUID(id, -1); previous != "" {
		aliases = append(aliases, previous)
	}
	return aliases
}

// GoodNotes derives the note-blob UUID from the event page UUID by adding one
// to the 128-bit value. Include both neighbours because either form may appear
// in an index written by a different app version.
func adjacentUUID(id string, delta int) string {
	chars := []byte(id)
	carry := delta
	for i := len(chars) - 1; i >= 0 && carry != 0; i-- {
		if chars[i] == '-' {
			continue
		}
		value := strings.IndexByte("0123456789ABCDEF", chars[i])
		if value < 0 {
			return ""
		}
		value += carry
		switch {
		case value < 0:
			chars[i] = 'F'
			carry = -1
		case value > 15:
			chars[i] = '0'
			carry = 1
		default:
			chars[i] = "0123456789ABCDEF"[value]
			carry = 0
		}
	}
	if carry != 0 {
		return ""
	}
	return string(chars)
}
