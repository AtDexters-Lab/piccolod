package app

import (
	"fmt"
	"sort"
	"strings"

	"piccolod/internal/api"
)

// serviceStartOrder returns a deterministic start order for a set of services based on their `after` edges.
// The returned order is topological (dependencies first) with a lexicographic tiebreaker.
func serviceStartOrder(services map[string]api.AppService) ([]string, error) {
	if services == nil {
		return nil, nil
	}

	// Build adjacency list dep -> dependents, and indegree per node.
	dependents := make(map[string][]string, len(services))
	indegree := make(map[string]int, len(services))

	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
		indegree[name] = 0
	}
	sort.Strings(names)

	for svcName, svc := range services {
		seenDeps := make(map[string]struct{}, len(svc.After))
		for _, raw := range svc.After {
			dep := strings.TrimSpace(raw)
			if dep == "" {
				continue
			}
			if dep == svcName {
				return nil, fmt.Errorf("services.%s.after cannot reference itself", svcName)
			}
			if _, ok := services[dep]; !ok {
				return nil, fmt.Errorf("services.%s.after references unknown service '%s'", svcName, dep)
			}
			if _, ok := seenDeps[dep]; ok {
				continue
			}
			seenDeps[dep] = struct{}{}

			dependents[dep] = append(dependents[dep], svcName)
			indegree[svcName]++
		}
	}

	// Ensure deterministic iteration of dependents lists.
	for k := range dependents {
		sort.Strings(dependents[k])
	}

	// Kahn's algorithm with lexicographic selection of the next node.
	available := make([]string, 0, len(services))
	for _, name := range names {
		if indegree[name] == 0 {
			available = append(available, name)
		}
	}
	sort.Strings(available)

	order := make([]string, 0, len(services))
	for len(available) > 0 {
		cur := available[0]
		available = available[1:]
		order = append(order, cur)

		for _, child := range dependents[cur] {
			indegree[child]--
			if indegree[child] == 0 {
				available = append(available, child)
			}
		}
		sort.Strings(available)
	}

	if len(order) != len(services) {
		return nil, fmt.Errorf("services.after contains a dependency cycle")
	}

	return order, nil
}
