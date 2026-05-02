package main

import "testing"

func TestPprofAddrIsLoopback(t *testing.T) {
	cases := []struct {
		addr   string
		wantOK bool
	}{
		{"127.0.0.1:6060", true},
		{"[::1]:6060", true},
		{"localhost:6060", true},

		{":6060", false},        // empty host = all interfaces
		{"0.0.0.0:6060", false}, // any IPv4
		{"[::]:6060", false},    // any IPv6
		{"8.8.8.8:6060", false}, // arbitrary public IP
		{"6060", false},         // missing colon = invalid host:port
		{"", false},             // empty
	}
	for _, c := range cases {
		t.Run(c.addr, func(t *testing.T) {
			got, reason := pprofAddrIsLoopback(c.addr)
			if got != c.wantOK {
				t.Errorf("pprofAddrIsLoopback(%q) = %v (%s), want %v", c.addr, got, reason, c.wantOK)
			}
			if !got && reason == "" {
				t.Errorf("pprofAddrIsLoopback(%q) returned ok=false but no reason", c.addr)
			}
		})
	}
}
