package embeds

import (
	"net/url"
	"regexp"
	"strings"
)

var vimeoPath = regexp.MustCompile(`^/(?:video/|channels/[^/]+/|groups/[^/]+/videos/)?([0-9]{1,15})(?:/([a-zA-Z0-9]{6,64}))?/?$`)
var streamablePath = regexp.MustCompile(`^/(?:e/)?([a-z0-9]{1,64})/?$`)
var twitchVideoPath = regexp.MustCompile(`^/videos/([0-9]{1,20})/?$`)
var twitchChannelPath = regexp.MustCompile(`^/([a-zA-Z0-9_]{1,25})/?$`)
var twitchClipPath = regexp.MustCompile(`^/(?:[a-zA-Z0-9_]{1,25}/clip/)?([a-zA-Z0-9_-]{1,100})/?$`)
var playerToken = regexp.MustCompile(`^[a-zA-Z0-9]{6,64}$`)

func playerPreview(u *url.URL) Preview {
	p := Preview{URL: u.String(), Kind: "link"}
	if id := YouTube(u.String()); id != "" {
		p.YouTubeID, p.SiteName = id, "YouTube"
	} else {
		switch strings.TrimPrefix(u.Hostname(), "www.") {
		case "vimeo.com", "player.vimeo.com":
			if match := vimeoPath.FindStringSubmatch(u.Path); match != nil {
				hash := first(u.Query().Get("h"), match[2])
				if hash == "" || playerToken.MatchString(hash) {
					p.Player, p.PlayerID, p.PlayerToken, p.SiteName = "vimeo", match[1], hash, "Vimeo"
				}
			}
		case "streamable.com":
			if match := streamablePath.FindStringSubmatch(u.Path); match != nil {
				p.Player, p.PlayerID, p.SiteName = "streamable", match[1], "Streamable"
			}
		case "twitch.tv", "m.twitch.tv":
			if match := twitchVideoPath.FindStringSubmatch(u.Path); match != nil {
				p.Player, p.PlayerID, p.SiteName = "twitch-video", match[1], "Twitch"
			} else if strings.Contains(u.Path, "/clip/") {
				if match := twitchClipPath.FindStringSubmatch(u.Path); match != nil {
					p.Player, p.PlayerID, p.SiteName = "twitch-clip", match[1], "Twitch"
				}
			} else if match := twitchChannelPath.FindStringSubmatch(u.Path); match != nil {
				switch strings.ToLower(match[1]) {
				case "directory", "downloads", "settings", "subscriptions", "inventory", "wallet", "search", "videos":
				default:
					p.Player, p.PlayerID, p.SiteName = "twitch-channel", match[1], "Twitch"
				}
			}
		case "clips.twitch.tv":
			if match := twitchClipPath.FindStringSubmatch(u.Path); match != nil && match[1] != "embed" {
				p.Player, p.PlayerID, p.SiteName = "twitch-clip", match[1], "Twitch"
			}
		}
	}
	if p.YouTubeID != "" || p.Player != "" {
		p.Kind, p.Title = "video", p.SiteName+" video"
	}
	return p
}
