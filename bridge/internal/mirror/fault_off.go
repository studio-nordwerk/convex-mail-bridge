//go:build !faultinject

package mirror

// faultExpungeOrder is false in every normal build. See fault_on.go.
const faultExpungeOrder = false
