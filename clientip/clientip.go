// Package clientip resolves the real client IP address of an incoming HTTP request behind one
// or more trusted proxies (load balancers, ingress controllers, NATs), by walking the
// X-Forwarded-For chain and stopping at the first untrusted hop.
//
// Ported from ometria.js_tracker_pipeline's server/track.go (getIPAddress/ipIsTrusted), which has
// run this exact logic in production on every tracked page view for years. Extracted here so a
// second service needing the same trust decision (e.g. ometria.api.dynamic_content, for
// per-visitor geo lookups) doesn't need to duplicate deliberately careful, security-sensitive code
// a second time.
package clientip

import (
	"net"
	"net/http"
	"strings"
)

// IsTrusted reports whether ip falls within any of the given trusted CIDR blocks. An empty list
// of trusted blocks means every IP is trusted (no proxy trust boundary configured) — matching the
// upstream behaviour this package was ported from.
func IsTrusted(ip net.IP, trustedMasks []*net.IPNet) bool {
	if len(trustedMasks) == 0 {
		return true
	}
	for _, mask := range trustedMasks {
		if mask.Contains(ip) {
			return true
		}
	}
	return false
}

// GetIPAddress extracts the real client IP from an incoming request, given the set of CIDR
// blocks trusted to supply an accurate X-Forwarded-For header (typically the internal ingress/VPC
// range in front of the service). It walks the X-Forwarded-For chain from the end (each proxy
// appends its own hop) until it hits an entry outside the trusted range, and returns that as the
// real client IP. Falls back to the request's RemoteAddr when there's no X-Forwarded-For header,
// or when the immediate peer (RemoteAddr) itself isn't trusted.
//
// Returns the resolved IP and the remaining (unconsumed) forwarded-for chain, which the caller may
// want to log or pass on.
func GetIPAddress(r *http.Request, trustedMasks []*net.IPNet) (ip string, forwarded string) {
	ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	forwarded = r.Header.Get("X-Forwarded-For")
	if forwarded == "" {
		return
	}

	trustChainBroken := false
	remoteTrusted := true
	if !IsTrusted(net.ParseIP(ip), trustedMasks) {
		trustChainBroken = true
		remoteTrusted = false
	}

	ips := strings.Split(forwarded, ",")
	var validIPs []string
	var lastTrustedIP string

	// Iterate backwards because each proxy is appended to the header.
	for i := len(ips) - 1; i >= 0; i-- {
		fullIP := strings.TrimSpace(ips[i])
		// These may or may not include the port number. Make sure that they do not.
		ipStr, _, err := net.SplitHostPort(fullIP)
		if addrErr, ok := err.(*net.AddrError); err != nil && ok && addrErr.Err != "missing port in address" && addrErr.Err != "too many colons in address" {
			break
		}
		if ipStr == "" { // There was no host:port split returned
			ipStr = fullIP
		}

		parsed := net.ParseIP(ipStr)
		if parsed == nil {
			// Invalid IP - abort
			break
		}
		// IP is valid

		// Is trusted IP?
		if !trustChainBroken && IsTrusted(parsed, trustedMasks) {
			lastTrustedIP = ipStr
		} else {
			// Exclude some common private or reserved network spaces.
			if !strings.HasPrefix(ipStr, "10.") && !strings.HasPrefix(ipStr, "192.168") && !parsed.IsLoopback() {
				validIPs = append([]string{ipStr}, validIPs...)
			}
			trustChainBroken = true
		}
	}

	if remoteTrusted {
		if len(validIPs) > 0 {
			// Use first untrusted IP.
			ip = validIPs[len(validIPs)-1]
			validIPs = validIPs[:len(validIPs)-1]
		} else if lastTrustedIP != "" {
			// Request came from a trusted IP.
			ip = lastTrustedIP
		}
	}
	// Fall back to http.RemoteAddr.
	forwarded = strings.Join(validIPs, ",")
	return
}
