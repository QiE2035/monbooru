package web

import (
	"testing"
	"time"

	"github.com/leqwin/monbooru/internal/models"
	"github.com/leqwin/monbooru/internal/search"
)

func TestComputeInboxClusters_Empty(t *testing.T) {
	if got := computeInboxClusters(nil, ""); got != nil {
		t.Fatalf("empty input: got %v, want nil", got)
	}
}

func TestComputeInboxClusters_Singleton(t *testing.T) {
	base := time.Date(2026, 5, 22, 14, 32, 0, 0, time.UTC)
	imgs := []models.Image{{ID: 1, IngestedAt: base}}
	got := computeInboxClusters(imgs, "")
	if len(got) != 1 || got[0] == nil {
		t.Fatalf("singleton: got %v, want one cluster marker", got)
	}
	if got[0].Count != 1 || got[0].DateLabel != "2026-05-22" {
		t.Errorf("singleton header = %+v", got[0])
	}
}

func TestComputeInboxClusters_ExactBoundary(t *testing.T) {
	// Two rows exactly batchGapMinutes apart open a new cluster
	// (the boundary is >= gap). Bump by one second under the gap and
	// they stay in one cluster.
	base := time.Date(2026, 5, 22, 14, 32, 0, 0, time.UTC)
	t.Run("equal-to-gap", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Duration(batchGapMinutes) * time.Minute)},
			{ID: 1, IngestedAt: base},
		}
		got := computeInboxClusters(imgs, "")
		if got[0] == nil || got[1] == nil {
			t.Fatalf("equal-to-gap: expected two clusters, got %+v / %+v", got[0], got[1])
		}
		if got[0].Count != 1 || got[1].Count != 1 {
			t.Errorf("counts = %d, %d; want 1, 1", got[0].Count, got[1].Count)
		}
	})
	t.Run("one-second-under-gap", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Duration(batchGapMinutes)*time.Minute - time.Second)},
			{ID: 1, IngestedAt: base},
		}
		got := computeInboxClusters(imgs, "")
		if got[0] == nil || got[1] != nil {
			t.Fatalf("one-second-under-gap: expected single cluster, got %+v / %+v", got[0], got[1])
		}
		if got[0].Count != 2 {
			t.Errorf("count = %d, want 2", got[0].Count)
		}
	})
}

func TestComputeInboxClusters_MixedSizes(t *testing.T) {
	// Three clusters of size 3 / 1 / 2 in newest-DESC order. The
	// first cluster runs from 14:36 down to 14:32 (every minute);
	// the second is alone at 09:14; the third is two rows at 22:08
	// and 22:20 from the previous day.
	parse := func(s string) time.Time {
		t.Helper()
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	imgs := []models.Image{
		{ID: 1, IngestedAt: parse("2026-05-22T14:36:00Z")},
		{ID: 2, IngestedAt: parse("2026-05-22T14:34:00Z")},
		{ID: 3, IngestedAt: parse("2026-05-22T14:32:00Z")},
		{ID: 4, IngestedAt: parse("2026-05-22T09:14:00Z")},
		{ID: 5, IngestedAt: parse("2026-05-21T22:20:00Z")},
		{ID: 6, IngestedAt: parse("2026-05-21T22:08:00Z")},
	}
	got := computeInboxClusters(imgs, "inbox:true")
	wantStarts := map[int]int{0: 3, 3: 1, 4: 2} // idx -> count
	for i, c := range got {
		if want, ok := wantStarts[i]; ok {
			if c == nil {
				t.Errorf("idx %d: expected header marker, got nil", i)
				continue
			}
			if c.Count != want {
				t.Errorf("idx %d count = %d, want %d", i, c.Count, want)
			}
		} else if c != nil {
			t.Errorf("idx %d: expected nil, got %+v", i, c)
		}
	}
	if got[0] != nil && got[0].RangeLabel != "14:32 -> 14:36" {
		t.Errorf("cluster 0 range = %q", got[0].RangeLabel)
	}
	if got[4] != nil && got[4].RangeLabel != "22:08 -> 22:20" {
		t.Errorf("cluster 2 range = %q", got[4].RangeLabel)
	}
}

func TestComputeInboxClusters_UploadBatch(t *testing.T) {
	base := time.Date(2026, 5, 22, 14, 0, 0, 0, time.UTC)
	batch := func(v int64) *int64 { return &v }

	t.Run("different batches within the gap split", func(t *testing.T) {
		// Two drops a minute apart - well under batchGapMinutes - still
		// form two clusters because their tokens differ.
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Minute), UploadBatch: batch(200)},
			{ID: 1, IngestedAt: base, UploadBatch: batch(100)},
		}
		got := computeInboxClusters(imgs, "inbox:true")
		if got[0] == nil || got[1] == nil {
			t.Fatalf("expected two clusters, got %+v / %+v", got[0], got[1])
		}
	})

	t.Run("same batch beyond the gap stays one", func(t *testing.T) {
		// One drop's rows stay a single cluster even past the gap: the
		// shared token overrides the time rule.
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(30 * time.Minute), UploadBatch: batch(100)},
			{ID: 1, IngestedAt: base, UploadBatch: batch(100)},
		}
		got := computeInboxClusters(imgs, "inbox:true")
		if got[0] == nil || got[1] != nil {
			t.Fatalf("expected one cluster, got %+v / %+v", got[0], got[1])
		}
		if got[0].Count != 2 {
			t.Errorf("count = %d, want 2", got[0].Count)
		}
	})

	t.Run("upload row adjacent to a watcher row splits", func(t *testing.T) {
		// An upload batch meeting a tokenless watcher/sync row breaks even
		// within the gap.
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Minute), UploadBatch: batch(100)},
			{ID: 1, IngestedAt: base},
		}
		got := computeInboxClusters(imgs, "inbox:true")
		if got[0] == nil || got[1] == nil {
			t.Fatalf("expected two clusters, got %+v / %+v", got[0], got[1])
		}
	})

	t.Run("tokenless rows still use the time gap", func(t *testing.T) {
		imgs := []models.Image{
			{ID: 2, IngestedAt: base.Add(time.Minute)},
			{ID: 1, IngestedAt: base},
		}
		got := computeInboxClusters(imgs, "inbox:true")
		if got[0] == nil || got[1] != nil {
			t.Fatalf("expected one cluster, got %+v / %+v", got[0], got[1])
		}
	})
}

func TestInboxClustersActive(t *testing.T) {
	cases := []struct {
		name  string
		q     string
		sort  string
		order string
		want  bool
	}{
		{"newest desc + inbox:true", "inbox:true", "newest", "desc", true},
		{"newest desc + inbox:true with extra leaf", "inbox:true cat:character", "newest", "desc", true},
		{"newest asc + inbox:true", "inbox:true", "newest", "asc", false},
		{"filesize + inbox:true", "inbox:true", "filesize", "desc", false},
		{"negated inbox", "-inbox:true", "newest", "desc", false},
		{"inbox:false", "inbox:false", "newest", "desc", false},
		{"empty query", "", "newest", "desc", false},
	}
	for _, c := range cases {
		expr, _ := search.Parse(c.q)
		if got := inboxClustersActive(c.sort, c.order, expr); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestInboxFilterActive(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"bare inbox:true", "inbox:true", true},
		{"inbox:true with extra leaf", "inbox:true cat:character", true},
		{"extra leaf first", "1girl inbox:true", true},
		{"negated inbox", "-inbox:true", false},
		{"inbox:false", "inbox:false", false},
		{"inbox under OR", "inbox:true OR fav:true", false},
		{"empty query", "", false},
		{"unrelated query", "1girl", false},
	}
	for _, c := range cases {
		expr, _ := search.Parse(c.q)
		if got := inboxFilterActive(expr); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCollectionFilterActive(t *testing.T) {
	cases := []struct {
		name string
		q    string
		want bool
	}{
		{"bare collection", `collection:"my comic"`, true},
		{"collection with extra leaf", `collection:"my comic" 1girl`, true},
		{"extra leaf first", `1girl collection:"my comic"`, true},
		{"negated collection", `-collection:"my comic"`, false},
		{"collection under OR", `collection:"my comic" OR fav:true`, false},
		{"empty value", "collection:", false},
		{"empty query", "", false},
		{"unrelated query", "1girl", false},
	}
	for _, c := range cases {
		expr, _ := search.Parse(c.q)
		if got := collectionFilterActive(expr); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSearchWarning(t *testing.T) {
	cases := []struct {
		name, query, want string
	}{
		{"clean", "type:image 1girl", ""},
		{"unknown type", "type:video", "Unknown filter value: type:video"},
		{"negated unknown", "-mime:zzzz", "Unknown filter value: mime:zzzz"},
		{"two unknown", "type:video fav:maybe", "Unknown filter value: type:video, fav:maybe"},
		{"valid union", "type:image,archive", ""},
		{"bad union element", "type:image,video", "Unknown filter value: type:image,video"},
		{"open key untouched", "name:video source:booru", ""},
	}
	for _, c := range cases {
		expr, _ := search.Parse(c.query)
		if got := searchWarning(expr); got != c.want {
			t.Errorf("%s: searchWarning(%q) = %q, want %q", c.name, c.query, got, c.want)
		}
	}
}
