//go:build !localedge

package edge

// PlainHTTPAnyHost: the bridge as it is built for operation speaks plain
// http to this machine only (loopback). The shared secret and the device
// passwords travel on that link.
const PlainHTTPAnyHost = false
