// Package layout is the sole authority for packed-stream geometry.
//
// Geometry validation bounds chunk size, stream length, and dense chunk-state count
// before allocation. Layout maps authenticated manifest order to file ranges, while
// ChunkSet represents selection as normalized immutable half-open intervals so full
// shares and unions never require eager chunk-number slices.
package layout
