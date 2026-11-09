package report

import (
	"bytes"
	"fmt"
	"strings"
)

const (
	pdfWidth, pdfHeight = 842, 595 // A4 landscape in points
	pdfMargin           = 36
	pdfLine             = 13
)

// pdfEscape keeps a string inside the standard Helvetica WinAnsi set and escapes the PDF string delimiters.
func pdfEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\\' || r == '(' || r == ')':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r >= 32 && r < 127:
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	return b.String()
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "~"
	}
	return s
}

func pdfRows(r Report) []string {
	lines := []string{fmt.Sprintf("%-22s %-16s %-14s %6s %5s %8s %8s %8s %-9s %5s %4s", "ISP", "Node", "Plan", "Tests", "Fail", "AvgDown", "P95Down", "Deliv%", "Worst UTC", "Mins", "IPs")}
	for _, row := range r.Rows {
		lines = append(lines, fmt.Sprintf("%-22s %-16s %-14s %6d %5d %8s %8s %8s %-9s %5d %4d",
			clip(row.ISP, 22), clip(row.Node, 16), clip(planName(row.Plan), 14), row.Tests, row.Failures,
			num(row.AvgDown), num(row.P95Down), num(row.DeliveredPct), hours(row.WorstHours), row.DowntimeMin, row.IPChanges))
	}
	return lines
}

// PDF renders the report as a small paginated PDF using only the built-in Helvetica font (no dependencies). The table
// uses Courier so columns line up.
func PDF(r Report) []byte {
	title := fmt.Sprintf("Monthly ISP and plan report %s (UTC)%s - engine: %s", r.Month, map[bool]string{true: " - partial month", false: ""}[r.Partial], engineName(r.Engine))
	body := pdfRows(r)
	if len(r.Rows) == 0 {
		body = append(body, "", "No tests with a known ISP in this month.")
	}
	perPage := (pdfHeight - 2*pdfMargin - 40) / pdfLine
	var pages [][]string
	for len(body) > 0 {
		n := min(perPage, len(body))
		pages = append(pages, body[:n])
		body = body[n:]
	}
	var objs []string
	objs = append(objs, "<< /Type /Catalog /Pages 2 0 R >>")
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 5+2*i)
	}
	objs = append(objs, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)))
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Courier /Encoding /WinAnsiEncoding >>")
	objs = append(objs, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica-Bold /Encoding /WinAnsiEncoding >>")
	for i, lines := range pages {
		var c strings.Builder
		y := pdfHeight - pdfMargin
		fmt.Fprintf(&c, "BT /F2 12 Tf %d %d Td (%s) Tj ET\n", pdfMargin, y, pdfEscape(title))
		y -= 28
		fmt.Fprintf(&c, "BT /F1 7 Tf %d %d Td %d TL\n", pdfMargin, y, pdfLine)
		for _, l := range lines {
			fmt.Fprintf(&c, "(%s) Tj T*\n", pdfEscape(l))
		}
		fmt.Fprintf(&c, "ET\nBT /F1 7 Tf %d %d Td (Page %d of %d) Tj ET\n", pdfWidth-pdfMargin-60, pdfMargin-12, i+1, len(pages))
		objs = append(objs, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 %d %d] /Resources << /Font << /F1 3 0 R /F2 4 0 R >> >> /Contents %d 0 R >>", pdfWidth, pdfHeight, 6+2*i))
		objs = append(objs, fmt.Sprintf("<< /Length %d >>\nstream\n%sendstream", c.Len(), c.String()))
	}
	return assemble(objs)
}

func engineName(e string) string {
	if e == "" {
		return "primary"
	}
	return e
}

func assemble(objs []string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = out.Len()
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&out, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return out.Bytes()
}
