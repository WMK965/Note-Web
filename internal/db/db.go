// 数据库层包
// 封装 SQLite 数据库操作，提供 PDF 记录和页面标记的 CRUD 方法
// 使用 modernc.org/sqlite（纯Go实现，无需CGO）作为 SQLite 驱动
// 数据库采用 WAL 模式提高并发读写性能
//
// 表结构：
//
//	pdfs        - PDF 文件记录（路径、页数、缓存状态等）
//	page_marks  - 页面标记（PDF自带目录 + 用户自定义标记）
package db

import (
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"note-web/internal/models"

	_ "modernc.org/sqlite"
)

type Database struct {
	*sql.DB
}

func Init(path string) (*Database, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		return nil, err
	}

	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		return nil, err
	}

	return &Database{db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS pdfs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			path TEXT UNIQUE NOT NULL,
			title TEXT NOT NULL,
			cache_key TEXT NOT NULL DEFAULT '',
			page_count INTEGER NOT NULL DEFAULT 0,
			file_size INTEGER NOT NULL DEFAULT 0,
			file_mtime INTEGER NOT NULL DEFAULT 0,
			pinned INTEGER NOT NULL DEFAULT 0,
			cache_active INTEGER NOT NULL DEFAULT 0,
			cache_size INTEGER NOT NULL DEFAULT 0,
			cached_pages INTEGER NOT NULL DEFAULT 0,
			cache_last_access TIMESTAMP,
			cache_version INTEGER NOT NULL DEFAULT 0,
			changed_pages TEXT NOT NULL DEFAULT '',
			goodnotes_path TEXT NOT NULL DEFAULT '',
			goodnotes_size INTEGER NOT NULL DEFAULT 0,
			goodnotes_mtime INTEGER NOT NULL DEFAULT 0,
			outline_origin TEXT NOT NULL DEFAULT '',
			deep_scan INTEGER NOT NULL DEFAULT -1,
			deleted INTEGER NOT NULL DEFAULT 0,
			deleted_at INTEGER,
			rebuilding INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return err
	}

	var colCount int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pdfs') WHERE name = 'cache_key'`).Scan(&colCount)
	if colCount == 0 {
		if _, err := db.Exec(`ALTER TABLE pdfs ADD COLUMN cache_key TEXT NOT NULL DEFAULT ''`); err != nil {
			log.Printf("migration: failed to add cache_key column: %v", err)
		}
	}

	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pdfs') WHERE name = 'rebuilding'`).Scan(&colCount)
	if colCount == 0 {
		if _, err := db.Exec(`ALTER TABLE pdfs ADD COLUMN rebuilding INTEGER NOT NULL DEFAULT 0`); err != nil {
			log.Printf("migration: failed to add rebuilding column: %v", err)
		}
	}

	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pdfs') WHERE name = 'cache_version'`).Scan(&colCount)
	if colCount == 0 {
		if _, err := db.Exec(`ALTER TABLE pdfs ADD COLUMN cache_version INTEGER NOT NULL DEFAULT 0`); err != nil {
			log.Printf("migration: failed to add cache_version column: %v", err)
		}
	}

	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pdfs') WHERE name = 'changed_pages'`).Scan(&colCount)
	if colCount == 0 {
		if _, err := db.Exec(`ALTER TABLE pdfs ADD COLUMN changed_pages TEXT NOT NULL DEFAULT ''`); err != nil {
			log.Printf("migration: failed to add changed_pages column: %v", err)
		}
	}

	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "goodnotes_path", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "goodnotes_size", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "goodnotes_mtime", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "deleted_at", definition: "INTEGER"},
		{name: "outline_origin", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "deep_scan", definition: "INTEGER NOT NULL DEFAULT -1"},
	} {
		db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pdfs') WHERE name = ?`, column.name).Scan(&colCount)
		if colCount == 0 {
			if _, err := db.Exec(`ALTER TABLE pdfs ADD COLUMN ` + column.name + ` ` + column.definition); err != nil {
				log.Printf("migration: failed to add %s column: %v", column.name, err)
			}
		}
	}

	// 旧的软删除记录没有时间戳：用 updated_at 回填（表示为 Unix 秒），
	// 让宽限期从现在开始计算，避免升级后立刻把历史残留全部清掉。
	if _, err := db.Exec(`UPDATE pdfs SET deleted_at = CAST(strftime('%s', updated_at) AS INTEGER)
		WHERE deleted = 1 AND deleted_at IS NULL`); err != nil {
		log.Printf("migration: failed to backfill deleted_at: %v", err)
	}

	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_pdfs_pinned ON pdfs(pinned) WHERE pinned = 1 AND deleted = 0;
		CREATE INDEX IF NOT EXISTS idx_pdfs_deleted ON pdfs(deleted);
		CREATE INDEX IF NOT EXISTS idx_pdfs_cache_lru ON pdfs(cache_last_access) WHERE cache_active = 0 AND cached_pages > 0 AND deleted = 0;
		CREATE INDEX IF NOT EXISTS idx_pdfs_cache_key ON pdfs(cache_key) WHERE deleted = 0;
		CREATE TABLE IF NOT EXISTS page_marks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pdf_id INTEGER NOT NULL,
			page_num INTEGER NOT NULL,
			title TEXT NOT NULL,
			level INTEGER NOT NULL DEFAULT 0,
			is_outline INTEGER NOT NULL DEFAULT 0,
			sort_order REAL NOT NULL DEFAULT 0,
			parent_id INTEGER DEFAULT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (pdf_id) REFERENCES pdfs(id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS idx_marks_pdf ON page_marks(pdf_id);
	`)

	// 迁移：为旧表添加新列
	var markColCount int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('page_marks') WHERE name = 'sort_order'`).Scan(&markColCount)
	if markColCount == 0 {
		if _, err := db.Exec(`ALTER TABLE page_marks ADD COLUMN sort_order REAL NOT NULL DEFAULT 0`); err != nil {
			log.Printf("migration: failed to add sort_order column: %v", err)
		}
	}
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('page_marks') WHERE name = 'parent_id'`).Scan(&markColCount)
	if markColCount == 0 {
		if _, err := db.Exec(`ALTER TABLE page_marks ADD COLUMN parent_id INTEGER DEFAULT NULL`); err != nil {
			log.Printf("migration: failed to add parent_id column: %v", err)
		}
	}

	// 迁移：给 page_marks 的外键补上 ON DELETE CASCADE。
	// 这样删除一条 pdfs 记录时，它的目录项与用户标记会被自动清除，
	// 不需要在两处手工维护，也不会再留下孤儿标记。
	if !hasCascadeForeignKey(db, "page_marks") {
		if err := rebuildMarksWithCascade(db); err != nil {
			log.Printf("migration: failed to add ON DELETE CASCADE to page_marks: %v", err)
		}
	}

	// 目录解析版本：旧版本按错误的格式解析 mutool 输出，提取出的目录项
	// 标题和页码都是错的，且没有父子关系，需要清除后重新提取。
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL DEFAULT '')`); err != nil {
		log.Printf("migration: failed to create meta table: %v", err)
	}
	var outlineParser string
	db.QueryRow(`SELECT value FROM meta WHERE key = ?`, keyOutlineParser).Scan(&outlineParser)
	if outlineParser != outlineParserVersion {
		if _, err := db.Exec(`DELETE FROM page_marks WHERE is_outline = 1`); err != nil {
			log.Printf("migration: failed to drop stale outline marks: %v", err)
		}
		if _, err := db.Exec(`UPDATE pdfs SET goodnotes_path = '', goodnotes_size = 0, goodnotes_mtime = 0, outline_origin = ''`); err != nil {
			log.Printf("migration: failed to reset GoodNotes source state: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, keyOutlineParser, outlineParserVersion); err != nil {
			log.Printf("migration: failed to record outline parser version: %v", err)
		}
	}

	return err
}

// hasCascadeForeignKey 判断某张表的外键是否已经带上 ON DELETE CASCADE
func hasCascadeForeignKey(db *sql.DB, table string) bool {
	rows, err := db.Query(`SELECT "on_delete" FROM pragma_foreign_key_list(?)`, table)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var onDelete string
		if err := rows.Scan(&onDelete); err != nil {
			return false
		}
		if strings.EqualFold(onDelete, "CASCADE") {
			return true
		}
	}
	return false
}

// rebuildMarksWithCascade 重建 page_marks 表，把外键补成 ON DELETE CASCADE。
// SQLite 不支持修改外键约束，只能按官方推荐的“建新表 -> 拷数据 -> 换名”流程来做。
// 拷贝前先清掉历史孤儿行，否则新表的外键校验会失败。
func rebuildMarksWithCascade(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM page_marks WHERE pdf_id NOT IN (SELECT id FROM pdfs)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE page_marks_migrated (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			pdf_id INTEGER NOT NULL,
			page_num INTEGER NOT NULL,
			title TEXT NOT NULL,
			level INTEGER NOT NULL DEFAULT 0,
			is_outline INTEGER NOT NULL DEFAULT 0,
			sort_order REAL NOT NULL DEFAULT 0,
			parent_id INTEGER DEFAULT NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (pdf_id) REFERENCES pdfs(id) ON DELETE CASCADE
		)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO page_marks_migrated
			(id, pdf_id, page_num, title, level, is_outline, sort_order, parent_id, created_at)
		SELECT id, pdf_id, page_num, title, level, is_outline, sort_order, parent_id, created_at
		FROM page_marks`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE page_marks`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE page_marks_migrated RENAME TO page_marks`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_marks_pdf ON page_marks(pdf_id)`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("migration: rebuilt page_marks with ON DELETE CASCADE")
	return nil
}

// 目录解析逻辑的版本号，变更解析方式时递增即可让旧数据重新提取
const (
	keyOutlineParser     = "outline_parser"
	outlineParserVersion = "5"
	keyLastVacuum        = "last_vacuum"
)

const pdfColumns = `id, path, title, cache_key, page_count, file_size, file_mtime,
		       pinned, cache_active, cache_size, cached_pages, cache_last_access,
		       deleted, rebuilding, cache_version, changed_pages,
		       goodnotes_path, goodnotes_size, goodnotes_mtime, created_at, updated_at,
		       deleted_at, outline_origin, deep_scan`

func scanPDF(r interface{ Scan(...interface{}) error }) (*models.PDF, error) {
	p := &models.PDF{}
	var pinned, cacheActive, deleted, rebuilding int
	var cacheLastAccess sql.NullTime
	var deletedAt sql.NullInt64
	if err := r.Scan(
		&p.ID, &p.Path, &p.Title, &p.CacheKey, &p.PageCount, &p.FileSize, &p.FileMtime,
		&pinned, &cacheActive, &p.CacheSize, &p.CachedPages, &cacheLastAccess,
		&deleted, &rebuilding, &p.CacheVersion, &p.ChangedPages,
		&p.GoodNotesPath, &p.GoodNotesSize, &p.GoodNotesMtime, &p.CreatedAt, &p.UpdatedAt,
		&deletedAt, &p.OutlineOrigin, &p.DeepScan,
	); err != nil {
		return nil, err
	}
	p.Pinned = pinned == 1
	p.CacheActive = cacheActive == 1
	p.Deleted = deleted == 1
	if cacheLastAccess.Valid {
		p.CacheLastAccess = cacheLastAccess.Time
	}
	if deletedAt.Valid {
		stamp := time.Unix(deletedAt.Int64, 0)
		p.DeletedAt = &stamp
	}
	return p, nil
}

func scanPDFs(rows *sql.Rows) ([]*models.PDF, error) {
	var pdfs []*models.PDF
	for rows.Next() {
		p, err := scanPDF(rows)
		if err != nil {
			return nil, err
		}
		pdfs = append(pdfs, p)
	}
	return pdfs, rows.Err()
}

func (db *Database) AddPDF(path, title, cacheKey string, pageCount int, fileSize int64, fileMtime int64) (int64, error) {
	_, err := db.Exec(`
		INSERT INTO pdfs (path, title, cache_key, page_count, file_size, file_mtime, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			title = excluded.title,
			cache_key = excluded.cache_key,
			page_count = excluded.page_count,
			file_size = excluded.file_size,
			file_mtime = excluded.file_mtime,
			deleted = 0,
			updated_at = ?
		WHERE pdfs.deleted = 1 OR pdfs.file_mtime != excluded.file_mtime OR pdfs.file_size != excluded.file_size
	`,
		path, title, cacheKey, pageCount, fileSize, fileMtime,
		time.Now(), time.Now(), time.Now(),
	)
	if err != nil {
		return 0, err
	}
	var id int64
	if err := db.QueryRow(`SELECT id FROM pdfs WHERE path = ?`, path).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

func (db *Database) GetPDF(id int64) (*models.PDF, error) {
	row := db.QueryRow(`SELECT `+pdfColumns+` FROM pdfs WHERE id = ?`, id)
	return scanPDF(row)
}

func (db *Database) GetPDFByCacheKey(key string) (*models.PDF, error) {
	row := db.QueryRow(`SELECT `+pdfColumns+` FROM pdfs WHERE cache_key = ? AND deleted = 0`, key)
	return scanPDF(row)
}

func (db *Database) GetPDFByPath(path string) (*models.PDF, error) {
	row := db.QueryRow(`SELECT `+pdfColumns+` FROM pdfs WHERE path = ?`, path)
	return scanPDF(row)
}

func (db *Database) ListPDFs() ([]*models.PDF, error) {
	rows, err := db.Query(`SELECT ` + pdfColumns + ` FROM pdfs WHERE deleted = 0 ORDER BY title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPDFs(rows)
}

func (db *Database) ListPinnedPDFs() ([]*models.PDF, error) {
	rows, err := db.Query(`SELECT ` + pdfColumns + ` FROM pdfs WHERE pinned = 1 AND deleted = 0 ORDER BY title COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPDFs(rows)
}

func (db *Database) ListWatchablePDFs() ([]*models.PDF, error) {
	rows, err := db.Query(`SELECT ` + pdfColumns + ` FROM pdfs WHERE pinned = 1 AND deleted = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPDFs(rows)
}

func (db *Database) UpdatePDFMetadata(id int64, title string, pageCount int, fileSize int64, fileMtime int64) error {
	_, err := db.Exec(`UPDATE pdfs SET title = ?, page_count = ?, file_size = ?, file_mtime = ?, updated_at = ? WHERE id = ?`,
		title, pageCount, fileSize, fileMtime, time.Now(), id)
	return err
}

// SetOutlineSource 记录目录来源（goodnotes / pdf / 空）以及归档签名
func (db *Database) SetOutlineSource(id int64, path string, fileSize, fileMtime int64, origin string) error {
	_, err := db.Exec(`UPDATE pdfs SET goodnotes_path = ?, goodnotes_size = ?, goodnotes_mtime = ?, outline_origin = ?, updated_at = ? WHERE id = ?`,
		path, fileSize, fileMtime, origin, time.Now(), id)
	return err
}

// SetDeepScan 记录某个 PDF 的目录深度扫描偏好：1 开启、0 关闭、-1 自动
func (db *Database) SetDeepScan(id int64, mode int) error {
	if mode < -1 || mode > 1 {
		return fmt.Errorf("invalid deep scan mode: %d", mode)
	}
	_, err := db.Exec(`UPDATE pdfs SET deep_scan = ?, updated_at = ? WHERE id = ?`, mode, time.Now(), id)
	return err
}

// PDFRef 是扫描对账需要的最小记录集合：id、路径与缓存键。
type PDFRef struct {
	ID       int64
	Path     string
	CacheKey string
}

// PurgedPDF 描述一条已经被物理清理的记录，供上层删除对应的页面缓存目录。
type PurgedPDF struct {
	ID       int64
	Path     string
	CacheKey string
}

// ListActivePDFRefs 返回所有未删除记录的 id/路径/缓存键。
// 扫描器据此判断哪些文件已经消失，过滤逻辑放在 Go 侧以便排除不可达目录。
func (db *Database) ListActivePDFRefs() ([]PDFRef, error) {
	rows, err := db.Query(`SELECT id, path, cache_key FROM pdfs WHERE deleted = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []PDFRef
	for rows.Next() {
		var ref PDFRef
		if err := rows.Scan(&ref.ID, &ref.Path, &ref.CacheKey); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}

// CountActivePDFs 返回未删除记录数
func (db *Database) CountActivePDFs() (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM pdfs WHERE deleted = 0`).Scan(&count)
	return count, err
}

// SoftDeletePDFs 把这些记录标记为已删除，并记录首次删除时间（用于宽限期）。
// 已存在的 deleted_at 不会被刷新，保证宽限期从第一次发现文件消失时开始算。
func (db *Database) SoftDeletePDFs(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE pdfs
			SET deleted = 1, deleted_at = COALESCE(deleted_at, ?), updated_at = ?
			WHERE id = ? AND deleted = 0`, now.Unix(), now, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListCacheKeys 返回所有仍占用缓存目录的 cache_key（含已软删除但尚未清理的记录）。
// 扫描对账时用它判断磁盘上的缓存目录是否还有归属。
func (db *Database) ListCacheKeys() ([]string, error) {
	rows, err := db.Query(`SELECT cache_key FROM pdfs WHERE cache_key != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// PurgeDeletedPDFs 物理删除超过宽限期的记录，连同它的目录项与用户标记。
// cutoff 是最晚可保留的删除时间：deleted_at 早于它的记录会被清理。
// 返回被删除的记录，调用方负责清理对应的缓存目录。
func (db *Database) PurgeDeletedPDFs(cutoff time.Time) ([]PurgedPDF, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	cutoffUnix := cutoff.Unix()
	rows, err := tx.Query(`SELECT id, path, cache_key FROM pdfs
		WHERE deleted = 1 AND (deleted_at IS NULL OR deleted_at <= ?)`, cutoffUnix)
	if err != nil {
		return nil, err
	}
	var purged []PurgedPDF
	for rows.Next() {
		var item PurgedPDF
		if err := rows.Scan(&item.ID, &item.Path, &item.CacheKey); err != nil {
			rows.Close()
			return nil, err
		}
		purged = append(purged, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if len(purged) == 0 {
		return nil, nil
	}

	// 外键已经带 ON DELETE CASCADE，这里显式删除是为了兼容尚未迁移的旧库。
	for _, item := range purged {
		if _, err := tx.Exec(`DELETE FROM page_marks WHERE pdf_id = ?`, item.ID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM pdfs WHERE id = ?`, item.ID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return purged, nil
}

// CleanupOrphanMarks 删除指向不存在 PDF 的标记（历史遗留或外部改库造成）。
func (db *Database) CleanupOrphanMarks() (int64, error) {
	res, err := db.Exec(`DELETE FROM page_marks WHERE pdf_id NOT IN (SELECT id FROM pdfs)`)
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return affected, nil
}

// MaybeVacuum 在距上次 VACUUM 超过 minInterval 时回收数据库空闲页。
// minInterval 不为正数时不做任何事；时间戳记录在 meta 表，跨重启生效。
func (db *Database) MaybeVacuum(minInterval time.Duration) (bool, error) {
	if minInterval <= 0 {
		return false, nil
	}
	var last string
	db.QueryRow(`SELECT value FROM meta WHERE key = ?`, keyLastVacuum).Scan(&last)
	if last != "" {
		if parsed, err := time.Parse(time.RFC3339, last); err == nil && time.Since(parsed) < minInterval {
			return false, nil
		}
	}

	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("maintenance: wal checkpoint failed: %v", err)
	}
	if _, err := db.Exec(`VACUUM`); err != nil {
		return false, err
	}
	if _, err := db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		keyLastVacuum, time.Now().Format(time.RFC3339)); err != nil {
		return true, err
	}
	return true, nil
}

func (db *Database) PinPDF(id int64) error {
	_, err := db.Exec(`UPDATE pdfs SET pinned = 1, updated_at = ? WHERE id = ?`, time.Now(), id)
	return err
}

func (db *Database) UnpinPDF(id int64) error {
	_, err := db.Exec(`UPDATE pdfs SET pinned = 0, cache_active = 0, updated_at = ? WHERE id = ?`, time.Now(), id)
	return err
}

func (db *Database) SetCacheActive(id int64, active bool) error {
	v := 0
	if active {
		v = 1
	}
	_, err := db.Exec(`UPDATE pdfs SET cache_active = ?, updated_at = ? WHERE id = ?`, v, time.Now(), id)
	return err
}

func (db *Database) SetRebuilding(id int64, rebuilding bool) error {
	v := 0
	if rebuilding {
		v = 1
	}
	_, err := db.Exec(`UPDATE pdfs SET rebuilding = ?, updated_at = ? WHERE id = ?`, v, time.Now(), id)
	return err
}

func (db *Database) UpdateCacheStats(id int64, cachedPages int, cacheSize int64) error {
	_, err := db.Exec(`UPDATE pdfs SET cached_pages = ?, cache_size = ?, cache_last_access = ?, updated_at = ? WHERE id = ?`,
		cachedPages, cacheSize, time.Now(), time.Now(), id)
	return err
}

func (db *Database) TouchCacheAccess(id int64) error {
	_, err := db.Exec(`UPDATE pdfs SET cache_last_access = ? WHERE id = ?`, time.Now(), id)
	return err
}

func (db *Database) GetTotalCacheSize() (int64, error) {
	var size int64
	err := db.QueryRow(`SELECT COALESCE(SUM(cache_size), 0) FROM pdfs WHERE cached_pages > 0 AND deleted = 0`).Scan(&size)
	return size, err
}

func (db *Database) GetLRUPDFs(limit int) ([]*models.PDF, error) {
	rows, err := db.Query(`SELECT `+pdfColumns+` FROM pdfs WHERE cached_pages > 0 AND deleted = 0 ORDER BY cache_last_access ASC NULLS FIRST LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPDFs(rows)
}

func (db *Database) IncrementCacheStats(id int64, size int64) error {
	_, err := db.Exec(`UPDATE pdfs SET cached_pages = cached_pages + 1, cache_size = cache_size + ?, cache_last_access = ?, updated_at = ? WHERE id = ?`,
		size, time.Now(), time.Now(), id)
	return err
}

func (db *Database) ClearCacheStats(id int64) error {
	_, err := db.Exec(`UPDATE pdfs SET cached_pages = 0, cache_size = 0, cache_active = 0, updated_at = ? WHERE id = ?`, time.Now(), id)
	return err
}

// RecordCacheChange 记录一次缓存内容变化：版本号递增，并保存本次变化的页码。
// changedPages 为 nil 表示变化范围未知（例如整份缓存被清除），前端需要按“全部可能变化”处理。
// 前端通过版本号判断文档是否更新，并通过页码列表只刷新真正变化的页面。
func (db *Database) RecordCacheChange(id int64, changedPages []int) error {
	_, err := db.Exec(`UPDATE pdfs SET cache_version = cache_version + 1, changed_pages = ?, updated_at = ? WHERE id = ?`,
		FormatPageNumbers(changedPages), time.Now(), id)
	return err
}

// FormatPageNumbers 将页码列表编码为数据库存储格式（逗号分隔）。
// nil 和空列表都编码为空字符串，表示变化范围未知。
func FormatPageNumbers(pages []int) string {
	if len(pages) == 0 {
		return ""
	}
	parts := make([]string, 0, len(pages))
	for _, p := range pages {
		parts = append(parts, strconv.Itoa(p))
	}
	return strings.Join(parts, ",")
}

// ParsePageNumbers 解析 FormatPageNumbers 写入的页码列表。
// 空字符串解析为 nil（变化范围未知）。
func ParsePageNumbers(s string) []int {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	pages := make([]int, 0, len(parts))
	for _, part := range parts {
		if p, err := strconv.Atoi(strings.TrimSpace(part)); err == nil {
			pages = append(pages, p)
		}
	}
	if len(pages) == 0 {
		return nil
	}
	return pages
}

func (db *Database) GetCacheStatus(id int64) (*models.CacheStatus, error) {
	var pinned, cacheActive, rebuilding int
	var pageCount, cachedPages int
	var cacheSize int64
	var cacheVersion int64
	var changedPages string
	err := db.QueryRow(`SELECT page_count, cached_pages, cache_size, pinned, cache_active, rebuilding, cache_version, changed_pages FROM pdfs WHERE id = ?`, id).
		Scan(&pageCount, &cachedPages, &cacheSize, &pinned, &cacheActive, &rebuilding, &cacheVersion, &changedPages)
	if err != nil {
		return nil, err
	}
	return &models.CacheStatus{
		Pinned:       pinned == 1,
		Total:        pageCount,
		Cached:       cachedPages,
		CacheSize:    cacheSize / (1024 * 1024),
		Rebuilding:   rebuilding == 1,
		Version:      cacheVersion,
		ChangedPages: ParsePageNumbers(changedPages),
	}, nil
}

func (db *Database) ListMarks(pdfID int64) ([]*models.PageMark, error) {
	rows, err := db.Query(`
		SELECT id, page_num, title, level, is_outline, sort_order, parent_id FROM page_marks
		WHERE pdf_id = ? ORDER BY sort_order ASC, id ASC
	`, pdfID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var marks []*models.PageMark
	for rows.Next() {
		m := &models.PageMark{}
		var isOutline int
		var parentID sql.NullInt64
		if err := rows.Scan(&m.ID, &m.PageNum, &m.Title, &m.Level, &isOutline, &m.SortOrder, &parentID); err != nil {
			return nil, err
		}
		m.IsOutline = isOutline == 1
		if parentID.Valid {
			m.ParentID = parentID.Int64
		}
		marks = append(marks, m)
	}
	return marks, rows.Err()
}

func (db *Database) AddMark(pdfID int64, pageNum int, title string, parentID int64, level int, sortOrder float64) (int64, error) {
	var parentVal interface{}
	if parentID == 0 {
		parentVal = nil
	} else {
		parentVal = parentID
	}
	res, err := db.Exec(`INSERT INTO page_marks (pdf_id, page_num, title, is_outline, parent_id, level, sort_order) VALUES (?, ?, ?, 0, ?, ?, ?)`,
		pdfID, pageNum, title, parentVal, level, sortOrder)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *Database) DeleteMark(markID, pdfID int64) error {
	// 将子标记的 parent_id 设为 NULL（M10: 防止悬空引用）
	db.Exec(`UPDATE page_marks SET parent_id = NULL WHERE parent_id = ? AND pdf_id = ?`, markID, pdfID)
	_, err := db.Exec(`DELETE FROM page_marks WHERE id = ? AND pdf_id = ? AND is_outline = 0`, markID, pdfID)
	return err
}

// ReorderMarks 批量更新标记的排序和父子关系
func (db *Database) ReorderMarks(pdfID int64, items []ReorderItem) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, item := range items {
		var parentVal interface{}
		if item.ParentID == 0 {
			parentVal = nil
		} else {
			parentVal = item.ParentID
		}
		if _, err := tx.Exec(`UPDATE page_marks SET sort_order = ?, parent_id = ? WHERE id = ? AND pdf_id = ?`,
			item.SortOrder, parentVal, item.ID, pdfID); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// ReorderItem 是 ReorderMarks 的参数
type ReorderItem struct {
	ID        int64   `json:"id"`
	SortOrder float64 `json:"sort_order"`
	ParentID  int64   `json:"parent_id"`
}

// GetMaxSortOrder 获取某个PDF下标记的最大排序值
func (db *Database) GetMaxSortOrder(pdfID int64) (float64, error) {
	var maxOrder float64
	err := db.QueryRow(`SELECT COALESCE(MAX(sort_order), 0) FROM page_marks WHERE pdf_id = ?`, pdfID).Scan(&maxOrder)
	return maxOrder, err
}

// HasOutlineMarks 检查是否已提取过PDF目录
func (db *Database) HasOutlineMarks(pdfID int64) (bool, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM page_marks WHERE pdf_id = ? AND is_outline = 1`, pdfID).Scan(&count)
	return count > 0, err
}

func (db *Database) DeleteOutlineMarks(pdfID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := detachOutlineChildren(tx, pdfID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM page_marks WHERE pdf_id = ? AND is_outline = 1`, pdfID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (db *Database) AddOutlineMarks(pdfID int64, entries []models.OutlineEntry) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := addOutlineMarks(tx, pdfID, entries); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ReplaceOutlineMarksFromArchive 原子地替换自动生成的目录项，
// 并记录归档签名（路径/大小/修改时间）与目录来源（goodnotes 或 pdf）。
func (db *Database) ReplaceOutlineMarksFromArchive(pdfID int64, entries []models.OutlineEntry, path string, fileSize, fileMtime int64, origin string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	if err := detachOutlineChildren(tx, pdfID); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`DELETE FROM page_marks WHERE pdf_id = ? AND is_outline = 1`, pdfID); err != nil {
		tx.Rollback()
		return err
	}
	if err := addOutlineMarks(tx, pdfID, entries); err != nil {
		tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`UPDATE pdfs SET goodnotes_path = ?, goodnotes_size = ?, goodnotes_mtime = ?, outline_origin = ?, updated_at = ? WHERE id = ?`,
		path, fileSize, fileMtime, origin, time.Now(), pdfID); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type statementExecutor interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

func detachOutlineChildren(executor statementExecutor, pdfID int64) error {
	_, err := executor.Exec(`UPDATE page_marks SET parent_id = NULL, level = 0
		WHERE pdf_id = ? AND is_outline = 0 AND parent_id IN (
			SELECT id FROM page_marks WHERE pdf_id = ? AND is_outline = 1
		)`, pdfID, pdfID)
	return err
}

func addOutlineMarks(executor statementExecutor, pdfID int64, entries []models.OutlineEntry) error {
	// 按层级维护祖先栈，为每个目录项算出父级：
	// 前端按 parent_id 渲染树形目录，缺失父级就会退化为平铺列表
	var stack []int64
	for i, e := range entries {
		level := e.Level
		if level < 0 {
			level = 0
		}
		if level > len(stack) {
			level = len(stack) // 层级跳跃时挂到当前最深的祖先，避免悬空父级
		}
		stack = stack[:level]

		var parentID interface{}
		if len(stack) > 0 {
			parentID = stack[len(stack)-1]
		}

		res, err := executor.Exec(`INSERT INTO page_marks (pdf_id, page_num, title, level, is_outline, parent_id, sort_order)
			VALUES (?, ?, ?, ?, 1, ?, ?)`,
			pdfID, e.Page, e.Title, level, parentID, float64(i+1))
		if err != nil {
			return err
		}

		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		stack = append(stack, id)
	}
	return nil
}
