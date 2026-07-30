package db

import (
	"database/sql"
	"time"

	"note-web/internal/models"

	_ "modernc.org/sqlite"
)

type Database struct {
	*sql.DB
}

func Init(path string) (*Database, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
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
			deleted INTEGER NOT NULL DEFAULT 0,
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
		db.Exec(`ALTER TABLE pdfs ADD COLUMN cache_key TEXT NOT NULL DEFAULT ''`)
	}

	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('pdfs') WHERE name = 'rebuilding'`).Scan(&colCount)
	if colCount == 0 {
		db.Exec(`ALTER TABLE pdfs ADD COLUMN rebuilding INTEGER NOT NULL DEFAULT 0`)
	}

	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_pdfs_pinned ON pdfs(pinned) WHERE pinned = 1 AND deleted = 0;
		CREATE INDEX IF NOT EXISTS idx_pdfs_deleted ON pdfs(deleted);
		CREATE INDEX IF NOT EXISTS idx_pdfs_cache_lru ON pdfs(cache_last_access) WHERE cache_active = 0 AND cached_pages > 0 AND deleted = 0;
		CREATE INDEX IF NOT EXISTS idx_pdfs_cache_key ON pdfs(cache_key) WHERE deleted = 0;
	`)
	return err
}

const pdfColumns = `id, path, title, cache_key, page_count, file_size, file_mtime,
		       pinned, cache_active, cache_size, cached_pages, cache_last_access,
		       deleted, rebuilding, created_at, updated_at`

func scanPDF(r interface{ Scan(...interface{}) error }) (*models.PDF, error) {
	p := &models.PDF{}
	var pinned, cacheActive, deleted, rebuilding int
	var cacheLastAccess sql.NullTime
	if err := r.Scan(
		&p.ID, &p.Path, &p.Title, &p.CacheKey, &p.PageCount, &p.FileSize, &p.FileMtime,
		&pinned, &cacheActive, &p.CacheSize, &p.CachedPages, &cacheLastAccess,
		&deleted, &rebuilding, &p.CreatedAt, &p.UpdatedAt,
	); err != nil {
		return nil, err
	}
	p.Pinned = pinned == 1
	p.CacheActive = cacheActive == 1
	p.Deleted = deleted == 1
	if cacheLastAccess.Valid {
		p.CacheLastAccess = cacheLastAccess.Time
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
	res, err := db.Exec(`
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
	return res.LastInsertId()
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

func (db *Database) UpdatePDFMetadata(id int64, pageCount int, fileSize int64, fileMtime int64) error {
	_, err := db.Exec(`UPDATE pdfs SET page_count = ?, file_size = ?, file_mtime = ?, updated_at = ? WHERE id = ?`,
		pageCount, fileSize, fileMtime, time.Now(), id)
	return err
}

func (db *Database) MarkDeletedExcept(paths []string) error {
	if len(paths) == 0 {
		_, err := db.Exec(`UPDATE pdfs SET deleted = 1, updated_at = ? WHERE deleted = 0`, time.Now())
		return err
	}
	query := `UPDATE pdfs SET deleted = 1, updated_at = ? WHERE deleted = 0 AND path NOT IN (`
	args := []interface{}{time.Now()}
	for i, p := range paths {
		if i > 0 {
			query += ","
		}
		query += "?"
		args = append(args, p)
	}
	query += ")"
	_, err := db.Exec(query, args...)
	return err
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

func (db *Database) GetPassiveCacheSize() (int64, error) {
	var size int64
	err := db.QueryRow(`SELECT COALESCE(SUM(cache_size), 0) FROM pdfs WHERE cache_active = 0 AND cached_pages > 0 AND deleted = 0`).Scan(&size)
	return size, err
}

func (db *Database) GetLRUPDFs(limit int) ([]*models.PDF, error) {
	rows, err := db.Query(`SELECT `+pdfColumns+` FROM pdfs WHERE cache_active = 0 AND cached_pages > 0 AND deleted = 0 ORDER BY cache_last_access ASC NULLS FIRST LIMIT ?`, limit)
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

func (db *Database) GetCacheStatus(id int64) (*models.CacheStatus, error) {
	var pinned, cacheActive, rebuilding int
	var pageCount, cachedPages int
	var cacheSize int64
	err := db.QueryRow(`SELECT page_count, cached_pages, cache_size, pinned, cache_active, rebuilding FROM pdfs WHERE id = ?`, id).
		Scan(&pageCount, &cachedPages, &cacheSize, &pinned, &cacheActive, &rebuilding)
	if err != nil {
		return nil, err
	}
	return &models.CacheStatus{
		Pinned:     pinned == 1,
		Total:      pageCount,
		Cached:     cachedPages,
		CacheSize:  cacheSize / (1024 * 1024),
		Rebuilding: rebuilding == 1,
	}, nil
}
