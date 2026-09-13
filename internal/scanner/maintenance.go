// 扫描器的对账与清理逻辑：每轮扫描结束后同步数据库与磁盘状态
package scanner

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"note-web/internal/db"
)

// 发现数量骤降的保护阈值：相对上一轮减少超过 30% 且绝对减少不少于 5 个时，
// 认为本轮结果不可信并推迟物理清理，避免挂载掉线等情况把整库判成已删除。
const (
	deletionGuardRatio = 0.3
	deletionGuardMin   = 5
)

// reconcile 在每轮扫描末尾对账数据库与磁盘：
//  1. 把磁盘上已消失的记录标记为软删除，并记录首次消失时间（可逆，每轮执行）
//  2. 物理清理超过宽限期的记录（page_marks 由外键级联删除）及其页面缓存；
//     本轮扫描不可信时推迟一轮
//  3. 清理孤儿标记与无主缓存目录
//  4. 到点执行 VACUUM
func (s *Scanner) reconcile(foundPaths map[string]bool, failedDirs []string) {
	refs, err := s.db.ListActivePDFRefs()
	if err != nil {
		log.Printf("maintenance: failed to list active PDFs: %v", err)
		return
	}

	missing := make([]int64, 0)
	for _, ref := range refs {
		if foundPaths[ref.Path] || underAnyDir(ref.Path, failedDirs) {
			continue
		}
		missing = append(missing, ref.ID)
	}

	// 软删除是可逆的（文件回来后 AddPDF 会复用同一行），所以每轮都执行。
	if err := s.db.SoftDeletePDFs(missing); err != nil {
		log.Printf("maintenance: failed to mark %d record(s) deleted: %v", len(missing), err)
	}

	// 物理清理不可逆：本轮扫描看起来不可信时推迟到下一轮。
	var purged []db.PurgedPDF
	if reason := s.deletionGuardReason(len(foundPaths), len(refs)); reason != "" {
		if len(missing) > 0 {
			log.Printf("maintenance: deferred purging %d record(s): %s", len(missing), reason)
		}
	} else {
		purged, err = s.db.PurgeDeletedPDFs(time.Now().Add(-s.retention))
		if err != nil {
			log.Printf("maintenance: failed to purge deleted records: %v", err)
			purged = nil
		}
	}
	if s.cache != nil {
		// 事务提交后再删缓存目录，避免数据库回滚时缓存已经消失。
		for _, item := range purged {
			if err := s.cache.RemoveCacheDir(item.CacheKey); err != nil {
				log.Printf("maintenance: failed to remove cache for %s: %v", item.Path, err)
			}
		}
	}

	orphanMarks, err := s.db.CleanupOrphanMarks()
	if err != nil {
		log.Printf("maintenance: failed to clean orphan marks: %v", err)
	}

	orphanDirs, orphanBytes := 0, int64(0)
	if s.cache != nil {
		liveKeys, keyErr := s.db.ListCacheKeys()
		if keyErr != nil {
			log.Printf("maintenance: failed to list cache keys: %v", keyErr)
		} else {
			live := make(map[string]bool, len(liveKeys))
			for _, key := range liveKeys {
				live[key] = true
			}
			var sweepErr error
			orphanDirs, orphanBytes, sweepErr = s.cache.SweepOrphanCacheDirs(live)
			if sweepErr != nil {
				log.Printf("maintenance: failed to sweep orphan cache dirs: %v", sweepErr)
			}
		}
	}

	s.lastFound = len(foundPaths)
	s.hasBaseline = true

	if len(missing) > 0 || len(purged) > 0 || orphanMarks > 0 || orphanDirs > 0 {
		log.Printf("maintenance: soft-deleted=%d purged=%d orphan-marks=%d orphan-cache-dirs=%d freed=%.1fMB",
			len(missing), len(purged), orphanMarks, orphanDirs, float64(orphanBytes)/(1024*1024))
	}

	if vacuumed, err := s.db.MaybeVacuum(s.vacuumInterval); err != nil {
		log.Printf("maintenance: vacuum failed: %v", err)
	} else if vacuumed {
		log.Printf("maintenance: vacuum completed")
	}
}

// deletionGuardReason 返回非空字符串表示本轮扫描结果不可信、应推迟物理清理。
func (s *Scanner) deletionGuardReason(found, existing int) string {
	if existing == 0 || found >= existing {
		return ""
	}
	if !s.hasBaseline {
		// 首次扫描（含重启后的首轮）：库里还有记录却一个文件都没扫到，
		// 更像是目录未挂载而不是真的全部删除，先观察一轮。
		if found == 0 {
			return "first scan found no PDFs while the database still has records"
		}
		return ""
	}
	drop := s.lastFound - found
	if drop >= deletionGuardMin && float64(drop) > float64(s.lastFound)*deletionGuardRatio {
		return fmt.Sprintf("found %d PDFs, down from %d", found, s.lastFound)
	}
	return ""
}

// underAnyDir 判断路径是否位于本轮遍历失败的目录之下。
// 这些目录的扫描结果不完整，不能据此判定文件已被删除。
func underAnyDir(path string, dirs []string) bool {
	for _, dir := range dirs {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			continue
		}
		if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return true
		}
	}
	return false
}
