package redact

import (
	"strings"
)

// ── semantic validators (ported from silencer engine.cpp) ────────────────

// semanticValid decides whether a candidate match is a real sensitive value.
// Boundary checks use the whole-match span; value checks use the captured text.
func semanticValid(label, text string, matchBegin, matchEnd int, value string) bool {
	switch label {
	case "IP_PRIVATE", "IP_INTERNAL", "IP_PUBLIC":
		return ipv4CandidateOK(text, matchBegin, matchEnd, value, label)
	case "CARD":
		if adjacentAlpha(text, matchBegin, matchEnd) || dotNeighbor(text, matchBegin, matchEnd) {
			return false
		}
		return luhnValid(value)
	case "IDCARD":
		if adjacentAlpha(text, matchBegin, matchEnd) {
			return false
		}
		return validIDCard(value)
	case "PHONE":
		if adjacentAlpha(text, matchBegin, matchEnd) {
			return false
		}
		return validPhone(value)
	case "EMAIL":
		return emailCandidateOK(text, matchBegin, matchEnd, value)
	case "IBAN":
		return validIBAN(value)
	case "JWT":
		return validJWT(value)
	case "IPV6_PRIVATE":
		return validIPv6Private(value)
	case "USCC":
		return validUSCC(value)
	case "PLATE":
		if adjacentAlpha(text, matchBegin, matchEnd) || dotNeighbor(text, matchBegin, matchEnd) {
			return false
		}
		return validPlate(value)
	case "LANDLINE":
		if adjacentAlpha(text, matchBegin, matchEnd) {
			return false
		}
		return validLandline(value)
	default:
		return true
	}
}

func adjacentAlpha(text string, begin, end int) bool {
	if begin > 0 && isAlnum(text[begin-1]) {
		return true
	}
	return end < len(text) && isAlnum(text[end])
}

func dotNeighbor(text string, begin, end int) bool {
	return (begin > 0 && text[begin-1] == '.') || (end < len(text) && text[end] == '.')
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// validIPv4 parses strict dotted-quad addresses.
func validIPv4(value string) (octets [4]int, ok bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 {
		return octets, false
	}
	for i, part := range parts {
		if len(part) == 0 || len(part) > 3 || (len(part) > 1 && part[0] == '0') {
			return octets, false
		}
		n := 0
		for _, c := range part {
			if c < '0' || c > '9' {
				return octets, false
			}
			n = n*10 + int(c-'0')
		}
		if n > 255 {
			return octets, false
		}
		octets[i] = n
	}
	return octets, true
}

func ipv4CandidateOK(text string, matchBegin, matchEnd int, value, label string) bool {
	ip, ok := validIPv4(value)
	if !ok {
		return false
	}
	if adjacentAlpha(text, matchBegin, matchEnd) || dotNeighbor(text, matchBegin, matchEnd) {
		return false
	}
	private192 := ip[0] == 192 && ip[1] == 168
	linkLocal := ip[0] == 169 && ip[1] == 254
	cgnat := ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127
	private10 := ip[0] == 10
	private172 := ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31
	switch label {
	case "IP_PRIVATE":
		return private192 || linkLocal || cgnat
	case "IP_INTERNAL":
		return private10 || private172
	}
	// IP_PUBLIC: exclude known DNS and reserved ranges.
	for _, known := range []string{"8.8.8.8", "8.8.4.4", "1.1.1.1", "1.0.0.1", "4.2.2.1", "4.2.2.2", "4.2.2.3",
		"114.114.114.114", "114.114.115.115", "223.5.5.5", "223.6.6.6", "119.29.29.29", "180.76.76.76",
		"9.9.9.9", "149.112.112.112", "208.67.222.222", "208.67.220.220"} {
		if value == known {
			return false
		}
	}
	dots, allSingle := 0, true
	partLen := 0
	for i := 0; i < len(value); i++ {
		if value[i] == '.' {
			dots++
			allSingle = allSingle && partLen == 1
			partLen = 0
		} else {
			partLen++
		}
	}
	allSingle = allSingle && partLen == 1
	if dots == 3 && allSingle {
		return false
	}
	d1 := strings.IndexByte(value, '.')
	d2 := -1
	if d1 >= 0 {
		d2 = strings.IndexByte(value[d1+1:], '.') + d1 + 1
	}
	d3 := -1
	if d2 >= 0 {
		d3 = strings.IndexByte(value[d2+1:], '.') + d2 + 1
	}
	if d1 == 1 && d2 >= 0 && d3 >= 0 && ip[2] == 0 && len(value)-d3-1 == 3 {
		return false
	}
	reserved := ip[0] == 0 || private10 || ip[0] == 127 || private192 || linkLocal || cgnat ||
		private172 || ip[0] >= 224 || (ip[0] == 100 && ip[1] < 64) ||
		(ip[0] == 192 && (ip[1] == 0 || ip[1] == 168)) ||
		(ip[0] == 192 && ip[1] == 88 && ip[2] == 99) ||
		(ip[0] == 198 && (ip[1] == 18 || ip[1] == 19 || ip[1] == 51)) ||
		(ip[0] == 203 && ip[1] == 0) || ip[0] == 240
	return !reserved
}
