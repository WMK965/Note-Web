// Package pdfoutline 从 PDF 字节流里读出书签目录（/Outlines）。
//
// 存在的意义：GoodNotes 只会把用户在应用内手工创建的目录写进自己的事件流
// （见 internal/goodnotes 的字段 65）。导入的 PDF 如果自带目录，GoodNotes
// 只是把原文件原样存成附件，目录仍留在那份 PDF 里。这里用纯 Go 把那份
// 目录读出来，不依赖 mutool 等外部命令。
//
// 解析器只做“够用”的工作：按对象头切片定位对象、支持 Flate 压缩对象流、
// 从 Catalog 找 /Outlines 与 /Pages，然后按 First/Next 递归收集标题，
// 并把 /Dest 解析成页码。遇到异常输入时返回错误或空结果，不会 panic。
package pdfoutline

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"note-web/internal/models"
)

const (
	// MaxFileSize 是允许解析的 PDF 体积上限，超过则放弃，避免把超大附件读进内存
	MaxFileSize = 256 << 20
	// MaxEntries 限制目录条目数，防止异常文件造成内存膨胀
	MaxEntries = 5000
	maxDepth   = 48
)

// Parse 解析 PDF 自带目录，按阅读顺序返回带层级的条目。
// 没有目录时返回空切片和 nil 错误。
func Parse(data []byte) ([]models.OutlineEntry, error) {
	if len(data) == 0 {
		return nil, errors.New("empty pdf")
	}
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("pdf too large: %d bytes", len(data))
	}
	if !bytes.HasPrefix(data, []byte("%PDF-")) {
		return nil, errors.New("not a pdf")
	}

	doc := newDocument(data)
	catalog := doc.catalog()
	if catalog == nil {
		return nil, errors.New("pdf catalog not found")
	}
	doc.catalogDict = catalog
	root := doc.fetch(doc.ref(catalog, "Outlines"))
	if root == nil {
		return nil, nil
	}
	first := doc.ref(root, "First")
	if first == 0 {
		return nil, nil
	}

	pages := doc.pageNumbers(catalog)
	parser := &outlineParser{doc: doc, pages: pages, seen: map[int]bool{}}
	parser.walk(first, 0)
	return parser.entries, nil
}

// ---------------------------------------------------------------------------
// PDF 值模型
// ---------------------------------------------------------------------------

type kind int

const (
	kNull kind = iota
	kBool
	kNumber
	kName
	kString
	kArray
	kDict
	kRef
)

type value struct {
	kind kind
	num  float64
	ref  int
	name string
	str  []byte
	arr  []value
	dict map[string]value
}

type span struct {
	data  []byte
	start int
	end   int
}

type document struct {
	// root 是整个 PDF 文件；对象流解压出来的对象使用各自的 data
	raw         []byte
	objs        map[int]span
	cache       map[int]map[string]value
	catalogDict map[string]value
}

var objectHeader = regexp.MustCompile(`(\d+)[ \t\r\n]+(\d+)[ \t\r\n]+obj\b`)

func newDocument(data []byte) *document {
	doc := &document{raw: data, objs: make(map[int]span), cache: make(map[int]map[string]value)}

	matches := objectHeader.FindAllSubmatchIndex(data, -1)
	positions := make([]int, 0, len(matches))
	numbers := make([]int, 0, len(matches))
	for _, m := range matches {
		number, err := strconv.Atoi(string(data[m[2]:m[3]]))
		if err != nil {
			continue
		}
		positions = append(positions, m[1])
		numbers = append(numbers, number)
	}
	// 用“下一个对象头”作为当前对象的结束位置，避免二进制流里出现 endobj 造成误判
	for i, start := range positions {
		end := len(data)
		if i+1 < len(positions) {
			end = positions[i+1]
		}
		doc.objs[numbers[i]] = span{data: data, start: start, end: end}
	}
	doc.indexObjectStreams()
	return doc
}

// indexObjectStreams 展开 /ObjStm 压缩对象流，让压缩存储的对象也能被读取。
func (d *document) indexObjectStreams() {
	type candidate struct {
		number int
		body   []byte
	}
	var streams []candidate
	for number, sp := range d.objs {
		body := sp.data[sp.start:sp.end]
		if bytes.Contains(body, []byte("/ObjStm")) {
			streams = append(streams, candidate{number: number, body: body})
		}
	}
	for _, c := range streams {
		d.expandObjectStream(c.body)
	}
}

func (d *document) expandObjectStream(body []byte) {
	dict, ok := parseDictionary(body)
	if !ok {
		return
	}
	countValue, hasCount := dict["N"]
	firstValue, hasFirst := dict["First"]
	if !hasCount || !hasFirst || countValue.kind != kNumber || firstValue.kind != kNumber {
		return
	}
	count := int(countValue.num)
	first := int(firstValue.num)
	if count <= 0 || count > 100000 || first <= 0 || first > len(body) {
		return
	}
	payload, ok := d.streamData(body, dict)
	if !ok || first > len(payload) {
		return
	}

	// 对象流开头是 count 组「对象号 偏移」整数
	header := payload[:first]
	fields := strings.Fields(string(header))
	if len(fields) < count*2 {
		return
	}
	for i := 0; i < count; i++ {
		number, err1 := strconv.Atoi(fields[2*i])
		offset, err2 := strconv.Atoi(fields[2*i+1])
		if err1 != nil || err2 != nil {
			return
		}
		start := first + offset
		if start < 0 || start > len(payload) {
			continue
		}
		end := len(payload)
		if i+1 < count {
			if next, err := strconv.Atoi(fields[2*i+3]); err == nil {
				if candidate := first + next; candidate > start && candidate <= len(payload) {
					end = candidate
				}
			}
		}
		if _, exists := d.objs[number]; exists {
			continue
		}
		d.objs[number] = span{data: payload, start: start, end: end}
	}
}

// streamData 取出对象里的流数据并按需解压。
func (d *document) streamData(body []byte, dict map[string]value) ([]byte, bool) {
	index := bytes.Index(body, []byte("stream"))
	if index < 0 {
		return nil, false
	}
	start := index + len("stream")
	if start < len(body) && body[start] == '\r' {
		start++
	}
	if start < len(body) && body[start] == '\n' {
		start++
	}
	end := bytes.Index(body[start:], []byte("endstream"))
	if end < 0 {
		return nil, false
	}
	raw := body[start : start+end]

	filter, hasFilter := dict["Filter"]
	if !hasFilter {
		return raw, true
	}
	if !usesFlate(filter) {
		return nil, false
	}
	if inflated, err := inflate(raw); err == nil {
		return inflated, true
	}
	return nil, false
}

func usesFlate(filter value) bool {
	switch filter.kind {
	case kName:
		return filter.name == "FlateDecode" || filter.name == "Fl"
	case kArray:
		for _, f := range filter.arr {
			if f.kind == kName && (f.name == "FlateDecode" || f.name == "Fl") {
				return true
			}
		}
	}
	return false
}

func inflate(raw []byte) ([]byte, error) {
	if reader, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		data, readErr := io.ReadAll(io.LimitReader(reader, MaxFileSize))
		reader.Close()
		if readErr == nil && len(data) > 0 {
			return data, nil
		}
	}
	reader := flate.NewReader(bytes.NewReader(raw))
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, MaxFileSize))
}

// fetch 返回某个对象的字典，失败返回 nil。
func (d *document) fetch(number int) map[string]value {
	if number <= 0 {
		return nil
	}
	if cached, ok := d.cache[number]; ok {
		return cached
	}
	sp, ok := d.objs[number]
	if !ok {
		return nil
	}
	dict, ok := parseDictionary(sp.data[sp.start:sp.end])
	if !ok {
		return nil
	}
	d.cache[number] = dict
	return dict
}

// ref 读取字典里的对象引用，没有或不是引用时返回 0。
func (d *document) ref(dict map[string]value, key string) int {
	if dict == nil {
		return 0
	}
	if v, ok := dict[key]; ok && v.kind == kRef {
		return v.ref
	}
	return 0
}

// catalog 定位文档 Catalog：先找 trailer/xref 里的 /Root，再退回扫描 /Type/Catalog。
func (d *document) catalog() map[string]value {
	if match := regexp.MustCompile(`/Root[ \t\r\n]+(\d+)[ \t\r\n]+\d+[ \t\r\n]+R`).FindSubmatch(d.raw); match != nil {
		if number, err := strconv.Atoi(string(match[1])); err == nil {
			if dict := d.fetch(number); dict != nil {
				return dict
			}
		}
	}
	numbers := make([]int, 0, len(d.objs))
	for number := range d.objs {
		numbers = append(numbers, number)
	}
	// 对象号越大越可能是新写入的 Catalog
	sortIntsDesc(numbers)
	for _, number := range numbers {
		sp := d.objs[number]
		if !bytes.Contains(sp.data[sp.start:sp.end], []byte("/Catalog")) {
			continue
		}
		if dict := d.fetch(number); dict != nil {
			return dict
		}
	}
	return nil
}

func sortIntsDesc(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] > values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// parseDictionary 从对象体里解析出顶层字典。
func parseDictionary(body []byte) (map[string]value, bool) {
	p := &parser{buf: body}
	p.skipSpace()
	if p.pos >= len(p.buf) || p.buf[p.pos] != '<' {
		return nil, false
	}
	v, err := p.parseValue(0)
	if err != nil || v.kind != kDict {
		return nil, false
	}
	return v.dict, true
}

// ---------------------------------------------------------------------------
// 迷你 PDF 语法解析
// ---------------------------------------------------------------------------

type parser struct {
	buf []byte
	pos int
}

func (p *parser) skipSpace() {
	for p.pos < len(p.buf) {
		switch p.buf[p.pos] {
		case ' ', '\t', '\r', '\n', '\f', 0:
			p.pos++
		case '%':
			for p.pos < len(p.buf) && p.buf[p.pos] != '\n' && p.buf[p.pos] != '\r' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *parser) parseValue(depth int) (value, error) {
	if depth > 64 {
		return value{}, errors.New("nesting too deep")
	}
	p.skipSpace()
	if p.pos >= len(p.buf) {
		return value{}, io.ErrUnexpectedEOF
	}
	switch c := p.buf[p.pos]; {
	case c == '<':
		if p.pos+1 < len(p.buf) && p.buf[p.pos+1] == '<' {
			return p.parseDict(depth)
		}
		return p.parseHexString()
	case c == '[':
		return p.parseArray(depth)
	case c == '/':
		return p.parseName()
	case c == '(':
		return p.parseLiteralString()
	case c == 't' || c == 'f' || c == 'n':
		return p.parseKeyword()
	default:
		return p.parseNumberOrRef()
	}
}

func (p *parser) parseDict(depth int) (value, error) {
	p.pos += 2 // <<
	dict := make(map[string]value)
	for {
		p.skipSpace()
		if p.pos >= len(p.buf) {
			return value{}, io.ErrUnexpectedEOF
		}
		if p.buf[p.pos] == '>' {
			if p.pos+1 < len(p.buf) && p.buf[p.pos+1] == '>' {
				p.pos += 2
				return value{kind: kDict, dict: dict}, nil
			}
			return value{}, errors.New("unexpected '>' in dictionary")
		}
		key, err := p.parseName()
		if err != nil {
			return value{}, err
		}
		val, err := p.parseValue(depth + 1)
		if err != nil {
			return value{}, err
		}
		if _, exists := dict[key.name]; !exists {
			dict[key.name] = val
		}
	}
}

func (p *parser) parseArray(depth int) (value, error) {
	p.pos++ // [
	items := make([]value, 0, 4)
	for {
		p.skipSpace()
		if p.pos >= len(p.buf) {
			return value{}, io.ErrUnexpectedEOF
		}
		if p.buf[p.pos] == ']' {
			p.pos++
			return value{kind: kArray, arr: items}, nil
		}
		item, err := p.parseValue(depth + 1)
		if err != nil {
			return value{}, err
		}
		items = append(items, item)
	}
}

func (p *parser) parseName() (value, error) {
	if p.pos >= len(p.buf) || p.buf[p.pos] != '/' {
		return value{}, errors.New("expected name")
	}
	p.pos++
	start := p.pos
	for p.pos < len(p.buf) && !isDelimiter(p.buf[p.pos]) {
		p.pos++
	}
	name := string(p.buf[start:p.pos])
	// 处理 #xx 形式的转义字符
	if strings.Contains(name, "#") {
		name = unescapeName(name)
	}
	return value{kind: kName, name: name}, nil
}

func (p *parser) parseHexString() (value, error) {
	p.pos++ // <
	start := p.pos
	for p.pos < len(p.buf) && p.buf[p.pos] != '>' {
		p.pos++
	}
	raw := string(p.buf[start:p.pos])
	if p.pos < len(p.buf) {
		p.pos++
	}
	digits := make([]byte, 0, len(raw)/2+1)
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if !isHexDigit(c) {
			continue
		}
		digits = append(digits, c)
	}
	if len(digits)%2 == 1 {
		digits = append(digits, '0')
	}
	out := make([]byte, 0, len(digits)/2)
	for i := 0; i+1 < len(digits); i += 2 {
		out = append(out, hexPair(digits[i], digits[i+1]))
	}
	return value{kind: kString, str: out}, nil
}

func (p *parser) parseLiteralString() (value, error) {
	p.pos++ // (
	out := make([]byte, 0, 16)
	depth := 1
	for p.pos < len(p.buf) {
		c := p.buf[p.pos]
		p.pos++
		switch c {
		case '\\':
			if p.pos >= len(p.buf) {
				return value{kind: kString, str: out}, nil
			}
			escaped := p.buf[p.pos]
			p.pos++
			switch escaped {
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case '\r':
				if p.pos < len(p.buf) && p.buf[p.pos] == '\n' {
					p.pos++
				}
			case '\n':
				// 行继续，忽略
			case '0', '1', '2', '3', '4', '5', '6', '7':
				digits := []byte{escaped}
				for len(digits) < 3 && p.pos < len(p.buf) && p.buf[p.pos] >= '0' && p.buf[p.pos] <= '7' {
					digits = append(digits, p.buf[p.pos])
					p.pos++
				}
				if parsed, err := strconv.ParseUint(string(digits), 8, 16); err == nil {
					out = append(out, byte(parsed))
				}
			default:
				out = append(out, escaped)
			}
		case '(':
			depth++
			out = append(out, c)
		case ')':
			depth--
			if depth == 0 {
				return value{kind: kString, str: out}, nil
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return value{kind: kString, str: out}, nil
}

func (p *parser) parseKeyword() (value, error) {
	start := p.pos
	for p.pos < len(p.buf) && !isDelimiter(p.buf[p.pos]) {
		p.pos++
	}
	switch string(p.buf[start:p.pos]) {
	case "true":
		return value{kind: kBool, num: 1}, nil
	case "false":
		return value{kind: kBool}, nil
	case "null":
		return value{kind: kNull}, nil
	default:
		return value{}, errors.New("unknown keyword")
	}
}

func (p *parser) parseNumberOrRef() (value, error) {
	first, err := p.parseNumber()
	if err != nil {
		return value{}, err
	}
	save := p.pos
	p.skipSpace()
	if p.pos < len(p.buf) && (p.buf[p.pos] >= '0' && p.buf[p.pos] <= '9') {
		generation, err := p.parseNumber()
		if err == nil {
			p.skipSpace()
			if p.pos < len(p.buf) && p.buf[p.pos] == 'R' {
				p.pos++
				return value{kind: kRef, ref: int(first.num)}, nil
			}
		}
		_ = generation
	}
	p.pos = save
	return value{kind: kNumber, num: first.num}, nil
}

func (p *parser) parseNumber() (value, error) {
	p.skipSpace()
	start := p.pos
	for p.pos < len(p.buf) {
		c := p.buf[p.pos]
		if (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.' {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return value{}, errors.New("expected number")
	}
	number, err := strconv.ParseFloat(string(p.buf[start:p.pos]), 64)
	if err != nil {
		return value{}, err
	}
	return value{kind: kNumber, num: number}, nil
}

func isDelimiter(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', '\f', 0, '/', '<', '>', '[', ']', '(', ')', '%':
		return true
	}
	return false
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func hexPair(high, low byte) byte {
	value := func(c byte) byte {
		switch {
		case c >= '0' && c <= '9':
			return c - '0'
		case c >= 'a' && c <= 'f':
			return c - 'a' + 10
		default:
			return c - 'A' + 10
		}
	}
	return value(high)<<4 | value(low)
}

func unescapeName(name string) string {
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '#' && i+2 < len(name) && isHexDigit(name[i+1]) && isHexDigit(name[i+2]) {
			out.WriteByte(hexPair(name[i+1], name[i+2]))
			i += 2
			continue
		}
		out.WriteByte(name[i])
	}
	return out.String()
}
