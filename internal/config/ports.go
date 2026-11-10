package config

import (
	"fmt"
	"strconv"
	"strings"
)

// Port forwards 127.0.0.1:Local on the client to 127.0.0.1:Remote in the
// session's network namespace.
type Port struct {
	Local  int `json:"local"`
	Remote int `json:"remote"`
}

func (p Port) String() string {
	if p.Local == p.Remote {
		return strconv.Itoa(p.Local)
	}
	return fmt.Sprintf("%d:%d", p.Local, p.Remote)
}

// ParsePorts parses a ports list of numbers and "local:remote" strings.
func ParsePorts(values []any) ([]Port, error) {
	var out []Port
	seen := map[int]bool{}
	for _, v := range values {
		var p Port
		var err error
		switch x := v.(type) {
		case int64:
			p, err = portPair(int(x), int(x))
		case int:
			p, err = portPair(x, x)
		case float64:
			if x != float64(int(x)) {
				return nil, fmt.Errorf("%v: not a port number", x)
			}
			p, err = portPair(int(x), int(x))
		case string:
			p, err = ParsePort(x)
		default:
			return nil, fmt.Errorf("%v: want a port number or \"local:remote\"", v)
		}
		if err != nil {
			return nil, err
		}
		if seen[p.Local] {
			return nil, fmt.Errorf("local port %d forwarded twice", p.Local)
		}
		seen[p.Local] = true
		out = append(out, p)
	}
	return out, nil
}

// ParsePortsEnv parses a comma separated list such as "8080,3000:3001".
func ParsePortsEnv(s string) ([]Port, error) {
	var vals []any
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			vals = append(vals, f)
		}
	}
	return ParsePorts(vals)
}

// ParsePort parses "8080" or "3000:3001" (local:remote).
func ParsePort(s string) (Port, error) {
	l, r, pair := strings.Cut(strings.TrimSpace(s), ":")
	ln, err := strconv.Atoi(l)
	if err != nil {
		return Port{}, fmt.Errorf("%q: want a port number or \"local:remote\"", s)
	}
	rn := ln
	if pair {
		if rn, err = strconv.Atoi(r); err != nil {
			return Port{}, fmt.Errorf("%q: want a port number or \"local:remote\"", s)
		}
	}
	return portPair(ln, rn)
}

func portPair(l, r int) (Port, error) {
	for _, n := range []int{l, r} {
		if n < 1 || n > 65535 {
			return Port{}, fmt.Errorf("port %d out of range 1-65535", n)
		}
	}
	return Port{Local: l, Remote: r}, nil
}
