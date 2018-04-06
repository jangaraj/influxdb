package storage

import (
	"context"
	"fmt"

	"github.com/influxdata/influxdb/tsdb"
)

func newMultiShardBatchCursor(ctx context.Context, row seriesRow, rr *readRequest) tsdb.Cursor {
	req := &tsdb.CursorRequest{
		Name:      row.name,
		Tags:      row.tags,
		Field:     row.field,
		Ascending: rr.asc,
		StartTime: rr.start,
		EndTime:   rr.end,
	}

	var shard tsdb.CursorIterator
	var cur tsdb.Cursor
	for cur == nil && len(row.query) > 0 {
		shard, row.query = row.query[0], row.query[1:]
		cur, _ = shard.Next(ctx, req)
	}

	if cur == nil {
		return nil
	}

	switch c := cur.(type) {
	case tsdb.IntegerBatchCursor:
		return newIntegerMultiShardBatchCursor(ctx, c, rr, req, row.query)
	case tsdb.FloatBatchCursor:
		return newFloatMultiShardBatchCursor(ctx, c, rr, req, row.query)
	case tsdb.UnsignedBatchCursor:
		return newUnsignedMultiShardBatchCursor(ctx, c, rr, req, row.query)
	case tsdb.StringBatchCursor:
		return newStringMultiShardBatchCursor(ctx, c, rr, req, row.query)
	case tsdb.BooleanBatchCursor:
		return newBooleanMultiShardBatchCursor(ctx, c, rr, req, row.query)
	default:
		panic(fmt.Sprintf("unreachable: %T", cur))
	}
}
