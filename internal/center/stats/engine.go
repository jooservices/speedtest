package stats

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

var parameter = regexp.MustCompile(`\$(\d+)`)

// query scopes every results join, including outer joins, before aggregation.
func (s *Store) query(ctx context.Context, q Query, sql string, args ...any) (pgx.Rows, error) {
	sql, args = scopedQuery(q, sql, args...)
	return s.DB.Query(ctx, sql, args...)
}

func scopedQuery(q Query, sql string, args ...any) (string, []any) {
	index := 0
	for _, match := range parameter.FindAllStringSubmatch(sql, -1) {
		n, _ := strconv.Atoi(match[1])
		if n > index {
			index = n
		}
	}
	predicate := fmt.Sprintf("r.node_id = n.id AND r.engine = COALESCE(NULLIF($%d::text,''),n.primary_engine)", index+1)
	args = append(args, q.Engine)
	index++
	predicate += fmt.Sprintf(" AND ($%d::text = '' OR lower(btrim(%s)) = lower($%d::text))", index+1, effectiveISP("r"), index+1)
	args = append(args, q.ISP)
	index++
	if q.ISPsSet {
		predicate += fmt.Sprintf(" AND r.isp_id = ANY($%d::uuid[])", index+1)
		args = append(args, q.ISPIDs)
	}
	sql = strings.ReplaceAll(sql, "r.node_id = n.id", predicate)
	return sql, args
}

func effectiveISP(result string) string {
	return "COALESCE(NULLIF(btrim(" + result + ".engine_isp),''), NULLIF(btrim((SELECT geo->>'organisation' FROM ip_addresses WHERE ip = " + result + ".ip)),''), '')"
}
