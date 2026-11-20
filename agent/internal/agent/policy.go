package agent

import (
	"slices"

	"github.com/my-compute-cloud/mcc/agent/internal/config"
	"github.com/my-compute-cloud/mcc/agent/internal/protocol"
)

// policyReport is the owner's policy as the control plane is told it, with each default
// filled in so that the report says what the host does, not what its file leaves out.
//
// The control plane cannot change any of it, and the agent refuses what breaks it whatever
// the control plane believes. It is told so that it does not send work that would be
// refused, and so that the account can see what each host accepts.
//
// Left out on purpose: paths on the host (the cache directory, Xcode) and the commands and
// environment of declared tasks, which may hold what the owner would not want to leave the
// machine. The names of the tasks travel with the host's capacity.
func policyReport(p config.Policy) protocol.HostPolicy {
	workloads := slices.Clone(p.AllowedWorkloads)
	if len(workloads) == 0 {
		workloads = []string{workloadGitHubRunner}
	}
	cache := ""
	if p.CacheDir != "" {
		cache = p.CacheScope
		if cache == "" {
			cache = "runner"
		}
	}
	return protocol.HostPolicy{
		MaxVms:                  p.MaxVMs,
		AllowedImages:           slices.Clone(p.AllowedImages),
		ExcludedImages:          slices.Clone(p.ExcludedImages),
		AllowedGithubUrls:       slices.Clone(p.AllowedGitHubURLs),
		AllowedWorkloads:        workloads,
		ActiveHours:             p.ActiveHours,
		Cpus:                    p.CPUs,
		MemoryMb:                p.MemoryMB,
		DiskGb:                  p.DiskGB,
		IsolateNetwork:          p.NetworkIsolation(),
		RequireNetworkIsolation: p.RequireNetworkIsolation,
		SharedRunner:            p.SharedRunnerEnabled(),
		ImageRefreshHours:       p.ImageRefreshHours,
		Cache:                   cache,
		CacheMaxGb:              p.CacheMaxGB,
		ShareNixStore:           p.ShareNixStore,
		HostTasks:               p.HostTasks.Enabled,
		UpgradeWaitMinutes:      int(p.UpgradeWait().Minutes()),
	}
}
