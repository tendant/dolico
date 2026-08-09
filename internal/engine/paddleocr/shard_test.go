package paddleocr

import (
	"reflect"
	"testing"
)

func TestShardSplitsIntoContiguousNearEqualChunks(t *testing.T) {
	cases := []struct {
		pages []int
		n     int
		want  [][]int
	}{
		{[]int{1, 2, 3, 4}, 2, [][]int{{1, 2}, {3, 4}}},
		// 5 pages over 2 shards: the remainder goes to the front.
		{[]int{1, 2, 3, 4, 5}, 2, [][]int{{1, 2, 3}, {4, 5}}},
		{[]int{1, 2, 3}, 4, [][]int{{1}, {2}, {3}}}, // never more shards than pages
		{[]int{1, 2, 3}, 1, [][]int{{1, 2, 3}}},
		{[]int{1, 2, 3}, 0, [][]int{{1, 2, 3}}}, // a nonsense count still works
		{[]int{7}, 4, [][]int{{7}}},
	}
	for _, tc := range cases {
		got := shard(tc.pages, tc.n)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("shard(%v, %d) = %v, want %v", tc.pages, tc.n, got, tc.want)
		}
		// Whatever the split, every page must appear exactly once and in order.
		var flat []int
		for _, chunk := range got {
			flat = append(flat, chunk...)
		}
		if !reflect.DeepEqual(flat, tc.pages) {
			t.Errorf("shard(%v, %d) lost or reordered pages: %v", tc.pages, tc.n, flat)
		}
	}
}

// Pages reach here already sorted, but a shard must never silently drop one
// even if that changes.
func TestShardPreservesEveryPage(t *testing.T) {
	pages := make([]int, 37)
	for i := range pages {
		pages[i] = i + 1
	}
	for n := 1; n <= 12; n++ {
		seen := map[int]int{}
		for _, chunk := range shard(pages, n) {
			for _, p := range chunk {
				seen[p]++
			}
		}
		if len(seen) != len(pages) {
			t.Fatalf("n=%d covered %d of %d pages", n, len(seen), len(pages))
		}
		for p, count := range seen {
			if count != 1 {
				t.Fatalf("n=%d saw page %d %d times", n, p, count)
			}
		}
	}
}
