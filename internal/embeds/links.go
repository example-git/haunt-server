package embeds

import (
	"net/url"
	"regexp"
	"strings"
)

const MaxLinks = 3

var linkPattern = regexp.MustCompile(`(?i)https?://[^\s<>"\x60]+`)
var youtubeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{11}$`)
var tiktokPostPath = regexp.MustCompile(`^/@[a-zA-Z0-9_.]+/video/[0-9]{1,25}/?$`)
var tiktokShortPath = regexp.MustCompile(`^/(?:t/)?[a-zA-Z0-9]+/?$`)

// Links extracts a small, stable list. Backtick code and <angle-wrapped links>
// do not generate previews; punctuation outside a URL is not fetched.
func Links(body string) []string {
	body = hideCode(body)
	result := []string{}
	seen := map[string]bool{}
	for _, span := range linkPattern.FindAllStringIndex(body, -1) {
		raw := trimLink(body[span[0]:span[1]])
		if span[0] > 0 && body[span[0]-1] == '<' && span[0]+len(raw) < len(body) && body[span[0]+len(raw)] == '>' {
			continue
		}
		u, err := ParseURL(raw)
		if err != nil {
			continue
		}
		key := u.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, key)
		if len(result) == MaxLinks {
			break
		}
	}
	return result
}
func trimLink(raw string) string {
	raw = strings.TrimRight(raw, ".,!?:;'")
	for _, pair := range [][2]string{{"(", ")"}, {"[", "]"}, {"{", "}"}} {
		for strings.HasSuffix(raw, pair[1]) && strings.Count(raw, pair[1]) > strings.Count(raw, pair[0]) {
			raw = strings.TrimSuffix(raw, pair[1])
		}
	}
	return raw
}
func hideCode(body string) string {
	data := []byte(body)
	for start := 0; start < len(data); {
		if data[start] != '`' {
			start++
			continue
		}
		end := start
		for end < len(data) && data[end] == '`' {
			end++
		}
		delimiter := string(data[start:end])
		offset := strings.Index(string(data[end:]), delimiter)
		if offset < 0 {
			start = end
			continue
		}
		finish := end + offset + len(delimiter)
		for i := start; i < finish; i++ {
			data[i] = ' '
		}
		start = finish
	}
	return string(data)
}
func YouTube(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	id := ""
	switch host {
	case "youtu.be":
		id = strings.TrimPrefix(u.Path, "/")
	case "youtube.com", "www.youtube.com", "m.youtube.com", "www.youtube-nocookie.com":
		if u.Path == "/watch" {
			id = u.Query().Get("v")
		} else {
			parts := strings.Split(strings.Trim(u.Path, "/"), "/")
			if len(parts) == 2 && (parts[0] == "shorts" || parts[0] == "live" || parts[0] == "embed") {
				id = parts[1]
			}
		}
	}
	if youtubeID.MatchString(id) {
		return id
	}
	return ""
}

func ClientPost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Port() != "" {
		return false
	}
	switch strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.") {
	case "bsky.app":
		return blueskyPath.MatchString(u.Path)
	case "tiktok.com", "m.tiktok.com":
		return tiktokPostPath.MatchString(u.Path) || (strings.HasPrefix(u.Path, "/t/") && tiktokShortPath.MatchString(u.Path))
	case "vm.tiktok.com", "vt.tiktok.com":
		return !strings.HasPrefix(u.Path, "/t/") && tiktokShortPath.MatchString(u.Path)
	}
	return false
}
