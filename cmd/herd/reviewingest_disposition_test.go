package main

// Polarity-independent replay is covered by the shipped CLI regression
// TestFAC763LiveAdmissionAckFailureIsStructuredAndRecoverable. Enqueued=false
// also names a fresh FAIL/BLOCKED, so a bool-only disposition test cannot prove
// the duplicate-suppression contract.
