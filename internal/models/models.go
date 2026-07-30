package models

import "time"

type PDF struct {
	ID              int64     `json:"id"`
	Path            string    `json:"-"`
	Title           string    `json:"title"`
	CacheKey        string    `json:"cache_key"`
	RelativePath    string    `json:"relative_path"`
	PageCount       int       `json:"page_count"`
	FileSize        int64     `json:"file_size"`
	FileMtime       int64     `json:"file_mtime"`
	Pinned          bool      `json:"pinned"`
	CacheActive     bool      `json:"cache_active"`
	CacheSize       int64     `json:"cache_size"`
	CachedPages     int       `json:"cached_pages"`
	CacheLastAccess time.Time `json:"cache_last_access"`
	Deleted         bool      `json:"-"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type CacheStatus struct {
	Pinned      bool   `json:"pinned"`
	Total       int    `json:"total"`
	Cached      int    `json:"cached"`
	CacheSize   int64  `json:"cache_size_mb"`
	Rebuilding  bool   `json:"rebuilding"`
}
