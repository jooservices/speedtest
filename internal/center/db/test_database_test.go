package db

import (
	"strings"
	"testing"
)

func TestCheckTestDatabase(t *testing.T) {
	accepted := []string{"speedtest_test", "speedtest_wt0", "speedtest_wta", "speedtest_wt51", "speedtest_wtabc012xyz789", "wt_joost_94", "wt_ab", "wt_a0123456789012345678901234567890123"}
	for _, name := range accepted {
		t.Run("accept/"+name, func(t *testing.T) {
			if err := CheckTestDatabase(name); err != nil {
				t.Fatalf("valid test database rejected: %v", err)
			}
		})
	}
	refused := []string{"", "wt_", "wt_a", "wt_A1", "wt_a-b", "wt_a01234567890123456789012345678901234", "xwt_ab", "wt_ab ", "speedtest", "other_test", "other_wt51", "speedtest_wt", "speedtest_wt1234567890123", "speedtest_wtA", "speedtest_wtaB", "speedtest_wt-1", "speedtest_wt_1", "speedtest_wté", "speedtest_test1", "prefix_speedtest_wt51", "speedtest_wt51_suffix", " speedtest_wt51", "speedtest_wt51 ", "speedtest_wt51\n", "speedtest_wt51\x00"}
	for _, name := range refused {
		t.Run("refuse/"+name, func(t *testing.T) {
			err := CheckTestDatabase(name)
			if err == nil {
				t.Fatal("unsafe database accepted")
			}
			if strings.Contains(err.Error(), name) && name != "" {
				t.Fatal("error includes untrusted database name")
			}
		})
	}
}
