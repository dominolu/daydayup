package redact

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"strings"
	"time"
)

// timeNow is overridable in tests; default returns the wall clock.
var timeNow = func() *time.Time {
	now := time.Now()
	return &now
}

func luhnValid(value string) bool {
	sum, double := 0, false
	digits := 0
	for i := len(value) - 1; i >= 0; i-- {
		c := value[i]
		if c == ' ' || c == '-' {
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
		d := int(c - '0')
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
		digits++
	}
	if digits < 13 || digits > 19 || len(value) == 0 || value[0] == '0' {
		return false
	}
	hasSpace := strings.Contains(value, " ")
	hasDash := strings.Contains(value, "-")
	if !hasSpace && !hasDash {
		return sum%10 == 0
	}
	if hasSpace && hasDash {
		return false
	}
	separator := byte(' ')
	if hasDash {
		separator = '-'
	}
	group, begin := 0, 0
	for begin <= len(value) {
		end := strings.IndexByte(value[begin:], separator)
		stop := len(value)
		if end >= 0 {
			stop = begin + end
		}
		length := stop - begin
		if length == 0 || length > 6 || (group == 0 && (length < 3 || length > 6)) {
			return false
		}
		group++
		if end < 0 {
			break
		}
		begin = stop + 1
	}
	return group >= 3 && group <= 5 && sum%10 == 0
}

func leapYear(year int) bool { return year%4 == 0 && (year%100 != 0 || year%400 == 0) }

func validIDCard(value string) bool {
	weights := [17]int{7, 9, 10, 5, 8, 4, 2, 1, 6, 3, 7, 9, 10, 5, 8, 4, 2}
	checks := "10X98765432"
	monthDays := [12]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
	if len(value) != 15 && len(value) != 18 {
		return false
	}
	province := value[:2]
	if !contains([]string{"11", "12", "13", "14", "15", "21", "22", "23", "31", "32", "33", "34", "35", "36", "37", "41", "42", "43", "44", "45", "46", "50", "51", "52", "53", "54", "61", "62", "63", "64", "65", "71", "81", "82"}, province) {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			if !(i == 17 && (value[i] == 'X' || value[i] == 'x')) {
				return false
			}
		}
	}
	year, month, day := 0, 0, 0
	if len(value) == 18 {
		year = atoi(value[6:10])
		month = atoi(value[10:12])
		day = atoi(value[12:14])
	} else {
		year = 1900 + atoi(value[6:8])
		month = atoi(value[8:10])
		day = atoi(value[10:12])
	}
	// Year bound uses UTC current year; keep parity with the C++ engine.
	currentYear := 9999
	if now := timeNow(); now != nil {
		currentYear = now.Year()
	}
	if year < 1880 || year > currentYear || month < 1 || month > 12 {
		return false
	}
	maxDay := monthDays[month-1]
	if month == 2 && leapYear(year) {
		maxDay++
	}
	if day < 1 || day > maxDay {
		return false
	}
	if len(value) == 15 {
		return true
	}
	sum := 0
	for i := 0; i < 17; i++ {
		sum += int(value[i]-'0') * weights[i]
	}
	check := value[17]
	if check >= 'a' && check <= 'z' {
		check -= 'a' - 'A'
	}
	return check == checks[sum%11]
}

func validPhone(value string) bool {
	normalized := strings.NewReplacer("（", "(", "）", ")").Replace(value)
	digits := strings.Builder{}
	groupSeparator := byte(0)
	for i := 0; i < len(normalized); i++ {
		c := normalized[i]
		switch {
		case c >= '0' && c <= '9':
			digits.WriteByte(c)
		case c == ' ' || c == '-':
			if groupSeparator != 0 && groupSeparator != c {
				return false
			}
			groupSeparator = c
		case c != '+' && c != '(' && c != ')':
			return false
		}
	}
	d := digits.String()
	if strings.HasPrefix(d, "0086") {
		d = d[4:]
	} else if strings.HasPrefix(d, "86") && len(d) == 13 {
		d = d[2:]
	}
	if len(d) != 11 || d[0] != '1' || d[1] < '3' || d[1] > '9' {
		return false
	}
	for i := 1; i < len(d); i++ {
		if d[i] != d[0] {
			return true
		}
	}
	return false
}

func emailCandidateOK(text string, matchBegin, matchEnd int, value string) bool {
	if matchBegin > 0 {
		prev := text[matchBegin-1]
		if isAlnum(prev) || prev >= 0x80 || prev == '.' || prev == '_' || prev == '%' || prev == '+' || prev == '-' || prev == ':' {
			return false
		}
	}
	scheme := strings.LastIndex(text[:matchBegin], "://")
	if scheme >= 0 {
		rest := text[scheme+3:]
		authorityEnd := len(text)
		if idx := strings.IndexAny(rest, "/?#"); idx >= 0 {
			authorityEnd = scheme + 3 + idx
		}
		if matchBegin < authorityEnd {
			colon := strings.IndexByte(text[scheme+3:authorityEnd], ':')
			if colon >= 0 && scheme+3+colon < matchBegin {
				return false
			}
		}
	}
	return validEmail(value)
}

func validEmail(value string) bool {
	marker := strings.IndexByte(value, '@')
	if marker <= 0 || marker+1 >= len(value) || strings.IndexByte(value[marker+1:], '@') >= 0 {
		return false
	}
	local, domain := value[:marker], value[marker+1:]
	if len(local) > 64 || len(domain) < 3 || local[0] == '.' || local[len(local)-1] == '.' ||
		strings.Contains(local, "..") || strings.Contains(domain, "..") {
		return false
	}
	dot := strings.LastIndexByte(domain, '.')
	if dot < 0 || len(domain)-dot-1 < 2 {
		return false
	}
	begin := 0
	for begin < len(domain) {
		separator := strings.IndexByte(domain[begin:], '.')
		end := len(domain)
		if separator >= 0 {
			end = begin + separator
		}
		if end == begin || end-begin > 63 || domain[begin] == '-' || domain[end-1] == '-' {
			return false
		}
		begin = end + 1
	}
	return true
}

func validIBAN(value string) bool {
	if len(value) < 15 || len(value) > 34 {
		return false
	}
	if value[0] < 'A' || value[0] > 'Z' || value[1] < 'A' || value[1] > 'Z' || value[2] < '0' || value[2] > '9' || value[3] < '0' || value[3] > '9' {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	remainder := 0
	consume := func(c byte) {
		if c >= '0' && c <= '9' {
			remainder = (remainder*10 + int(c-'0')) % 97
		} else {
			n := int(c-'A') + 10
			remainder = (remainder*10 + n/10) % 97
			remainder = (remainder*10 + n%10) % 97
		}
	}
	for i := 4; i < len(value); i++ {
		consume(value[i])
	}
	for i := 0; i < 4; i++ {
		consume(value[i])
	}
	return remainder == 1
}

func validJWT(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || len(parts[0]) == 0 || len(parts[1]) == 0 || len(parts[2]) == 0 {
		return false
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var parsed map[string]any
	if json.Unmarshal(header, &parsed) != nil {
		return false
	}
	alg, ok := parsed["alg"].(string)
	return ok && alg != ""
}

func validIPv6Private(value string) bool {
	address := value
	if idx := strings.IndexByte(value, '%'); idx >= 0 {
		address = value[:idx]
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.To16() == nil {
		return false
	}
	b := ip.To16()
	linkLocal := b[0] == 0xfe && b[1]&0xc0 == 0x80
	uniqueLocal := b[0]&0xfe == 0xfc
	return linkLocal || uniqueLocal
}

func validUSCC(value string) bool {
	alphabet := "0123456789ABCDEFGHJKLMNPQRTUWXY"
	weights := [17]int{1, 3, 9, 27, 19, 26, 16, 17, 20, 29, 25, 13, 8, 24, 10, 30, 28}
	if len(value) != 18 {
		return false
	}
	total := 0
	for i := 0; i < 17; i++ {
		pos := strings.IndexByte(alphabet, value[i])
		if pos < 0 {
			return false
		}
		total += weights[i] * pos
	}
	check := strings.IndexByte(alphabet, value[17])
	return check >= 0 && value[17] == alphabet[(31-total%31)%31]
}

func validPlate(value string) bool {
	provinces := []string{"京", "津", "沪", "渝", "冀", "豫", "云", "辽", "黑", "湘", "皖", "鲁", "新", "苏", "浙",
		"赣", "鄂", "桂", "甘", "晋", "蒙", "陕", "吉", "闽", "贵", "粤", "青", "藏", "川", "宁", "琼", "使", "领"}
	if len(value) < 9 || len(value) > 10 {
		return false
	}
	found := false
	for _, p := range provinces {
		if strings.HasPrefix(value, p) {
			found = true
			break
		}
	}
	// Provinces are multi-byte; index 3 is the letter after the 2-byte province.
	if !found || len(value) < 4 || value[3] < 'A' || value[3] > 'Z' {
		return false
	}
	body := value[4:]
	if len(body) < 5 || len(body) > 6 {
		return false
	}
	hasDigit := false
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c >= '0' && c <= '9' {
			hasDigit = true
		} else if c < 'A' || c > 'Z' {
			return false
		}
	}
	return hasDigit
}

func validLandline(value string) bool {
	var digits strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] >= '0' && value[i] <= '9' {
			digits.WriteByte(value[i])
		}
	}
	d := digits.String()
	if strings.HasPrefix(d, "0086") {
		d = d[4:]
	} else if strings.HasPrefix(d, "86") {
		d = d[2:]
	}
	if len(d) < 10 || len(d) > 17 || d[0] != '0' {
		return false
	}
	local := 0
	if len(d) >= 4 && (d[1] == '1' || d[1] == '2') {
		local = 3
	} else if len(d) >= 5 && d[1] >= '3' && d[1] <= '9' {
		local = 4
	} else {
		return false
	}
	return local < len(d) && d[local] >= '2' && d[local] <= '9'
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func atoi(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			break
		}
		n = n*10 + int(s[i]-'0')
	}
	return n
}
