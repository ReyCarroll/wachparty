package main

import (
	"net"
	"testing"
)

func TestClassify(t *testing.T) {
	for _, c := range []struct{ name, ip, want string }{
		{"ethernet 2", "25.12.34.56", "hamachi"},
		{"hamachi", "5.1.2.3", "hamachi"},
		{"tailscale", "100.101.102.103", "tailscale"},
		{"wi-fi", "100.64.0.9", "tailscale"},
		{"wi-fi", "100.128.0.1", "lan"},
		{"zerotier one [abc]", "10.147.17.5", "zerotier"},
		{"ztabcdef", "172.28.1.1", "zerotier"},
		{"wi-fi", "192.168.1.20", "lan"},
		{"wi-fi", "169.254.3.4", ""},
		{"vethernet (wsl)", "172.24.80.1", "virtual"},
		{"vethernet (default switch)", "172.19.16.1", "virtual"},
		{"virtualbox host-only network", "192.168.56.1", "virtual"},
		{"ethernet", "192.168.1.5", "lan"},
	} {
		if got := classify(c.name, net.ParseIP(c.ip)); got != c.want {
			t.Errorf("%s %s: got %q want %q", c.name, c.ip, got, c.want)
		}
	}
}
