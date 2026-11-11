// Package isp models Internet Service Providers from ASN/organisation data.
package isp

import (
	"regexp"
	"strings"
)

var suffix = regexp.MustCompile(`(?i)[\s,]+(group|corp\.?|corporation|inc\.?|incorporated|ltd\.?|limited|llc|co\.?|company|plc|gmbh|s\.?a\.?|bv|ag)\.?$`)

// NormaliseName turns an ASN organisation string into a short display name.
// ASN remains the unique identity; the name is only for humans. Original casing
// of the remaining words is preserved so acronyms such as VNPT stay intact.
func NormaliseName(organisation string) string {
	name := strings.Join(strings.Fields(strings.TrimSpace(organisation)), " ")
	if name == "" {
		return ""
	}
	for {
		next := strings.TrimSpace(suffix.ReplaceAllString(name, ""))
		if next == name || next == "" {
			break
		}
		name = next
	}
	return name
}
