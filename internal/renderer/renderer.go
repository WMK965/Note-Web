// PDF 渲染器包
// 封装外部命令（pdftoppm、pdfinfo、mutool）用于渲染PDF页面和提取元数据
// pdftoppm（来自 poppler-utils）：将PDF页面渲染为JPEG图片
// pdfinfo（来自 poppler-utils）：获取PDF页数和标题
// mutool（来自 mupdf-tools）：提取PDF目录/书签
// rsc.io/pdf（纯Go库）：当 pdfinfo 崩溃时的页数获取后备方案
package renderer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"note-web/internal/config"
	"note-web/internal/models"
	"rsc.io/pdf"
)

// Renderer 是PDF渲染器，封装了渲染参数和外部命令调用
type Renderer struct {
	jpegQuality int // JPEG压缩质量（1-100）
	scaleWidth  int // 页面渲染宽度（像素）
	thumbWidth  int // 缩略图宽度（像素）
}

// New 创建渲染器实例
func New(cfg config.RenderConfig) *Renderer {
	return &Renderer{
		jpegQuality: cfg.JpegQuality,
		scaleWidth:  cfg.ScaleWidth,
		thumbWidth:  cfg.ThumbWidth,
	}
}

// RenderPage 将PDF的指定页面渲染为JPEG图片
// 参数：
//   - pdfPath: PDF文件路径
//   - pageNum: 页码（从1开始）
//   - outputPrefix: 输出文件前缀（不含.jpg扩展名，pdftoppm会自动添加）
// 返回：生成的JPEG文件完整路径
func (r *Renderer) RenderPage(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	return r.render(pdfPath, pageNum, outputPrefix, r.scaleWidth, r.jpegQuality)
}

// RenderThumbnail 渲染缩略图（更小尺寸、更低质量以减小文件大小）
func (r *Renderer) RenderThumbnail(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	return r.render(pdfPath, pageNum, outputPrefix, r.thumbWidth, 60)
}

// render 是实际的渲染函数，调用 pdftoppm 命令
// 命令格式：pdftoppm -f {页码} -l {页码} -jpeg -jpegopt quality={质量}
//          -scale-to-x {宽度} -scale-to-y -1 -singlefile {输入} {输出前缀}
// -scale-to-y -1 表示按比例自动计算高度
// -singlefile 表示只输出一个文件（不添加页码后缀）
func (r *Renderer) render(pdfPath string, pageNum int, outputPrefix string, width int, quality int) (string, error) {
	cmd := exec.Command("pdftoppm",
		"-f", strconv.Itoa(pageNum),
		"-l", strconv.Itoa(pageNum),
		"-jpeg",
		"-jpegopt", fmt.Sprintf("quality=%d", quality),
		"-scale-to-x", strconv.Itoa(width),
		"-scale-to-y", "-1",
		"-singlefile",
		pdfPath,
		outputPrefix,
	)

	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("pdftoppm failed: %w: %s", err, string(output))
	}

	// pdftoppm 会在 outputPrefix 后自动添加 .jpg 扩展名
	return outputPrefix + ".jpg", nil
}

// PDFInfo 包含从PDF文件中提取的元数据
type PDFInfo struct {
	PageCount int    // 总页数
	Title     string // 文档标题
}

// GetPDFInfo 获取PDF的页数和标题信息
// 首先尝试使用 pdfinfo 命令，如果失败（例如在Windows上崩溃），
// 则使用纯Go库 rsc.io/pdf 作为后备方案获取页数
// 标题为空或"Untitled"时，使用文件名（不含扩展名）作为标题
func (r *Renderer) GetPDFInfo(pdfPath string) (*PDFInfo, error) {
	info := &PDFInfo{}

	// 方案1：使用 pdfinfo 命令（可获取页数和标题）
	if pageCount, title, err := r.getInfoViaPdfinfo(pdfPath); err == nil {
		info.PageCount = pageCount
		info.Title = title
	} else if pageCount, err := r.getInfoViaGo(pdfPath); err == nil {
		// 方案2：使用纯Go库（仅获取页数，无法获取标题）
		info.PageCount = pageCount
	}

	// 标题为空时使用文件名
	if info.Title == "" || info.Title == "Untitled" {
		base := filepath.Base(pdfPath)
		info.Title = strings.TrimSuffix(base, filepath.Ext(base))
	}

	// 两种方案都失败，返回错误
	if info.PageCount == 0 {
		return nil, fmt.Errorf("could not determine page count for %s", pdfPath)
	}

	return info, nil
}

// getInfoViaPdfinfo 使用 pdfinfo 命令获取页数和标题
// pdfinfo 输出示例：
//   Pages:          120
//   Title:          My Notes
func (r *Renderer) getInfoViaPdfinfo(pdfPath string) (pageCount int, title string, err error) {
	cmd := exec.Command("pdfinfo", pdfPath)
	output, err := cmd.Output()
	if err != nil {
		return 0, "", err
	}

	// 逐行解析 pdfinfo 的输出
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

// getInfoViaGo 使用纯Go库 rsc.io/pdf 获取页数
// 作为 pdfinfo 命令崩溃时的后备方案
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

// fileSize 获取文件大小（rsc.io/pdf 需要此参数）
func fileSize(path string) int64 {
	stat, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return stat.Size()
}

// CheckDependencies 检查必需的外部依赖是否安装
// pdftoppm 是必需的（用于渲染页面）
// pdfinfo 和 mutool 是可选的（有Go库后备方案）
func CheckDependencies() error {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		return fmt.Errorf("pdftoppm not found in PATH (install poppler-utils)")
	}
	return nil
}

// GetOutline 使用 mutool 提取PDF的目录/书签
// 命令：mutool show input.pdf outline
// 输出格式（tab分隔）：level\tpage\ttitle
//   level: 层级（0为顶级，1为二级，...）
//   page: 页码
//   title: 目录标题
func (r *Renderer) GetOutline(pdfPath string) ([]models.OutlineEntry, error) {
	cmd := exec.Command("mutool", "show", pdfPath, "outline")
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
		// 按 tab 分割为 3 列：level, page, title
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
		entries = append(entries, models.OutlineEntry{
			Level: level,
			Page:  page,
			Title: title,
		})
	}
	return entries, nil
}

// HasMutool 检查 mutool 命令是否可用
func HasMutool() bool {
	_, err := exec.LookPath("mutool")
	return err == nil
}
