package config

import (
	"encoding/hex"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// MediaTransport is a host decision. Clients consume it from call admission;
// they do not present transport choices to users.
func (c Config) MediaTransport() string {
	if c.Cloudflared && !c.HasExternalTURN() {
		return "websocket"
	}
	return "webrtc"
}

func (c Config) HasExternalTURN() bool {
	if c.CloudflareTURNKeyID != "" {
		return true
	}
	for _, server := range c.ICE {
		for _, address := range server.URLs {
			if strings.HasPrefix(address, "turn:") || strings.HasPrefix(address, "turns:") {
				return true
			}
		}
	}
	return false
}

func (e source) loadExternalMedia(c *Config) error {
	key, hasKey := e.lookup("HAUNT_CLOUDFLARE_TURN_KEY_ID")
	token, hasToken := e.lookup("HAUNT_CLOUDFLARE_TURN_API_TOKEN")
	if hasKey || hasToken {
		decoded, err := hex.DecodeString(key)
		if !hasKey || !hasToken || err != nil || len(decoded) != 16 || len(token) < 1 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
			return fmt.Errorf("Cloudflare TURN requires a 32-character key ID and a non-empty API token")
		}
		c.CloudflareTURNKeyID, c.CloudflareTURNToken = key, token
		c.MediaRelayOnly = true
	}
	if raw, exists := e.lookup("HAUNT_CLOUDFLARED_ENABLED"); exists {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("HAUNT_CLOUDFLARED_ENABLED must be true or false")
		}
		c.Cloudflared = value
	}
	host, present := e.lookup("HAUNT_MEDIA_TURN_HOST")
	if !present {
		for _, key := range []string{"HAUNT_MEDIA_TURN_PORT", "HAUNT_MEDIA_TURN_TLS_PORT", "HAUNT_MEDIA_TURN_USERNAME", "HAUNT_MEDIA_TURN_PASSWORD"} {
			if _, exists := e.lookup(key); exists {
				return fmt.Errorf("%s requires HAUNT_MEDIA_TURN_HOST", key)
			}
		}
	} else {
		if hasKey {
			return fmt.Errorf("configure either Cloudflare TURN or HAUNT_MEDIA_TURN_HOST")
		}
		if !mediaHost(host) {
			return fmt.Errorf("HAUNT_MEDIA_TURN_HOST requires an IP or DNS hostname without a scheme or port")
		}
		username, err := e.required("HAUNT_MEDIA_TURN_USERNAME")
		if err != nil {
			return err
		}
		password, err := e.required("HAUNT_MEDIA_TURN_PASSWORD")
		if err != nil {
			return err
		}
		if len(username) > 512 || len(password) > 512 {
			return fmt.Errorf("media TURN credentials must be at most 512 bytes")
		}
		port, err := e.number("HAUNT_MEDIA_TURN_PORT", 3478, 1, 65535)
		if err != nil {
			return err
		}
		tlsPort, err := e.number("HAUNT_MEDIA_TURN_TLS_PORT", 0, 0, 65535)
		if err != nil {
			return err
		}
		address := net.JoinHostPort(host, strconv.FormatInt(port, 10))
		urls := []string{"turn:" + address + "?transport=udp", "turn:" + address + "?transport=tcp"}
		if tlsPort != 0 {
			urls = append(urls, "turns:"+net.JoinHostPort(host, strconv.FormatInt(tlsPort, 10))+"?transport=tcp")
		}
		if len(c.ICE) >= 8 {
			return fmt.Errorf("media configuration supports at most eight ICE servers")
		}
		c.ICE = append(c.ICE, ICEServer{URLs: urls, Username: username, Credential: password})
		c.MediaRelayOnly = true
	}
	if c.Cloudflared && c.HasExternalTURN() {
		c.MediaRelayOnly = true
	}
	return nil
}

func mediaHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast()
	}
	if len(host) < 1 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}
