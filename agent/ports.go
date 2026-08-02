package main

import (
	"net"
	"sort"
	"strconv"
)

func occupiedLocalTCPPorts(host string, startPort, endPort int) []int {
	if host == "" || startPort <= 0 || endPort < startPort || !isLocalIPAddress(host) {
		return nil
	}
	occupied := []int{}
	for port := startPort; port <= endPort; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			occupied = append(occupied, port)
			continue
		}
		_ = listener.Close()
	}
	return occupied
}

func localTCPPortAvailable(host string, port int) bool {
	if host == "" || port <= 0 || port > 65535 || !isLocalIPAddress(host) {
		return false
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func isLocalIPAddress(host string) bool {
	target := net.ParseIP(host)
	if target == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch typed := addr.(type) {
		case *net.IPNet:
			ip = typed.IP
		case *net.IPAddr:
			ip = typed.IP
		}
		if ip != nil && ip.Equal(target) {
			return true
		}
	}
	return false
}

func mergeReservedPorts(primary []int, extra []int) []int {
	if len(primary) == 0 && len(extra) == 0 {
		return nil
	}
	merged := map[int]bool{}
	for _, port := range primary {
		if port > 0 && port <= 65535 {
			merged[port] = true
		}
	}
	for _, port := range extra {
		if port > 0 && port <= 65535 {
			merged[port] = true
		}
	}
	out := make([]int, 0, len(merged))
	for port := range merged {
		out = append(out, port)
	}
	sort.Ints(out)
	return out
}
