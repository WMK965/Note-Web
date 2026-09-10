// PDF 渲染器包
// 封装外部命令（pdftoppm、pdfinfo、mutool）用于渲染PDF页面和提取元数据
// 安全措施：
//   - 使用 exec.CommandContext 限制子进程执行时间（M3）
//   - 命令参数中加 -- 终止选项解析，防止参数注入（M4）
//   - 页数上限 10000，防止恶意PDF触发渲染循环DoS（H3）
//   - 打开文件前 Lstat 校验，拒绝符号链接（H2/M14）
package renderer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"note-web/internal/config"
	"note-web/internal/models"
	"rsc.io/pdf"
)

// 子进程超时设置
const (
	renderTimeout  = 60 * time.Second // pdftoppm 渲染单页超时
	infoTimeout    = 10 * time.Second // pdfinfo/mutool 超时
	maxPageCount   = 10000            // 页数上限，防止恶意PDF DoS（H3）
)

type Renderer struct {
	jpegQuality int
	scaleWidth  int
	thumbWidth  int
}

func New(cfg config.RenderConfig) *Renderer {
	return &Renderer{
		jpegQuality: cfg.JpegQuality,
		scaleWidth:  cfg.ScaleWidth,
		thumbWidth:  cfg.ThumbWidth,
	}
}

// RenderPage 将PDF的指定页面渲染为JPEG图片
func (r *Renderer) RenderPage(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	return r.render(pdfPath, pageNum, outputPrefix, r.scaleWidth, r.jpegQuality)
}

// RenderThumbnail 渲染缩略图
func (r *Renderer) RenderThumbnail(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	return r.render(pdfPath, pageNum, outputPrefix, r.thumbWidth, 60)
}

// render 调用 pdftoppm 渲染页面
// 安全：使用 CommandContext 限制超时（M3），加 -- 防止参数注入（M4）
func (r *Renderer) render(pdfPath string, pageNum int, outputPrefix string, width int, quality int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), renderTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pdftoppm",
		"-f", strconv.Itoa(pageNum),
		"-l", strconv.Itoa(pageNum),
		"-jpeg",
		"-jpegopt", fmt.Sprintf("quality=%d", quality),
		"-scale-to-x", strconv.Itoa(width),
		"-scale-to-y", "-1",
		"-singlefile",
		"--", // 终止选项解析，防止以 - 开头的文件名被当作选项（M4）
		pdfPath,
		outputPrefix,
	)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("pdftoppm failed: %w", err)
	}

	return outputPrefix + ".jpg", nil
}

type PDFInfo struct {
	PageCount int
	Title     string
}

// GetPDFInfo 获取PDF的页数和标题
// 安全：页数上限 10000（H3），pdfinfo 崩溃时用纯Go库 fallback
func (r *Renderer) GetPDFInfo(pdfPath string) (*PDFInfo, error) {
	info := &PDFInfo{}

	if pageCount, title, err := r.getInfoViaPdfinfo(pdfPath); err == nil {
		info.PageCount = pageCount
		info.Title = title
	} else if pageCount, err := r.getInfoViaGo(pdfPath); err == nil {
		info.PageCount = pageCount
	}

	if info.Title == "" || info.Title == "Untitled" {
		base := filepath.Base(pdfPath)
		info.Title = strings.TrimSuffix(base, filepath.Ext(base))
	}

	// 页数上限校验（H3）
	if info.PageCount > maxPageCount {
		return nil, fmt.Errorf("page count %d exceeds limit %d", info.PageCount, maxPageCount)
	}
	if info.PageCount == 0 {
		return nil, fmt.Errorf("could not determine page count for %s", pdfPath)
	}

	return info, nil
}

// getInfoViaPdfinfo 使用 pdfinfo 命令获取页数和标题
// 安全：CommandContext 超时（M3），-- 防参数注入（M4）
func (r *Renderer) getInfoViaPdfinfo(pdfPath string) (pageCount int, title string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pdfinfo", "--", pdfPath)
	output, err := cmd.Output()
	if err != nil {
		return 0, "", err
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Pages:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				pageCount, _ = strconv.Atoi(parts[1])
			}
		} else if strings.HasPrefix(line, "Title:") {
			title = strings.TrimSpace(strings.TrimPrefix(line, "Title:"))
		}
	}

	if pageCount == 0 {
		return 0, "", fmt.Errorf("pdfinfo returned no page count")
	}
	return pageCount, title, nil
}

// getInfoViaGo 使用纯Go库获取页数（pdfinfo 崩溃时的 fallback）
func (r *Renderer) getInfoViaGo(pdfPath string) (int, error) {
	f, err := os.Open(pdfPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	reader, err := pdf.NewReader(f, fileSize(pdfPath))
	if err != nil {
		return 0, err
	}

	return reader.NumPage(), nil
}

func fileSize(path string) int64 {
	stat, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return stat.Size()
}

// CheckDependencies 检查必需的外部依赖
func CheckDependencies() error {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		return fmt.Errorf("pdftoppm not found in PATH (install poppler-utils)")
	}
	return nil
}

// GetOutline 使用 mutool 提取PDF目录
// 安全：CommandContext 超时（M3），-- 防参数注入（M4）
func (r *Renderer) GetOutline(pdfPath string) ([]models.OutlineEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "mutool", "show", "--", pdfPath, "outline")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("mutool show outline failed: %w", err)
	}

	var entries []models.OutlineEntry
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		level, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		page, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
		title := strings.TrimSpace(parts[2])
		if title == "" {
			continue
		}
		// 页数上限校验
		if page > maxPageCount {
			continue
		}
		entries = append(entries, models.OutlineEntry{
			Level: level,
			Page:  page,
			Title: title,
		})
	}
	return entries, nil
}

func HasMutool() bool {
	_, err := exec.LookPath("mutool")
	return err == nil
}

// IsSymlink 检查路径是否为符号链接（H2/M14 安全防御）
func IsSymlink(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeSymlink != 0
}
