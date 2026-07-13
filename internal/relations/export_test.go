package relations

// Size returns the number of (image_id, phash) entries currently in the
// tree. Test-only: production code never queries the count.
func (t *BKTree) Size() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.idIndex)
}
