// Package app provides the realclientip-go strategy factory for Scorpion.
package app

import (
	"fmt"
	"net"

	"github.com/realclientip/realclientip-go"

	"github.com/blkst8/scorpion/internal/config"
)

// NewIPStrategy creates a realclientip strategy from configuration.
// This is called once at startup — the returned strategy is safe for concurrent use.
func NewIPStrategy(cfg config.IP) (realclientip.Strategy, error) {
	headerName := cfg.Header
	if headerName == "" {
		headerName = "X-Forwarded-For"
	}

	switch cfg.Strategy {
	case "rightmost_trusted_count":
		if cfg.TrustedCount <= 0 {
			return nil, fmt.Errorf("trusted_count must be > 0 for rightmost_trusted_count strategy")
		}
		return realclientip.NewRightmostTrustedCountStrategy(headerName, cfg.TrustedCount)

	case "rightmost_trusted_range":
		if len(cfg.TrustedRanges) == 0 {
			return nil, fmt.Errorf("trusted_ranges must not be empty for rightmost_trusted_range strategy")
		}
		ranges := make([]net.IPNet, 0, len(cfg.TrustedRanges))
		for _, cidr := range cfg.TrustedRanges {
			_, network, err := net.ParseCIDR(cidr)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted range %q: %w", cidr, err)
			}
			ranges = append(ranges, *network)
		}
		return realclientip.NewRightmostTrustedRangeStrategy(headerName, ranges)

	case "rightmost_non_private":
		return realclientip.NewRightmostNonPrivateStrategy(headerName)

	case "single_ip_header":
		return realclientip.NewSingleIPHeaderStrategy(headerName)

	case "remote_addr", "":
		return realclientip.RemoteAddrStrategy{}, nil

	default:
		return nil, fmt.Errorf("unknown IP extraction strategy: %q", cfg.Strategy)
	}
}
