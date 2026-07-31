// 数据模型包
// 定义数据库实体和API响应的数据结构
package models

import "time"

// PDF 表示数据库中的一个PDF文件记录
type PDF struct {
	ID              int64     `json:"id"`                // 数据库自增ID
	Path            string    `json:"-"`                 // 文件绝对路径（不返回给前端，安全考虑）
	Title           string    `json:"title"`             // 文档标题（取自PDF元数据或文件名）
	CacheKey        string    `json:"cache_key"`         // 缓存键（路径哈希，用于URL路由和缓存目录命名）
	RelativePath    string    `json:"relative_path"`     // 相对于扫描根目录的路径（用于文件树展示）
	PageCount       int       `json:"page_count"`        // 总页数
	FileSize        int64     `json:"file_size"`          // 文件大小（字节）
	FileMtime       int64     `json:"file_mtime"`         // 文件最后修改时间（Unix时间戳）
	Pinned          bool      `json:"pinned"`             // 是否标记为主动缓存
	CacheActive     bool      `json:"cache_active"`       // 主动缓存是否已就绪
	CacheSize       int64     `json:"cache_size"`         // 已缓存页面总大小（字节）
	CachedPages     int       `json:"cached_pages"`       // 已缓存页面数
	CacheLastAccess time.Time `json:"cache_last_access"`  // 最后访问时间（用于LRU淘汰）
	Deleted         bool      `json:"-"`                  // 是否已标记删除（不返回给前端）
	CreatedAt       time.Time `json:"created_at"`         // 记录创建时间
	UpdatedAt       time.Time `json:"updated_at"`         // 记录最后更新时间
}

// CacheStatus 表示某个PDF的缓存状态（用于API响应）
type CacheStatus struct {
	Pinned      bool   `json:"pinned"`        // 是否已标记缓存
	Total       int    `json:"total"`         // 总页数
	Cached      int    `json:"cached"`        // 已缓存页数
	CacheSize   int64  `json:"cache_size_mb"` // 缓存大小（MB）
	Rebuilding  bool   `json:"rebuilding"`    // 是否正在重建缓存
}

// PageMark 表示一个页面标记（可以是PDF自带目录项或用户自定义标记）
type PageMark struct {
	ID        int64   `json:"id"`          // 标记ID
	PageNum   int     `json:"page_num"`    // 页码（0表示纯标题/分隔符）
	Title     string  `json:"title"`       // 标记标题
	IsOutline bool    `json:"is_outline"`  // 是否来自PDF自带目录
	Level     int     `json:"level"`       // 目录层级（0为顶级）
	SortOrder float64 `json:"sort_order"`  // 排序值（支持小数，用于拖拽排序）
	ParentID  int64   `json:"parent_id"`   // 父标记ID（0表示无父级）
}

// OutlineEntry 表示PDF目录的一个条目（从 mutool 提取）
type OutlineEntry struct {
	Level int    // 目录层级（0为顶级）
	Page  int    // 页码
	Title string // 目录标题
}
