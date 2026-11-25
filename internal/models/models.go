// 数据模型包
// 定义数据库实体和API响应的数据结构
package models

import (
	"strings"
	"time"
)

// 目录来源。notes 是 A.goodnotes 自建目录，source_pdf 是归档内嵌的来源
// PDF 目录，pdf 是 A.pdf 本体目录。
const (
	OutlineSourceNotes     = "notes"
	OutlineSourceSourcePDF = "source_pdf"
	OutlineSourcePDF       = "pdf"

	OutlineSourcesAuto = "auto"
	OutlineSourcesNone = "none"
)

var outlineSourceOrder = []string{
	OutlineSourceNotes,
	OutlineSourceSourcePDF,
	OutlineSourcePDF,
}

// NormalizeOutlineSources 去除未知值和重复值，并按固定顺序排列。
func NormalizeOutlineSources(sources []string) []string {
	seen := make(map[string]bool, len(sources))
	for _, source := range sources {
		switch source {
		case OutlineSourceNotes, OutlineSourceSourcePDF, OutlineSourcePDF:
			seen[source] = true
		}
	}
	normalized := make([]string, 0, len(seen))
	for _, source := range outlineSourceOrder {
		if seen[source] {
			normalized = append(normalized, source)
		}
	}
	return normalized
}

// ParseOutlineSources 解析数据库中的来源集合。auto 表示尚未确定，none
// 表示用户明确关闭全部来源。
func ParseOutlineSources(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == OutlineSourcesAuto || raw == OutlineSourcesNone {
		return nil
	}
	return NormalizeOutlineSources(strings.Split(raw, ","))
}

// EncodeOutlineSources 将来源集合编码为数据库格式。
func EncodeOutlineSources(sources []string) string {
	normalized := NormalizeOutlineSources(sources)
	if len(normalized) == 0 {
		return OutlineSourcesNone
	}
	return strings.Join(normalized, ",")
}

// PDF 表示数据库中的一个PDF文件记录
type PDF struct {
	ID                   int64      `json:"id"`                // 数据库自增ID
	Path                 string     `json:"-"`                 // 文件绝对路径（不返回给前端，安全考虑）
	Title                string     `json:"title"`             // 文档标题（取自PDF元数据或文件名）
	CacheKey             string     `json:"cache_key"`         // 缓存键（路径哈希，用于URL路由和缓存目录命名）
	RelativePath         string     `json:"relative_path"`     // 相对于扫描根目录的路径（用于文件树展示）
	PageCount            int        `json:"page_count"`        // 总页数
	FileSize             int64      `json:"file_size"`         // 文件大小（字节）
	FileMtime            int64      `json:"file_mtime"`        // 文件最后修改时间（Unix时间戳）
	Pinned               bool       `json:"pinned"`            // 是否标记为主动缓存
	CacheActive          bool       `json:"cache_active"`      // 主动缓存是否已就绪
	CacheSize            int64      `json:"cache_size"`        // 已缓存页面总大小（字节）
	CachedPages          int        `json:"cached_pages"`      // 已缓存页面数
	CacheVersion         int64      `json:"-"`                 // 缓存版本号（每次页面内容变化递增）
	ChangedPages         string     `json:"-"`                 // 最近一次变化的具体页码（逗号分隔，空表示范围未知）
	GoodNotesPath        string     `json:"-"`                 // 同目录同名的 GoodNotes 归档路径
	GoodNotesSize        int64      `json:"-"`                 // GoodNotes 归档大小（用于变化检测）
	GoodNotesMtime       int64      `json:"-"`                 // GoodNotes 归档修改时间（用于变化检测）
	OutlineSources       string     `json:"-"`                 // 用户选择的目录来源集合；auto/none/CSV
	ActiveOutlineSources string     `json:"-"`                 // 最近一次实际生效的目录来源集合
	OutlineOrigin        string     `json:"-"`                 // Deprecated: 旧版单一目录来源
	DeepScan             int        `json:"-"`                 // Deprecated: 旧版深度扫描偏好
	CacheLastAccess      time.Time  `json:"cache_last_access"` // 最后访问时间（用于LRU淘汰）
	Deleted              bool       `json:"-"`                 // 是否已标记删除（不返回给前端）
	DeletedAt            *time.Time `json:"-"`                 // 首次被标记删除的时间（用于清理宽限期）
	CreatedAt            time.Time  `json:"created_at"`        // 记录创建时间
	UpdatedAt            time.Time  `json:"updated_at"`        // 记录最后更新时间
}

// CacheStatus 表示某个PDF的缓存状态（用于API响应）
type CacheStatus struct {
	Pinned       bool  `json:"pinned"`        // 是否已标记缓存
	Total        int   `json:"total"`         // 总页数
	Cached       int   `json:"cached"`        // 已缓存页数
	CacheSize    int64 `json:"cache_size_mb"` // 缓存大小（MB）
	Rebuilding   bool  `json:"rebuilding"`    // 是否正在重建缓存
	Version      int64 `json:"version"`       // 缓存版本号，前端据此判断是否需要刷新页面
	ChangedPages []int `json:"changed_pages"` // 最近一次变化的具体页码，空数组表示变化范围未知
}

// PageMark 表示一个页面标记（可以是PDF自带目录项或用户自定义标记）
type PageMark struct {
	ID            int64   `json:"id"`                       // 标记ID
	PageNum       int     `json:"page_num"`                 // 页码（0表示纯标题/分隔符）
	Title         string  `json:"title"`                    // 标记标题
	IsOutline     bool    `json:"is_outline"`               // 是否来自自动提取的目录
	OutlineSource string  `json:"outline_source,omitempty"` // 目录来源；用户标记为空
	Level         int     `json:"level"`                    // 目录层级（0为顶级）
	SortOrder     float64 `json:"sort_order"`               // 排序值（支持小数，用于拖拽排序）
	ParentID      int64   `json:"parent_id"`                // 父标记ID（0表示无父级）
}

// OutlineEntry 表示PDF目录的一个条目（从 mutool 提取）
type OutlineEntry struct {
	Level int    // 目录层级（0为顶级）
	Page  int    // 页码
	Title string // 目录标题
}
