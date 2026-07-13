package search

// AdjacencyCacheClear drops every entry so a test starts with a clean
// slate. Test-only: runtime callers invalidate per gallery via
// AdjacencyCacheDropForGallery.
func AdjacencyCacheClear() {
	adjCacheMu.Lock()
	defer adjCacheMu.Unlock()
	adjCacheEntries = make(map[string]adjacencyCacheEntry)
	adjCacheOrder = nil
}
