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
	"log"
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
	renderTimeout = 60 * time.Second // pdftoppm 渲染单页超时
	infoTimeout   = 10 * time.Second // pdfinfo/mutool 超时
	maxPageCount  = 10000            // 页数上限，防止恶意PDF DoS（H3）
)

type Renderer struct {
	jpegQuality    int
	scaleShortEdge int
	thumbWidth     int
}

func New(cfg config.RenderConfig) *Renderer {
	return &Renderer{
		jpegQuality:    cfg.JpegQuality,
		scaleShortEdge: cfg.ScaleWidth,
		thumbWidth:     cfg.ThumbWidth,
	}
}

// RenderPage 将PDF的指定页面渲染为JPEG图片
func (r *Renderer) RenderPage(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	scaleX, scaleY := r.pageScaleArgs(pdfPath, pageNum)
	return r.render(pdfPath, pageNum, outputPrefix, scaleX, scaleY, r.jpegQuality)
}

// RenderThumbnail 渲染缩略图
func (r *Renderer) RenderThumbnail(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	return r.render(pdfPath, pageNum, outputPrefix, r.thumbWidth, -1, 60)
}

// render 调用 pdftoppm 渲染页面
// 安全：使用 CommandContext 限制超时（M3），加 -- 防止参数注入（M4）
func (r *Renderer) render(pdfPath string, pageNum int, outputPrefix string, scaleX, scaleY, quality int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), renderTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "pdftoppm",
		"-f", strconv.Itoa(pageNum),
		"-l", strconv.Itoa(pageNum),
		"-jpeg",
		"-jpegopt", fmt.Sprintf("quality=%d", quality),
		"-scale-to-x", strconv.Itoa(scaleX),
		"-scale-to-y", strconv.Itoa(scaleY),
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

// pageScaleArgs 让 scale_width 表示页面短边长度。
// pdftoppm 默认使用 MediaBox，且旋转不会改变短边长度，因此直接按原始横竖方向选轴。
// 无法读取页面尺寸时保持旧的竖页行为。
func (r *Renderer) pageScaleArgs(pdfPath string, pageNum int) (int, int) {
	width, height, ok := readPageBoxSize(pdfPath, pageNum)
	if ok && width > height {
		return -1, r.scaleShortEdge
	}
	return r.scaleShortEdge, -1
}

func readPageBoxSize(pdfPath string, pageNum int) (width, height float64, ok bool) {
	f, err := os.Open(pdfPath)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()

	reader, err := pdf.NewReader(f, fileSize(pdfPath))
	if err != nil || pageNum < 1 || pageNum > reader.NumPage() {
		return 0, 0, false
	}
	page := reader.Page(pageNum)

	box := inheritedPageValue(page.V, "MediaBox")
	if box.Len() < 4 {
		return 0, 0, false
	}

	width = absFloat(box.Index(2).Float64() - box.Index(0).Float64())
	height = absFloat(box.Index(3).Float64() - box.Index(1).Float64())
	if width <= 0 || height <= 0 {
		return 0, 0, false
	}
	return width, height, true
}

// Page 结构没有公开继承属性访问器，这里沿 Parent 链读取 MediaBox。
func inheritedPageValue(value pdf.Value, key string) pdf.Value {
	for depth := 0; depth < 64 && !value.IsNull(); depth++ {
		if found := value.Key(key); !found.IsNull() {
			return found
		}
		value = value.Key("Parent")
	}
	return pdf.Value{}
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
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
//
// 输出格式（每行一条目录项，按层级缩进）：
//
//	<状态字符><制表符×N>"标题"<制表符>uri
//
// 状态字符：'|' 无子项，'+' 有子项（展开），'-' 有子项（折叠）
// N = 层级 + 1（第一个制表符是分隔符，其余为缩进）
// uri 形如 "#page=24&zoom=nan,0,-.29"，页码从 1 开始
func (r *Renderer) GetOutline(pdfPath string) ([]models.OutlineEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "mutool", "show", "--", pdfPath, "outline")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("mutool show outline failed: %w", err)
	}

	entries := parseMutoolOutline(string(output))
	if len(entries) == 0 && strings.TrimSpace(string(output)) != "" {
		log.Printf("outline: parsed no entries from mutool output for %s", pdfPath)
	}
	return entries, nil
}

// maxOutlineDepth 限制目录层级，防止异常输入产生超深缩进
const maxOutlineDepth = 20

// parseMutoolOutline 解析 mutool show <pdf> outline 的输出
func parseMutoolOutline(output string) []models.OutlineEntry {
	var entries []models.OutlineEntry

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if len(line) < 2 {
			continue
		}
		// 状态字符标识该项在目录树中的位置，其他行（如警告）直接忽略
		switch line[0] {
		case '|', '+', '-':
		default:
			continue
		}

		rest := line[1:]
		indent := 0
		for len(rest) > 0 && rest[0] == '\t' {
			indent++
			rest = rest[1:]
		}
		level := indent - 1 // 第一个制表符是分隔符，其余才是缩进
		if level < 0 {
			level = 0
		}
		if level > maxOutlineDepth {
			level = maxOutlineDepth
		}

		if !strings.HasPrefix(rest, `"`) {
			continue
		}
		title, tail, ok := parseQuotedString(rest)
		if !ok {
			continue
		}
		title = cleanOutlineTitle(title)
		if title == "" {
			continue
		}

		entries = append(entries, models.OutlineEntry{
			Level: level,
			Page:  outlinePageFromURI(strings.TrimSpace(tail)),
			Title: title,
		})
	}

	return entries
}

// parseQuotedString 解析 mutool 输出的带引号字符串（如 `"第一章\x00"`）
// 返回字符串内容、剩余部分以及是否正常闭合
func parseQuotedString(s string) (value string, rest string, ok bool) {
	if !strings.HasPrefix(s, `"`) {
		return "", s, false
	}

	var b strings.Builder
	for i := 1; i < len(s); {
		c := s[i]
		switch {
		case c == '"':
			return b.String(), s[i+1:], true
		case c == '\\' && i+1 < len(s):
			esc := s[i+1]
			if esc == 'x' && i+3 < len(s) {
				if v, err := strconv.ParseUint(s[i+2:i+4], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i += 4
					continue
				}
			}
			switch esc {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			default:
				b.WriteByte(esc) // \" \\ 以及未识别的转义
			}
			i += 2
		default:
			b.WriteByte(c)
			i++
		}
	}

	return b.String(), "", false // 引号未闭合
}

// cleanOutlineTitle 去掉标题中的控制字符（PDF元数据里常见的结尾 NUL）和首尾空白
func cleanOutlineTitle(title string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r == 0 || r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, title)
	return strings.TrimSpace(cleaned)
}

// outlinePageFromURI 从 mutool 的目标 URI 中取出页码（1-based）
// 形如 "#page=24&zoom=nan,0,-.29"；命名目标等无法解析的形式返回 0（不跳转）
func outlinePageFromURI(uri string) int {
	const marker = "#page="
	idx := strings.Index(uri, marker)
	if idx < 0 {
		return 0
	}

	digits := uri[idx+len(marker):]
	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}

	page, err := strconv.Atoi(digits[:end])
	if err != nil || page > maxPageCount {
		return 0
	}
	return page
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
