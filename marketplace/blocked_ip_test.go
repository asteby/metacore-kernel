package marketplace

import (
	"net"
	"testing"
)

func TestBlockedIP(t *testing.T) {
	cases := []struct {
		ip        string
		loopback  bool
		wantBlock bool
	}{
		{"8.8.8.8", false, false},
		{"2606:4700::1111", false, false},
		{"127.0.0.1", false, true},
		{"127.0.0.1", true, false},
		{"::1", false, true},
		{"::1", true, false},
		{"10.0.0.1", true, true},
		{"172.16.5.4", true, true},
		{"192.168.0.1", true, true},
		{"169.254.169.254", true, true},
		{"fd00:ec2::254", true, true},
		{"fe80::1", true, true},
		{"100.64.0.1", true, true},
		{"0.0.0.0", true, true},
		{"::ffff:10.0.0.1", true, true},
		{"::ffff:127.0.0.1", false, true},
		{"224.0.0.1", true, true},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("bad ip %s", c.ip)
		}
		if got := blockedIP(ip, c.loopback); got != c.wantBlock {
			t.Errorf("blockedIP(%s, loopback=%v)=%v want %v", c.ip, c.loopback, got, c.wantBlock)
		}
	}
}
