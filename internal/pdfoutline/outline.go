package pdfoutline

import (
	"strings"
	"unicode/utf16"

	"note-web/internal/models"
)

// outlineParser 负责遍历书签树并把标题翻译成条目
type outlineParser struct {
	doc     *document
	pages   map[int]int // 页对象号 -> 1 起的页码
	entries []models.OutlineEntry
	seen    map[int]bool
}

// walk 先横向遍历 Next 链，再对每个条目递归处理子目录
func (p *outlineParser) walk(number, level int) {
	for number != 0 {
		if len(p.entries) >= MaxEntries || level > maxDepth || p.seen[number] {
			return
		}
		p.seen[number] = true
		item := p.doc.fetch(number)
		if item == nil {
			return
		}
		title := cleanTitle(decodeText(item["Title"]))
		if title != "" {
			page := 0
			if ref := p.doc.destinationPage(item); ref != 0 {
				page = p.pages[ref]
			}
			p.entries = append(p.entries, models.OutlineEntry{Level: level, Page: page, Title: title})
		}
		if child := p.doc.ref(item, "First"); child != 0 {
			p.walk(child, level+1)
		}
		number = p.doc.ref(item, "Next")
	}
}

// pageNumbers 按 /Pages 树的顺序给每个页对象编号
func (d *document) pageNumbers(catalog map[string]value) map[int]int {
	out := make(map[int]int)
	root := d.ref(catalog, "Pages")
	if root == 0 {
		return out
	}
	seen := make(map[int]bool)
	counter := 0
	var walk func(number, depth int)
	walk = func(number, depth int) {
		if number == 0 || depth > maxDepth || seen[number] {
			return
		}
		seen[number] = true
		node := d.fetch(number)
		if node == nil {
			return
		}
		kids, ok := node["Kids"]
		if !ok || kids.kind != kArray || len(kids.arr) == 0 {
			// 没有子节点就是叶子页
			counter++
			out[number] = counter
			return
		}
		for _, kid := range kids.arr {
			if kid.kind == kRef {
				walk(kid.ref, depth+1)
			}
		}
	}
	walk(root, 0)
	return out
}

// destinationPage 解析条目的跳转目标，返回目标页的对象号
func (d *document) destinationPage(item map[string]value) int {
	if dest, ok := item["Dest"]; ok {
		if ref := d.destinationRef(d.resolve(dest)); ref != 0 {
			return ref
		}
	}
	// /A 常常是间接引用（形如 /A 123 0 R），先解引用再读动作字典
	if raw, ok := item["A"]; ok {
		action := d.resolve(raw)
		if action.kind == kDict {
			if s := d.resolve(action.dict["S"]); s.kind == kName && s.name == "GoTo" {
				if dest, ok2 := action.dict["D"]; ok2 {
					return d.destinationRef(d.resolve(dest))
				}
			}
		}
	}
	return 0
}

// resolve 把间接引用换成它指向的真实值；不是引用时原样返回
func (d *document) resolve(v value) value {
	if v.kind != kRef {
		return v
	}
	sp, ok := d.objs[v.ref]
	if !ok {
		return value{}
	}
	p := &parser{buf: sp.data[sp.start:sp.end]}
	p.skipSpace()
	parsed, err := p.parseValue(0)
	if err != nil {
		return value{}
	}
	return parsed
}

func (d *document) destinationRef(dest value) int {
	dest = d.resolve(dest)
	switch dest.kind {
	case kArray:
		if len(dest.arr) == 0 {
			return 0
		}
		switch first := dest.arr[0]; first.kind {
		case kRef:
			return first.ref
		case kDict:
			// 有些实现把目标写成 << /D [...] >>
			if inner, ok := first.dict["D"]; ok {
				return d.destinationRef(inner)
			}
		}
		return 0
	case kRef:
		return dest.ref
	case kName:
		return d.namedDestination(dest.name)
	case kDict:
		if inner, ok := dest.dict["D"]; ok {
			return d.destinationRef(inner)
		}
	}
	return 0
}

// namedDestination 处理 /Dest /Name 与 /A << /D (name) >> 这类命名目标
func (d *document) namedDestination(name string) int {
	if name == "" {
		return 0
	}
	if dests, ok := d.catalogDict["Dests"]; ok && dests.kind == kDict {
		if dest, ok2 := dests.dict[name]; ok2 {
			return d.destinationRef(d.resolve(dest))
		}
	}
	if names := d.resolve(d.catalogDict["Names"]); names.kind == kDict {
		if tree, ok2 := names.dict["Dests"]; ok2 {
			return d.nameTreeLookup(d.resolve(tree), name, 0)
		}
	}
	return 0
}

func (d *document) nameTreeLookup(node value, name string, depth int) int {
	if depth > 16 {
		return 0
	}
	if node.kind == kRef {
		dict := d.fetch(node.ref)
		if dict == nil {
			return 0
		}
		node = value{kind: kDict, dict: dict}
	}
	if node.kind != kDict {
		return 0
	}
	if pairs := d.resolve(node.dict["Names"]); pairs.kind == kArray {
		for i := 0; i+1 < len(pairs.arr); i += 2 {
			if decodeText(pairs.arr[i]) == name {
				return d.destinationRef(d.resolve(pairs.arr[i+1]))
			}
		}
	}
	if kids := d.resolve(node.dict["Kids"]); kids.kind == kArray {
		for _, kid := range kids.arr {
			if page := d.nameTreeLookup(kid, name, depth+1); page != 0 {
				return page
			}
		}
	}
	return 0
}

// decodeText 解码 PDF 字符串：带 BOM 的按 UTF-16BE，其余按单字节编码处理
func decodeText(v value) string {
	if v.kind != kString {
		return ""
	}
	raw := v.str
	if len(raw) >= 2 && raw[0] == 0xFE && raw[1] == 0xFF {
		units := make([]uint16, 0, (len(raw)-2)/2)
		for i := 2; i+1 < len(raw); i += 2 {
			units = append(units, uint16(raw[i])<<8|uint16(raw[i+1]))
		}
		return string(utf16.Decode(units))
	}
	runes := make([]rune, 0, len(raw))
	for _, c := range raw {
		runes = append(runes, rune(c))
	}
	return string(runes)
}

// cleanTitle 去掉控制字符并修剪空白
func cleanTitle(title string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r == 0 || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, title))
}
