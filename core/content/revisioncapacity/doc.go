// Package revisioncapacity owns process-wide stable-revision and active-lease
// accounting. It coordinates pressure reclamation without ever holding its lock
// while invoking a store-owned reclaim target.
package revisioncapacity
