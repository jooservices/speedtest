package geo

import (
	"context"
	"errors"
	"log/slog"
	"strings"
)

// WithConsensus makes the resolver ask every configured fallback provider and keep the answer most of them agree
// on (by country), filling gaps from the agreeing providers. Disagreement is logged as a warning without the IP.
// A nil logger uses the default logger. It returns the resolver for chaining.
func (r *Resolver) WithConsensus(on bool, logger *slog.Logger) *Resolver {
	r.consensus = on
	if on {
		r.highValue = true
	}
	if logger == nil {
		logger = slog.Default()
	}
	r.log = logger
	return r
}

type sourced struct {
	source   string
	location Location
}

func (r *Resolver) lookupConsensus(ctx context.Context, ip string) (Location, error) {
	var answers []sourced
	var failures []error
	budgetExhausted := false
	for _, provider := range r.fallbacks {
		if provider.Source() == "ipgeolocation" && cheapProvidersAgree(answers) {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Location{}, err
		}
		location, err := lookupFallback(ctx, provider, ip, r.purpose)
		switch {
		case err == nil:
			answers = append(answers, sourced{provider.Source(), location})
		case errors.Is(err, ErrNotFound):
		case errors.Is(err, ErrBudgetExhausted):
			budgetExhausted = true
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return Location{}, err
		default:
			failures = append(failures, err)
		}
	}
	if len(answers) == 0 {
		if budgetExhausted {
			return Location{}, ErrBudgetExhausted
		}
		if len(failures) != 0 {
			return Location{}, errors.Join(append([]error{ErrNotFound}, failures...)...)
		}
		return Location{}, ErrNotFound
	}
	location, source, disagreement := pickConsensus(answers)
	if disagreement != "" && r.log != nil {
		r.log.Warn("geolocation providers disagree", "countries", disagreement)
	}
	return r.store(ctx, ip, location, source)
}

func cheapProvidersAgree(answers []sourced) bool {
	country, count := "", 0
	for _, answer := range answers {
		value := strings.ToUpper(answer.location.Country)
		if value == "" {
			continue
		}
		if country != "" && country != value {
			return false
		}
		country = value
		count++
	}
	return count >= 2
}

// pickConsensus chooses the country most providers report (ties go to the earliest provider), merges the agreeing
// answers (the first non-empty value of each field wins) and names the sources. The third result lists the
// competing country votes when providers disagree, empty otherwise.
func pickConsensus(answers []sourced) (Location, string, string) {
	winner, disagreement := countryConsensus(answers)
	var merged Location
	var names []string
	for _, a := range answers {
		country := strings.ToUpper(a.location.Country)
		if country == "" {
			asn := Location{ASN: a.location.ASN, Organisation: a.location.Organisation,
				FieldSources: FieldSources{ASN: a.location.FieldSources.ASN, Organisation: a.location.FieldSources.Organisation}}
			var contributed bool
			merged, contributed = fillSourced(merged, asn, a.source)
			if contributed {
				names = append(names, a.source)
			}
			continue
		}
		if country != winner {
			continue
		}
		var contributed bool
		merged, contributed = fillSourced(merged, a.location, a.source)
		if contributed {
			names = append(names, a.source)
		}
	}
	if len(names) == 0 {
		names = append(names, answers[0].source)
	}
	source := names[0]
	if len(names) > 1 {
		source = "consensus:" + strings.Join(names, "+")
	}
	return merged, source, disagreement
}

func countryConsensus(answers []sourced) (string, string) {
	votes := map[string]int{}
	order := []string{}
	for _, a := range answers {
		c := strings.ToUpper(a.location.Country)
		if c == "" {
			continue
		}
		if _, seen := votes[c]; !seen {
			order = append(order, c)
		}
		votes[c]++
	}
	winner := ""
	for _, c := range order {
		if winner == "" || votes[c] > votes[winner] {
			winner = c
		}
	}
	disagreement := ""
	if len(order) > 1 {
		disagreement = strings.Join(order, ",")
	}
	return winner, disagreement
}

func fillSourced(base, other Location, fallbackSource string) (Location, bool) {
	before := base
	incomingSources := mapsForMerge(other, fallbackSource)
	base = fill(base, other)
	sources := base.FieldSources.asMap()
	for _, field := range []struct {
		name string
		was  bool
		now  bool
	}{
		{"city", before.City != "", base.City != ""},
		{"region", before.Region != "", base.Region != ""},
		{"country", before.Country != "", base.Country != ""},
		{"time_zone", before.TimeZone != "", base.TimeZone != ""},
		{"organisation", before.Organisation != "", base.Organisation != ""},
		{"coordinates", before.Latitude != 0 || before.Longitude != 0, base.Latitude != 0 || base.Longitude != 0},
		{"asn", before.ASN > 0, base.ASN > 0},
		{"accuracy_radius", before.AccuracyRadius > 0, base.AccuracyRadius > 0},
	} {
		if !field.was && field.now {
			sources[field.name] = incomingSources[field.name]
		}
	}
	base.FieldSources = fieldSources(sources)
	return base, base != before
}

// fill returns base with every empty field taken from other.
func fill(base, other Location) Location {
	if base.City == "" {
		base.City = other.City
	}
	if base.Region == "" {
		base.Region = other.Region
	}
	if base.Country == "" {
		base.Country = other.Country
	}
	if base.Latitude == 0 && base.Longitude == 0 {
		base.Latitude, base.Longitude = other.Latitude, other.Longitude
	}
	if base.TimeZone == "" {
		base.TimeZone = other.TimeZone
	}
	if base.AccuracyRadius == 0 {
		base.AccuracyRadius = other.AccuracyRadius
	}
	if base.ASN == 0 {
		base.ASN = other.ASN
	}
	if base.Organisation == "" {
		base.Organisation = other.Organisation
	}
	return base
}
