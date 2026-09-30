//go:build race

package live

// raceEnabled is true when the binary is built with the race detector
// (-race sets the `race` build constraint). The soak uses it to relax
// the wall-clock and memory budgets, which `-race` inflates past their
// NFR thresholds; the plain run still enforces NFR-1/NFR-8 exactly.
const raceEnabled = true
