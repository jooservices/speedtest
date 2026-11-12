package geo

import (
	"sort"
	"strings"
)

const QualityThreshold = 85

// QualityScore scores completeness, source and age on a 0-100 scale.
func QualityScore(location Location, source string, ageDays int) int {
	score := 0
	if location.Country != "" {
		score += 10
	}
	if location.City != "" {
		score += 15
	}
	if location.Latitude >= -90 && location.Latitude <= 90 && location.Longitude >= -180 && location.Longitude <= 180 &&
		(location.Latitude != 0 || location.Longitude != 0) {
		score += 20
	}
	if location.ASN > 0 {
		score += 15
	}
	if strings.TrimSpace(location.Organisation) != "" {
		score += 15
	}
	if location.AccuracyRadius > 0 {
		score += 10
	}
	if strings.TrimSpace(source) != "" {
		score += 5
	}
	score += max(0, 10-min(max(ageDays, 0)/18, 10))
	return min(score, 100)
}

// MergeLocations fills gaps and replaces a field only when its incoming source ranks higher.
func MergeLocations(existing Location, existingSource string, incoming Location, incomingSource string) (Location, string) {
	sources := mapsForMerge(existing, existingSource)
	incomingSources := mapsForMerge(incoming, incomingSource)
	mergeText(&existing.City, incoming.City, "city", sources, incomingSources["city"])
	mergeText(&existing.Region, incoming.Region, "region", sources, incomingSources["region"])
	mergeText(&existing.Country, incoming.Country, "country", sources, incomingSources["country"])
	mergeText(&existing.TimeZone, incoming.TimeZone, "time_zone", sources, incomingSources["time_zone"])
	mergeText(&existing.Organisation, incoming.Organisation, "organisation", sources, incomingSources["organisation"])
	mergeCoordinates(&existing, incoming, sources, incomingSources["coordinates"])
	mergeASN(&existing.ASN, incoming.ASN, sources, incomingSources["asn"])
	mergeRadius(&existing.AccuracyRadius, incoming.AccuracyRadius, sources, incomingSources["accuracy_radius"])
	existing.FieldSources = fieldSources(sources)
	source := mergedSource(sources, existingSource, incomingSource)
	existing.QualityScore = QualityScore(existing, source, existing.AgeDays)
	return existing, source
}

func mapsForMerge(location Location, fallback string) map[string]string {
	result := location.FieldSources.asMap()
	for _, field := range []struct {
		name string
		set  bool
	}{{"city", location.City != ""}, {"region", location.Region != ""}, {"country", location.Country != ""},
		{"time_zone", location.TimeZone != ""}, {"organisation", location.Organisation != ""}, {"asn", location.ASN > 0},
		{"coordinates", location.Latitude != 0 || location.Longitude != 0}, {"accuracy_radius", location.AccuracyRadius > 0}} {
		if field.set && result[field.name] == "" {
			result[field.name] = fallback
		}
	}
	return result
}

func sourcesFor(location Location, source string) FieldSources {
	return fieldSources(mapsForMerge(location, source))
}

func (s FieldSources) asMap() map[string]string {
	return map[string]string{"city": s.City, "region": s.Region, "country": s.Country, "time_zone": s.TimeZone,
		"organisation": s.Organisation, "coordinates": s.Coordinates, "asn": s.ASN, "accuracy_radius": s.AccuracyRadius}
}

func fieldSources(values map[string]string) FieldSources {
	return FieldSources{City: values["city"], Region: values["region"], Country: values["country"],
		TimeZone: values["time_zone"], Organisation: values["organisation"], Coordinates: values["coordinates"],
		ASN: values["asn"], AccuracyRadius: values["accuracy_radius"]}
}

func mergeText(target *string, incoming, field string, sources map[string]string, incomingSource string) {
	if incoming != "" && preferField(*target != "", sources[field], incomingSource) {
		*target = incoming
		sources[field] = incomingSource
	}
}

func mergeASN(target *uint, incoming uint, sources map[string]string, incomingSource string) {
	if incoming > 0 && preferField(*target > 0, sources["asn"], incomingSource) {
		*target = incoming
		sources["asn"] = incomingSource
	}
}

func mergeCoordinates(target *Location, incoming Location, sources map[string]string, incomingSource string) {
	if (incoming.Latitude != 0 || incoming.Longitude != 0) &&
		preferField(target.Latitude != 0 || target.Longitude != 0, sources["coordinates"], incomingSource) {
		target.Latitude, target.Longitude = incoming.Latitude, incoming.Longitude
		sources["coordinates"] = incomingSource
	}
}

func mergeRadius(target *uint16, incoming uint16, sources map[string]string, incomingSource string) {
	if incoming > 0 && (*target == 0 || incoming < *target) {
		*target = incoming
		sources["accuracy_radius"] = incomingSource
	}
}

func preferField(oldPresent bool, oldSource, incomingSource string) bool {
	return !oldPresent || sourceRank(incomingSource) > sourceRank(oldSource)
}

func sourceRank(source string) int {
	lower := strings.ToLower(source)
	switch {
	case strings.HasPrefix(lower, "consensus:"):
		return 5
	case strings.Contains(lower, "geolite"), strings.Contains(lower, "db-ip"):
		return 4
	case strings.Contains(lower, "ipinfo"):
		return 3
	case strings.Contains(lower, "ripe"):
		return 2
	default:
		return 1
	}
}

func mergedSource(sources map[string]string, existing, incoming string) string {
	if existing == "" && strings.HasPrefix(incoming, "consensus:") {
		return incoming
	}
	if len(sources) == 0 {
		return firstSource(existing, incoming)
	}
	unique := map[string]struct{}{}
	for _, source := range sources {
		if source != "" {
			unique[source] = struct{}{}
		}
	}
	if len(unique) == 0 {
		return firstSource(existing, incoming)
	}
	if len(unique) == 1 {
		for source := range unique {
			return source
		}
	}
	names := make([]string, 0, len(unique))
	for source := range unique {
		names = append(names, source)
	}
	sort.Strings(names)
	return "merged:" + strings.Join(names, "+")
}

func firstSource(first, second string) string {
	if first != "" {
		return first
	}
	return second
}
