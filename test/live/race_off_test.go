//go:build !race

package live

// raceEnabled is false for ordinary builds; the soak then enforces the
// binding NFR-1/NFR-8 budgets verbatim.
const raceEnabled = false
