package clientip

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

// Ported verbatim from ometria.js_tracker_pipeline's server/track_test.go (TestGetIPAddress), the
// existing, production-proven case set for this exact logic.
func TestGetIPAddress(t *testing.T) {
	var tests = []struct {
		name              string
		remote            string
		header            string
		mask              string
		expectedIP        string
		expectedForwarded string
	}{
		{
			"noForwarded",
			"1.1.1.1",
			"",
			"4.5.6.7/32",
			"1.1.1.1",
			"",
		},
		{
			"forwardedTwo",
			"1.1.1.1",
			"2.2.2.2,3.3.3.3",
			"4.5.6.7/32",
			"1.1.1.1",
			"2.2.2.2,3.3.3.3",
		},
		{
			"forwardedMixed",
			"1.1.1.1",
			"2.2.2.2,2a00:1450:4009:815::200e",
			"4.5.6.7/32",
			"1.1.1.1",
			"2.2.2.2,2a00:1450:4009:815::200e",
		},
		{
			"forwardedInvalid",
			"1.1.1.1",
			"2.2.2.2,333.3.3.3,4.4.4.4",
			"4.5.6.7/32",
			"1.1.1.1",
			"4.4.4.4",
		},
		{
			"forwardedInvalid2",
			"1.1.1.1",
			"2.2.2.2,333.3.3.3,4.4.4.4",
			"1.1.1.1/8",
			"4.4.4.4",
			"",
		},
		{
			"forwardedTrustedDirect",
			"1.1.1.1",
			"2.2.2.2,3.3.3.3,1.2.3.4",
			"1.1.1.1/8",
			"3.3.3.3",
			"2.2.2.2",
		},
		{
			"trustedDirect",
			"1.1.1.1",
			"1.2.3.4",
			"1.1.1.1/8",
			"1.2.3.4",
			"",
		},
		{
			"forwardedTrusted",
			"1.1.1.1",
			"2.2.2.2,3.3.3.3,4.4.4.4",
			"1.1.1.1/24",
			"4.4.4.4",
			"2.2.2.2,3.3.3.3",
		},
		{
			"forwardedMixedTrusted",
			"1.1.1.1",
			"2.2.2.2,2a00:1450:4009:815::200e",
			"1.0.0.0/8",
			"2a00:1450:4009:815::200e",
			"2.2.2.2",
		},
		{
			"forwardedTrustedInMiddle",
			"1.1.1.1",
			"2.2.2.2,1.2.2.2,4.4.4.4",
			"1.1.1.1/8",
			"4.4.4.4",
			"2.2.2.2,1.2.2.2",
		},
		{
			"forwardedTrustedAtEnd",
			"1.1.1.1",
			"2.2.2.2,3.3.3.3,1.2.2.2",
			"1.1.1.1/8",
			"3.3.3.3",
			"2.2.2.2",
		},
		{
			"forwardedPrivate",
			"1.1.1.1",
			"2.2.2.2,192.168.1.1",
			"1.1.1.1/8",
			"2.2.2.2",
			"",
		},
		{
			"forwardedWithPort",
			"1.1.1.1",
			"2.2.2.2:8080",
			"1.1.1.1/8",
			"2.2.2.2",
			"",
		},
		{
			"forwardedJunk",
			"1.1.1.1",
			"3adf3",
			"1.1.1.1/8",
			"1.1.1.1",
			"",
		},
		{
			"forwardedIPv6",
			"[2804:214:81f4:41f6:114e:11d0:d23d:ac2a]",
			"",
			"1.1.1.1/8",
			"2804:214:81f4:41f6:114e:11d0:d23d:ac2a",
			"",
		},
		// The X-Forwarded-For values from ELB/Traefik contain whitespace between the IPs.
		{
			"forwardedTrustedInMiddleWhitespaceInHeader",
			"1.1.1.1",
			"2.2.2.2, 1.2.2.2, 4.4.4.4",
			"1.1.1.1/8",
			"4.4.4.4",
			"2.2.2.2,1.2.2.2",
		},
		// There can be multiple CIDRs in X-Forwarded-For that are considered
		// trusted, eg. the kube subnet and also the public IPs used for the
		// NATs in each availability zone.
		{
			"multipleTrustedMasks",
			"10.2.8.142",
			"86.136.123.123, 34.246.102.231, 10.2.10.0",
			"10.0.0.0/8, 34.246.102.0/24",
			"86.136.123.123",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", "/", nil)
			req.RemoteAddr = fmt.Sprintf("%s:%d", tt.remote, 8080)
			req.Header.Add("X-Forwarded-For", tt.header)

			var masks []*net.IPNet
			maskStrs := strings.Split(tt.mask, ",")
			for _, maskStr := range maskStrs {
				_, mask, err := net.ParseCIDR(strings.TrimSpace(maskStr))
				if err != nil {
					t.Fatalf("failed to parse CIDR: %s", err)
				}
				masks = append(masks, mask)
			}

			ip, forwarded := GetIPAddress(req, masks)
			if ip != tt.expectedIP {
				t.Errorf("got IP %q, expected %q", ip, tt.expectedIP)
			}
			if forwarded != tt.expectedForwarded {
				t.Errorf("got X-Forwarded-For %q, expected %q", forwarded, tt.expectedForwarded)
			}
		})
	}
}

func TestIsTrusted(t *testing.T) {
	_, mask, err := net.ParseCIDR("10.0.0.0/8")
	if err != nil {
		t.Fatalf("failed to parse CIDR: %s", err)
	}
	masks := []*net.IPNet{mask}

	if !IsTrusted(net.ParseIP("10.2.8.142"), masks) {
		t.Error("expected 10.2.8.142 to be trusted under 10.0.0.0/8")
	}
	if IsTrusted(net.ParseIP("8.8.8.8"), masks) {
		t.Error("expected 8.8.8.8 not to be trusted under 10.0.0.0/8")
	}
	// No trusted masks configured at all means every IP is trusted.
	if !IsTrusted(net.ParseIP("8.8.8.8"), nil) {
		t.Error("expected every IP to be trusted when no trusted masks are configured")
	}
}
