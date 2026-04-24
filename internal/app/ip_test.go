package app

import (
	"net/http"
	"testing"

	"github.com/realclientip/realclientip-go"

	"github.com/blkst8/scorpion/internal/config"
)

func TestIPExtraction(t *testing.T) {
	strategy, err := realclientip.NewRightmostTrustedCountStrategy("X-Forwarded-For", 2)
	if err != nil {
		t.Fatalf("failed to create strategy: %v", err)
	}

	tests := []struct {
		name       string
		xff        string
		remoteAddr string
		wantIP     string
	}{
		{
			name:       "normal proxy chain",
			xff:        "1.2.3.4, 10.0.0.1",
			remoteAddr: "10.0.0.1:8080",
			wantIP:     "1.2.3.4",
		},
		{
			name:       "forged XFF",
			xff:        "6.6.6.6, 5.5.5.5, 10.0.0.1",
			remoteAddr: "10.0.0.1:8080",
			wantIP:     "5.5.5.5",
		},
		{
			name:       "no XFF header",
			xff:        "",
			remoteAddr: "1.2.3.4:8080",
			wantIP:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			if tt.xff != "" {
				h.Set("X-Forwarded-For", tt.xff)
			}
			gotIP := strategy.ClientIP(h, tt.remoteAddr)
			if gotIP != tt.wantIP {
				t.Errorf("got IP %q, want %q", gotIP, tt.wantIP)
			}
		})
	}
}

func TestNewIPStrategy(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.IP
		wantErr bool
	}{
		{
			name:    "remote_addr",
			cfg:     config.IP{Strategy: "remote_addr"},
			wantErr: false,
		},
		{
			name:    "rightmost_trusted_count valid",
			cfg:     config.IP{Strategy: "rightmost_trusted_count", TrustedCount: 1, Header: "X-Forwarded-For"},
			wantErr: false,
		},
		{
			name:    "rightmost_trusted_count zero count",
			cfg:     config.IP{Strategy: "rightmost_trusted_count", TrustedCount: 0},
			wantErr: true,
		},
		{
			name:    "rightmost_trusted_range valid",
			cfg:     config.IP{Strategy: "rightmost_trusted_range", TrustedRanges: []string{"10.0.0.0/8"}, Header: "X-Forwarded-For"},
			wantErr: false,
		},
		{
			name:    "rightmost_trusted_range empty",
			cfg:     config.IP{Strategy: "rightmost_trusted_range"},
			wantErr: true,
		},
		{
			name:    "unknown strategy",
			cfg:     config.IP{Strategy: "bogus"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewIPStrategy(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewIPStrategy() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
