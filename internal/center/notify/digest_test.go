package notify

import (
	"strings"
	"testing"
)

func TestAC1DigestTelegramChunksPreserveEscapedUnicodeWithinProviderBound(t *testing.T) {
	for name, summary := range map[string]string{
		"node lines":              strings.Repeat("界&😀<>\"x\n", 700),
		"very long line":          strings.Repeat("😀&", 2000),
		"prefix before long line": "prefix\n" + strings.Repeat("😀&", 2000),
	} {
		t.Run(name, func(t *testing.T) {
			chunks := splitDigestTelegramSummary(summary)
			if len(chunks) < 2 {
				t.Fatalf("chunk count = %d, want multiple chunks", len(chunks))
			}
			if got := strings.Join(chunks, ""); got != summary {
				t.Fatal("joining Telegram chunks did not restore the original summary")
			}
			for index, chunk := range chunks {
				if units := telegramEscapedTextUnits(chunk); units > maxDigestChunkTextUnits {
					t.Errorf("chunk %d has %d escaped UTF-16 units, want at most %d", index+1, units, maxDigestChunkTextUnits)
				}
			}
		})
	}
	if got := telegramEscapedTextUnits("&<>\"😀"); got != 21 {
		t.Errorf("escaped UTF-16 units = %d, want 21", got)
	}
}

func TestAC1DigestTelegramChunksRepresentEmptySummary(t *testing.T) {
	chunks := splitDigestTelegramSummary("")
	if len(chunks) != 1 || chunks[0] != "" {
		t.Fatalf("empty summary chunks = %#v, want one empty part", chunks)
	}
}
