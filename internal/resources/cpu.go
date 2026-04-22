package resources

import "piccolod/internal/api"

// CPUWeight values mapped from the priority tier enum. Values are
// compressed for 2–4 core target hardware: a high:background ratio of
// 16:1 gives high-priority apps meaningful preference under contention
// without starving background for multiple seconds at a time. The
// server-class 1000/100/10 ratio creates user-visible stalls on small
// core counts. See plans/resource-stewardship.md D-2.
const (
	CPUWeightHigh       = 400
	CPUWeightNormal     = 100 // cgroup v2 default; explicit for clarity
	CPUWeightBackground = 25
)

// PriorityToCPUWeight maps an app's declared priority tier to a cgroup v2
// CPUWeight value. Unset / unrecognized → normal.
func PriorityToCPUWeight(p api.ResourcePriority) int {
	switch p {
	case api.PriorityHigh:
		return CPUWeightHigh
	case api.PriorityBackground:
		return CPUWeightBackground
	default:
		return CPUWeightNormal
	}
}
