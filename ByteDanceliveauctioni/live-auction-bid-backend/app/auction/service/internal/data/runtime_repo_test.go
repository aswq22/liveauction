package data

import (
	"reflect"
	"testing"
)

func TestRedisHashPairsOrdersRuntimeStateFields(t *testing.T) {
	state := map[string]any{
		"room_id":            "room-1",
		"lot_id":             "lot-1",
		"started_at_unix_ms": int64(123),
	}

	got := redisHashPairs(state)

	want := []any{
		"lot_id", "lot-1",
		"room_id", "room-1",
		"started_at_unix_ms", int64(123),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("redisHashPairs() = %#v, want %#v", got, want)
	}
}
