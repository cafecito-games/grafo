package sqlcfixture

import (
	"context"

	"example.com/sqlcfixture/internal/db"
)

func Load(ctx context.Context, queries *db.Queries, id int64) (db.User, error) {
	return queries.GetUser(ctx, id)
}
