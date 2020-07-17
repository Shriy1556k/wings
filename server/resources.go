package server

import (
	"github.com/docker/docker/api/types"
	"math"
	"sync"
)

// Defines the current resource usage for a given server instance. If a server is offline you
// should obviously expect memory and CPU usage to be 0. However, disk will always be returned
// since that is not dependent on the server being running to collect that data.
type ResourceUsage struct {
	sync.RWMutex

	// The total amount of memory, in bytes, that this server instance is consuming. This is
	// calculated slightly differently than just using the raw Memory field that the stats
	// return from the container, so please check the code setting this value for how that
	// is calculated.
	Memory uint64 `json:"memory_bytes"`
	// The total amount of memory this container or resource can use. Inside Docker this is
	// going to be higher than you'd expect because we're automatically allocating overhead
	// abilities for the container, so its not going to be a perfect match.
	MemoryLimit uint64 `json:"memory_limit_bytes"`
	// The absolute CPU usage is the amount of CPU used in relation to the entire system and
	// does not take into account any limits on the server process itself.
	CpuAbsolute float64 `json:"cpu_absolute"`
	// The current disk space being used by the server. This is cached to prevent slow lookup
	// issues on frequent refreshes.
	Disk int64 `json:"disk_bytes"`
	// Current network transmit in & out for a container.
	Network struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"network"`
}

// Resets the usages values to zero, used when a server is stopped to ensure we don't hold
// onto any values incorrectly.
func (ru *ResourceUsage) Empty() {
	ru.Lock()
	defer ru.Unlock()

	ru.Memory = 0
	ru.CpuAbsolute = 0
	ru.Network.TxBytes = 0
	ru.Network.RxBytes = 0
}

// The "docker stats" CLI call does not return the same value as the types.MemoryStats.Usage
// value which can be rather confusing to people trying to compare panel usage to
// their stats output.
//
// This math is straight up lifted from their CLI repository in order to show the same
// values to avoid people bothering me about it. It should also reflect a slightly more
// correct memory value anyways.
//
// @see https://github.com/docker/cli/blob/96e1d1d6/cli/command/container/stats_helpers.go#L227-L249
func (ru *ResourceUsage) CalculateDockerMemory(stats types.MemoryStats) uint64 {
	if v, ok := stats.Stats["total_inactive_file"]; ok && v < stats.Usage {
		return stats.Usage - v
	}

	if v := stats.Stats["inactive_file"]; v < stats.Usage {
		return stats.Usage - v
	}

	return stats.Usage
}

// Calculates the absolute CPU usage used by the server process on the system, not constrained
// by the defined CPU limits on the container.
//
// @see https://github.com/docker/cli/blob/aa097cf1aa19099da70930460250797c8920b709/cli/command/container/stats_helpers.go#L166
func (ru *ResourceUsage) CalculateAbsoluteCpu(pStats *types.CPUStats, stats *types.CPUStats) float64 {
	// Calculate the change in CPU usage between the current and previous reading.
	cpuDelta := float64(stats.CPUUsage.TotalUsage) - float64(pStats.CPUUsage.TotalUsage)

	// Calculate the change for the entire system's CPU usage between current and previous reading.
	systemDelta := float64(stats.SystemUsage) - float64(pStats.SystemUsage)

	// Calculate the total number of CPU cores being used.
	cpus := float64(stats.OnlineCPUs)
	if cpus == 0.0 {
		cpus = float64(len(stats.CPUUsage.PercpuUsage))
	}

	percent := 0.0
	if systemDelta > 0.0 && cpuDelta > 0.0 {
		percent = (cpuDelta / systemDelta) * cpus * 100.0
	}

	return math.Round(percent*1000) / 1000
}
