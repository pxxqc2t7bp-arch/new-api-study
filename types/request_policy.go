package types

import "strings"

type RoutingStrategy string

const (
	RoutingStrategyEconomy RoutingStrategy = "economy"
	RoutingStrategyStable  RoutingStrategy = "stable"
	RoutingStrategyLatency RoutingStrategy = "latency"
)

func ParseRoutingStrategy(value string) (RoutingStrategy, bool) {
	strategy := RoutingStrategy(strings.TrimSpace(value))
	switch strategy {
	case RoutingStrategyEconomy, RoutingStrategyStable, RoutingStrategyLatency:
		return strategy, true
	default:
		return "", false
	}
}
