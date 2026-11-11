package geo

import "testing"

func TestQualityScoreCompletenessAndAge(t *testing.T) {
	complete := Location{City: "London", Region: "England", Country: "GB", Latitude: 51.5, Longitude: -0.1,
		TimeZone: "Europe/London", AccuracyRadius: 5, ASN: 64500, Organisation: "Example"}
	if score := QualityScore(complete, "geolite2", 0); score != 100 {
		t.Fatalf("complete location score = %d, want 100", score)
	}
	if score := QualityScore(complete, "geolite2", 180); score != 90 {
		t.Fatalf("aged location score = %d, want 90", score)
	}
	if score := QualityScore(Location{}, "", 365); score != 0 {
		t.Fatalf("empty location score = %d, want 0", score)
	}
}

func TestMergeLocationsRanksSourcesAndFillsMissingFields(t *testing.T) {
	for source, want := range map[string]int{"ipwho": 1, "ripestat": 2, "ipinfo-lite": 3, "geolite2": 4, "consensus:ipwho": 5} {
		if got := sourceRank(source); got != want {
			t.Errorf("sourceRank(%q) = %d, want %d", source, got, want)
		}
	}
	existing := Location{City: "Old City", Country: "US", ASN: 64500, FieldSources: FieldSources{City: "ipwho", Country: "ipwho", ASN: "ipwho"}}
	merged, source := MergeLocations(existing, "ipwho", Location{City: "New City", Country: "GB", Region: "England", ASN: 15169}, "geolite2")
	if merged.City != "New City" || merged.Country != "GB" || merged.Region != "England" || merged.ASN != 15169 ||
		merged.FieldSources.Region != "geolite2" || merged.FieldSources.City != "geolite2" || source != "geolite2" {
		t.Fatalf("higher-ranked source was not merged: %+v source=%q", merged, source)
	}
	kept, _ := MergeLocations(merged, source, Location{City: "Poorer City", Country: "CA"}, "ipwho")
	if kept.City != "New City" || kept.Country != "GB" {
		t.Fatalf("poorer source overwrote fields: %+v", kept)
	}
}

func TestMergedSourceAndFieldSourceFallbacks(t *testing.T) {
	if got := mergedSource(nil, "old", "new"); got != "old" {
		t.Fatalf("empty source map = %q", got)
	}
	if got := firstSource("first", "second"); got != "first" {
		t.Fatalf("firstSource = %q", got)
	}
	location := Location{Country: "US", ASN: 64500}
	if got := sourcesFor(location, "ipinfo-lite"); got.Country != "ipinfo-lite" || got.ASN != "ipinfo-lite" {
		t.Fatalf("inferred sources = %+v", got)
	}
	merged, source := MergeLocations(Location{}, "", Location{}, "")
	if source != "" || merged.QualityScore != 10 {
		t.Fatalf("empty merge = %+v source=%q", merged, source)
	}
}

func TestMergeLocationsKeepsMostAccurateRadiusAndItsProvenance(t *testing.T) {
	for _, tc := range []struct {
		name           string
		existing       uint16
		existingSource string
		incoming       uint16
		incomingSource string
		want           uint16
		wantSource     string
	}{
		{name: "higher-ranked source has worse radius", existing: 5, existingSource: "ipwho", incoming: 500, incomingSource: "geolite2", want: 5, wantSource: "ipwho"},
		{name: "lower-ranked source has better radius", existing: 500, existingSource: "geolite2", incoming: 5, incomingSource: "ipwho", want: 5, wantSource: "ipwho"},
		{name: "existing radius is more accurate", existing: 10, existingSource: "ipwho", incoming: 20, incomingSource: "freeipapi", want: 10, wantSource: "ipwho"},
		{name: "empty radius accepts incoming", incoming: 20, incomingSource: "freeipapi", want: 20, wantSource: "freeipapi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := MergeLocations(Location{AccuracyRadius: tc.existing, FieldSources: FieldSources{AccuracyRadius: tc.existingSource}}, tc.existingSource,
				Location{AccuracyRadius: tc.incoming}, tc.incomingSource)
			if got.AccuracyRadius != tc.want || got.FieldSources.AccuracyRadius != tc.wantSource {
				t.Fatalf("radius merge = %d from %q, want %d from %q", got.AccuracyRadius, got.FieldSources.AccuracyRadius, tc.want, tc.wantSource)
			}
		})
	}
}

func TestMergeLocationsUsesIncomingFieldProvenance(t *testing.T) {
	incoming := Location{City: "London", Country: "GB", ASN: 174,
		FieldSources: FieldSources{City: "geolite2", Country: "ipinfo-lite-mmdb", ASN: "ipinfo-lite-mmdb"}}
	merged, source := MergeLocations(Location{}, "", incoming, "geolite2")
	if merged.FieldSources.City != "geolite2" || merged.FieldSources.Country != "ipinfo-lite-mmdb" ||
		merged.FieldSources.ASN != "ipinfo-lite-mmdb" || source != "merged:geolite2+ipinfo-lite-mmdb" {
		t.Fatalf("mixed offline provenance was collapsed: %+v source=%q", merged, source)
	}
}
