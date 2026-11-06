package server

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (s *serverAuth) isHTTPS(r *http.Request) bool {
	return requestIsHTTPS(r, s.trustedProxyCIDRs)
}

func (s *serverAuth) sameOrigin(r *http.Request) bool {
	return sameOriginWithScheme(r, s.isHTTPS(r))
}

func requestIsHTTPS(r *http.Request, trustedProxyCIDRs string) bool {
	if r.TLS != nil {
		return true
	}
	peer := net.ParseIP(remoteIP(r.RemoteAddr, "", ""))
	if peer == nil || !containsIP(trustedProxyNetworks(trustedProxyCIDRs), peer) {
		return false
	}
	forwardedProto := r.Header.Values("X-Forwarded-Proto")
	return len(forwardedProto) == 1 && strings.EqualFold(strings.TrimSpace(forwardedProto[0]), "https")
}

func sameOrigin(r *http.Request) bool {
	return sameOriginWithScheme(r, requestIsHTTPS(r, ""))
}

func sameOriginWithScheme(r *http.Request, isHTTPS bool) bool {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 {
		return false
	}
	origin, err := url.Parse(origins[0])
	if err != nil || origin.Opaque != "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return false
	}
	scheme := "http"
	if isHTTPS {
		scheme = "https"
	}
	if !strings.EqualFold(origin.Scheme, scheme) {
		return false
	}
	originAuthority, ok := canonicalAuthority(origin.Host, scheme)
	if !ok {
		return false
	}
	requestAuthority, ok := canonicalAuthority(r.Host, scheme)
	return ok && originAuthority == requestAuthority
}

func canonicalAuthority(host, scheme string) (string, bool) {
	parsed, err := url.Parse("//" + host)
	if err != nil || parsed.Host == "" || parsed.Path != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", false
	}
	name := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if name == "" {
		return "", false
	}
	port := parsed.Port()
	if port == "" {
		if strings.Contains(parsed.Host, ":") && net.ParseIP(name) == nil {
			return "", false
		}
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return "", false
	}
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		return name, true
	}
	return net.JoinHostPort(name, port), true
}

func remoteIP(remoteAddr, forwardedFor, trustedProxyCIDRs string) string {
	peer := remoteAddr
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		peer = host
	}
	peerIP := net.ParseIP(peer)
	if peerIP == nil {
		return peer
	}
	networks := trustedProxyNetworks(trustedProxyCIDRs)
	if !containsIP(networks, peerIP) || strings.TrimSpace(forwardedFor) == "" {
		return peerIP.String()
	}
	chain := strings.Split(forwardedFor, ",")
	chain = append(chain, peerIP.String())
	for index := len(chain) - 1; index >= 0; index-- {
		current := net.ParseIP(strings.TrimSpace(chain[index]))
		if current == nil {
			return peerIP.String()
		}
		if containsIP(networks, current) {
			continue
		}
		return current.String()
	}
	return peerIP.String()
}

func trustedProxyNetworks(raw string) []*net.IPNet {
	var networks []*net.IPNet
	for _, item := range strings.Split(raw, ",") {
		_, network, err := net.ParseCIDR(strings.TrimSpace(item))
		if err == nil && network != nil {
			networks = append(networks, network)
		}
	}
	return networks
}

func containsIP(networks []*net.IPNet, ip net.IP) bool {
	for _, network := range networks {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
