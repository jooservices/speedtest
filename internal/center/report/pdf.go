package report

import (
	"bytes"
	"embed"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
)

//go:embed fonts/NotoSans.ttf
var fontFiles embed.FS

const (
	pdfMargin    = 10.0
	pdfBottom    = 200.0
	pdfLine      = 3.3
	pdfFontSize  = 6.5
	pdfCellInset = 1.0
)

var pdfWidths = []float64{43, 32, 50, 12, 12, 20, 20, 20, 30, 20, 16}
var pdfHeaders = []string{"ISP", "Node", "Plan", "Tests", "Fail", "Avg down", "p95 down", "Delivered", "Worst UTC", "Downtime min", "IP changes"}

// PDF renders the report with an embedded Unicode font and wraps long table values.
func PDF(r Report) ([]byte, error) {
	font, err := fontFiles.ReadFile("fonts/NotoSans.ttf")
	if err != nil {
		return nil, err
	}
	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.AddUTF8FontFromBytes("NotoSans", "", font)
	pdf.SetMargins(pdfMargin, pdfMargin, pdfMargin)
	pdf.SetAutoPageBreak(false, pdfMargin)
	pdf.SetCompression(false)
	pdf.SetFont("NotoSans", "", pdfFontSize)
	if err := pdf.Error(); err != nil {
		return nil, err
	}
	pdfPage(pdf, r)
	if len(r.Rows) == 0 {
		pdf.SetXY(pdfMargin, pdf.GetY()+4)
		pdf.MultiCell(275, 5, "No tests reported this month.", "", "L", false)
	}
	for _, row := range r.Rows {
		if err := drawPDFRow(pdf, r, pdfRowValues(row)); err != nil {
			return nil, err
		}
	}
	var out bytes.Buffer
	if err := pdf.Output(&out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func pdfPage(pdf *fpdf.Fpdf, r Report) {
	pdf.AddPage()
	pdf.SetFont("NotoSans", "", 9)
	pdf.CellFormat(275, 7, pdfTitle(r), "", 1, "L", false, 0, "")
	pdf.SetFont("NotoSans", "", pdfFontSize)
	lines, height := pdfMeasure(pdf, pdfHeaders)
	pdfDrawCells(pdf, lines, height, true)
}

func pdfTitle(r Report) string {
	partial := ""
	if r.Partial {
		partial = " - partial month"
	}
	engine := r.Engine
	if engine == "" {
		engine = "primary"
	}
	return fmt.Sprintf("Monthly ISP and plan report %s (UTC)%s - engine: %s", r.Month, partial, engine)
}

func pdfRowValues(row Row) []string {
	return []string{row.ISP, row.Node, planName(row.Plan), fmt.Sprint(row.Tests), fmt.Sprint(row.Failures), num(row.AvgDown), num(row.P95Down),
		num(row.DeliveredPct), hours(row.WorstHours), fmt.Sprint(row.DowntimeMin), fmt.Sprint(row.IPChanges)}
}

func drawPDFRow(pdf *fpdf.Fpdf, report Report, values []string) error {
	lines, height := pdfMeasure(pdf, values)
	if pdf.GetY()+height > pdfBottom {
		pdfPage(pdf, report)
	}
	pdfDrawCells(pdf, lines, height, false)
	return pdf.Error()
}

func pdfMeasure(pdf *fpdf.Fpdf, values []string) ([][]string, float64) {
	lines := make([][]string, len(values))
	maxLines := 1
	for i, value := range values {
		lines[i] = pdf.SplitText(strings.ToValidUTF8(value, "�"), pdfWidths[i]-2*pdfCellInset)
		if len(lines[i]) == 0 {
			lines[i] = []string{""}
		}
		maxLines = max(maxLines, len(lines[i]))
	}
	return lines, float64(maxLines)*pdfLine + 2
}

func pdfDrawCells(pdf *fpdf.Fpdf, lines [][]string, height float64, header bool) {
	x, y := pdf.GetX(), pdf.GetY()
	for i, width := range pdfWidths {
		if header {
			pdf.SetFillColor(235, 239, 244)
			pdf.Rect(x, y, width, height, "DF")
		} else {
			pdf.Rect(x, y, width, height, "D")
		}
		for j, line := range lines[i] {
			pdf.Text(x+pdfCellInset, y+2.8+float64(j)*pdfLine, line)
		}
		x += width
	}
	pdf.SetXY(pdfMargin, y+height)
}
