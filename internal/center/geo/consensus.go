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

// pickConsensus chooses the country most providers report (ties go to the earliest provider), merges the agreeing
// answers (the first non-empty value of each field wins) and names the sources. The third result lists the
// competing country votes when providers disagree, empty otherwise.
func pickConsensus(answers []sourced) (Location, string, string) {
	votes := map[string]int{}
	order := []string{}
	for _, a := range answers {
		c := strings.ToUpper(a.location.Country)
		if _, seen := votes[c]; !seen {
			order = append(order, c)
		}
		votes[c]++
	}
	winner := order[0]
	for _, c := range order {
		if votes[c] > votes[winner] {
			winner = c
		}
	}
	var merged Location
	var names []string
	for _, a := range answers {
		if strings.ToUpper(a.location.Country) != winner {
			continue
		}
		names = append(names, a.source)
		merged = fill(merged, a.location)
	}
	source := names[0]
	if len(names) > 1 {
		source = "consensus:" + strings.Join(names, "+")
	}
	disagreement := ""
	if len(order) > 1 {
		parts := make([]string, 0, len(order))
		for _, c := range order {
			label := c
			if label == "" {
				label = "none"
			}
			parts = append(parts, label)
		}
		disagreement = strings.Join(parts, ",")
	}
	return merged, source, disagreement
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
