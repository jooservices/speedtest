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
	index := 0
	for _, match := range parameter.FindAllStringSubmatch(sql, -1) {
		n, _ := strconv.Atoi(match[1])
		if n > index {
			index = n
		}
	}
	predicate := fmt.Sprintf("r.node_id = n.id AND r.engine = COALESCE(NULLIF($%d::text,''),n.primary_engine)", index+1)
	sql = strings.ReplaceAll(sql, "r.node_id = n.id", predicate)
	return s.DB.Query(ctx, sql, append(args, q.Engine)...)
}
