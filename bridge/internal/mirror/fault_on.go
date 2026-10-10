//go:build faultinject

package mirror

// faultExpungeOrder switches on a deliberately wrong order of removals in
// applyDeltaLocked. It exists only to show that the random test finds this
// kind of mistake (test/fuzz-mutation-check.sh); it is never part of a normal
// build.
const faultExpungeOrder = true
