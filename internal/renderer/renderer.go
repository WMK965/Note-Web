package renderer

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"note-web/internal/config"
	"rsc.io/pdf"
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

func (r *Renderer) RenderPage(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	return r.render(pdfPath, pageNum, outputPrefix, r.scaleWidth, r.jpegQuality)
}

func (r *Renderer) RenderThumbnail(pdfPath string, pageNum int, outputPrefix string) (string, error) {
	return r.render(pdfPath, pageNum, outputPrefix, r.thumbWidth, 60)
}

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

	return outputPrefix + ".jpg", nil
}

type PDFInfo struct {
	PageCount int
	Title     string
}

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

	if info.PageCount == 0 {
		return nil, fmt.Errorf("could not determine page count for %s", pdfPath)
	}

	return info, nil
}

func (r *Renderer) getInfoViaPdfinfo(pdfPath string) (pageCount int, title string, err error) {
	cmd := exec.Command("pdfinfo", pdfPath)
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

func CheckDependencies() error {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		return fmt.Errorf("pdftoppm not found in PATH (install poppler-utils)")
	}
	return nil
}
