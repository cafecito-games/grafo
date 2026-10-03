package sqlite

// SetReconciliationBatchSizeForTest lowers the dirty-fact batch size for one
// test and restores it afterwards. Reconciling more than one batch is a
// behavior worth covering — the resolution cache and the test-edge source set
// are both held across batches — and at the production size covering it would
// mean generating a fact set the size of a real repository's.
//
// It mutates package state, so a caller must not run in parallel with a test
// that reconciles. No test in this package calls t.Parallel today; one that does
// has to leave this alone.
func SetReconciliationBatchSizeForTest(t interface{ Cleanup(func()) }, size int64) {
	previous := reconciliationBatchSize
	reconciliationBatchSize = size
	t.Cleanup(func() { reconciliationBatchSize = previous })
}
