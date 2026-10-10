//go:build localedge

package edge

// PlainHTTPAnyHost: built with -tags localedge, the bridge also speaks
// plain http to other hosts. This is for test/against-edge.sh alone, where
// the bridge runs in Docker and the edge is a local Convex on the machine
// (http://host.docker.internal:...). The image for operation is never built
// with this tag, and there is no setting that switches it on.
const PlainHTTPAnyHost = true
